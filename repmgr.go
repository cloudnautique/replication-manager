// replication-manager - Replication Manager Monitoring and CLI for MariaDB
// Author: Guillaume Lefranc <guillaume.lefranc@mariadb.com>
// License: GNU General Public License, version 3. Redistribution/Reuse of this code is permitted under the GNU v3 license, as an additional term ALL code must carry the original Author(s) credit in comment form.
// See LICENSE in this directory for the integral text.

package main

import (
	"flag"
	"fmt"
	"github.com/nsf/termbox-go"
	"github.com/tanji/mariadb-tools/dbhelper"
	"log"
	"strings"
	"time"
)

const repmgrVersion string = "0.5.0-dev"

var (
	hostList      []string
	servers       []*ServerMonitor
	slaves        []*ServerMonitor
	master        *ServerMonitor
	exit          bool
	vy            int
	dbUser        string
	dbPass        string
	rplUser       string
	rplPass       string
	switchOptions     = []string{"keep", "kill"}
	failOptions       = []string{"monitor", "force", "check"}
	failCount     int = 0
	tlog          TermLog
	ignoreList    []string
)

// Command specific options
var (
	version     = flag.Bool("version", false, "Return version")
	user        = flag.String("user", "", "User for MariaDB login, specified in the [user]:[password] format")
	hosts       = flag.String("hosts", "", "List of MariaDB hosts IP and port (optional), specified in the host:[port] format and separated by commas")
	socket      = flag.String("socket", "/var/run/mysqld/mysqld.sock", "Path of MariaDB unix socket")
	rpluser     = flag.String("rpluser", "", "Replication user in the [user]:[password] format")
	interactive = flag.Bool("interactive", true, "Ask for user interaction when failures are detected")
	verbose     = flag.Bool("verbose", false, "Print detailed execution info")
	preScript   = flag.String("pre-failover-script", "", "Path of pre-failover script")
	postScript  = flag.String("post-failover-script", "", "Path of post-failover script")
	maxDelay    = flag.Int64("maxdelay", 0, "Maximum replication delay before initiating failover")
	gtidCheck   = flag.Bool("gtidcheck", false, "Check that GTID sequence numbers are identical before initiating failover")
	prefMaster  = flag.String("prefmaster", "", "Preferred candidate server for master failover, in host:[port] format")
	ignoreSrv   = flag.String("ignore-servers", "", "List of servers to ignore in slave promotion operations")
	waitKill    = flag.Int64("wait-kill", 5000, "Wait this many milliseconds before killing threads on demoted master")
	readonly    = flag.Bool("readonly", true, "Set slaves as read-only after switchover")
	failover    = flag.String("failover", "", "Failover mode, either 'monitor', 'force' or 'check'")
	switchover  = flag.String("switchover", "", "Switchover mode, either 'keep' or 'kill' the old master.")
)

const (
	STATE_FAILED string = "Failed"
	STATE_MASTER string = "Master"
	STATE_SLAVE  string = "Slave"
	STATE_UNCONN string = "Unconnected"
)

func main() {
	flag.Parse()
	if *version == true {
		fmt.Println("MariaDB Replication Manager version", repmgrVersion)
	}
	// if slaves option has been supplied, split into a slice.
	if *hosts != "" {
		hostList = strings.Split(*hosts, ",")
	} else {
		log.Fatal("ERROR: No hosts list specified.")
	}
	// validate users.
	if *user == "" {
		log.Fatal("ERROR: No master user/pair specified.")
	}
	dbUser, dbPass = splitPair(*user)
	if *rpluser == "" {
		log.Fatal("ERROR: No replication user/pair specified.")
	}
	rplUser, rplPass = splitPair(*rpluser)

	// Check that failover and switchover modes are set correctly.
	if *switchover == "" && *failover == "" {
		log.Fatal("ERROR: None of the switchover or failover modes are set.")
	}
	if *switchover != "" && *failover != "" {
		log.Fatal("ERROR: Both switchover and failover modes are set.")
	}
	if !contains(failOptions, *failover) && *failover != "" {
		log.Fatalf("ERROR: Incorrect failover mode: %s", *failover)
	}
	if !contains(switchOptions, *switchover) && *switchover != "" {
		log.Fatalf("ERROR: Incorrect switchover mode: %s", *switchover)
	}

	if *ignoreSrv != "" {
		ignoreList = strings.Split(*ignoreSrv, ",")
	}

	// Create a connection to each host and build list of slaves.
	hostCount := len(hostList)
	servers = make([]*ServerMonitor, hostCount)
	slaveCount := 0
	for k, url := range hostList {
		var err error
		servers[k], err = newServerMonitor(url)
		if *verbose {
			log.Printf("DEBUG: Creating new server: %v", servers[k].URL)
		}
		if err != nil {
			log.Printf("INFO : Server %s is dead.", servers[k].URL)
			servers[k].State = STATE_FAILED
			continue
		}
		defer servers[k].Conn.Close()
		if *verbose {
			log.Printf("DEBUG: Checking if server %s is slave", servers[k].URL)
		}

		servers[k].refresh()
		if servers[k].UsingGtid != "" {
			if *verbose {
				log.Printf("DEBUG: Server %s is configured as a slave", servers[k].URL)
			}
			servers[k].State = STATE_SLAVE
			slaves = append(slaves, servers[k])
			slaveCount++
		} else {
			if *verbose {
				log.Printf("DEBUG: Server %s is not a slave. Setting aside", servers[k].URL)
			}
		}
	}

	// Check that all slave servers have the same master.
	for _, sl := range slaves {
		if sl.hasSiblings(slaves) == false {
			log.Fatalln("ERROR: Multi-master topologies are not yet supported.")
		}
	}

	// Depending if we are doing a failover or a switchover, we will find the master in the list of
	// dead hosts or unconnected hosts.
	if *switchover != "" || *failover == "monitor" {
		// First of all, get a server id from the slaves slice, they should be all the same
		sid := slaves[0].MasterServerId
		for k, s := range servers {
			if s.State == STATE_UNCONN {
				if s.ServerId == sid {
					master = servers[k]
					master.State = STATE_MASTER
					if *verbose {
						log.Printf("DEBUG: Server %s was autodetected as a master", s.URL)
					}
					break
				}
			}
		}
	} else {
		// Slave master_host variable must point to dead master
		smh := slaves[0].MasterHost
		for k, s := range servers {
			if s.State == STATE_FAILED {
				if s.Host == smh || s.IP == smh {
					master = servers[k]
					master.State = STATE_MASTER
					if *verbose {
						log.Printf("DEBUG: Server %s was autodetected as a master", s.URL)
					}
					break
				}
			}
		}
	}
	// Final check if master has been found
	if master == nil {
		log.Fatalln("ERROR: Could not autodetect a master!")
	}

	for _, sl := range slaves {
		if *verbose {
			log.Printf("DEBUG: Checking if server %s is a slave of server %s", sl.Host, master.Host)
		}
		if dbhelper.IsSlaveof(sl.Conn, sl.Host, master.IP) == false {
			log.Printf("WARN : Server %s is not a slave of declared master %s", master.URL, master.Host)
		}
	}

	// Check if preferred master is included in Host List
	ret := func() bool {
		for _, v := range hostList {
			if v == *prefMaster {
				return true
			}
		}
		return false
	}
	if ret() == false && *prefMaster != "" {
		log.Fatal("ERROR: Preferred master is not included in the hosts option")
	}

	// Do failover or switchover manually, or start the interactive monitor.

	if *failover == "force" {
		master.failover()
	} else if *switchover != "" && *interactive == false {
		master.switchover()
	} else {
	MainLoop:
		err := termbox.Init()
		if err != nil {
			log.Fatalln("Termbox initialization error", err)
		}
		tlog = NewTermLog(20)
		if *failover != "" {
			tlog.Add("Monitor started in failover mode")
		} else {
			tlog.Add("Monitor started in switchover mode")
		}
		termboxChan := new_tb_chan()
		interval := time.Second
		ticker := time.NewTicker(interval * 3)
		var command string
		for exit == false {
			select {
			case <-ticker.C:
				display()
			case event := <-termboxChan:
				switch event.Type {
				case termbox.EventKey:
					if event.Key == termbox.KeyCtrlS {
						nmUrl, nsKey := master.switchover()
						if nmUrl != "" && nsKey >= 0 {
							if *verbose {
								logprintf("DEBUG: Reinstancing new master: %s and new slave: %s [%d]", nmUrl, slaves[nsKey].URL, nsKey)
							}
							master, err = newServerMonitor(nmUrl)
							slaves[nsKey], err = newServerMonitor(slaves[nsKey].URL)
						}
					}
					if event.Key == termbox.KeyCtrlF {
						command = "failover"
						exit = true
					}
					if event.Key == termbox.KeyCtrlQ {
						exit = true
					}
				}
				switch event.Ch {
				case 's':
					termbox.Sync()
				}
			}
			if master.State == STATE_FAILED && *interactive == false {
				command = "failover"
				exit = true
			}
		}
		switch command {
		case "failover":
			termbox.Close()
			nmUrl, nmKey := master.failover()
			if nmUrl != "" {
				if *verbose {
					log.Printf("DEBUG: Reinstancing new master: %s", nmUrl)
				}
				master, err = newServerMonitor(nmUrl)
				// Remove new master from slave slice
				slaves = append(slaves[:nmKey], slaves[nmKey+1:]...)
			}
			log.Println("###### Restarting monitor console in 5 seconds. Press Ctrl-C to exit")
			time.Sleep(5 * time.Second)
			exit = false
			goto MainLoop
		}
		termbox.Close()
	}
}

func new_tb_chan() chan termbox.Event {
	termboxChan := make(chan termbox.Event)
	go func() {
		for {
			termboxChan <- termbox.PollEvent()
		}
	}()
	return termboxChan
}
