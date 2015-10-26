package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net"
	"os"
	"os/exec"
	"path"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mholt/caddy/app"
	"github.com/mholt/caddy/config"
	"github.com/mholt/caddy/config/letsencrypt"
	"github.com/mholt/caddy/server"
)

var (
	conf    string
	cpu     string
	version bool
	revoke  string
)

func init() {
	flag.StringVar(&conf, "conf", "", "Configuration file to use (default="+config.DefaultConfigFile+")")
	flag.BoolVar(&app.HTTP2, "http2", true, "Enable HTTP/2 support") // TODO: temporary flag until http2 merged into std lib
	flag.BoolVar(&app.Quiet, "quiet", false, "Quiet mode (no initialization output)")
	flag.StringVar(&cpu, "cpu", "100%", "CPU cap")
	flag.StringVar(&config.Root, "root", config.DefaultRoot, "Root path to default site")
	flag.StringVar(&config.Host, "host", config.DefaultHost, "Default host")
	flag.StringVar(&config.Port, "port", config.DefaultPort, "Default port")
	flag.BoolVar(&version, "version", false, "Show version")
	flag.BoolVar(&letsencrypt.Agreed, "agree", false, "Agree to Let's Encrypt Subscriber Agreement")
	flag.StringVar(&letsencrypt.DefaultEmail, "email", "", "Default email address to use for Let's Encrypt transactions")
	flag.StringVar(&revoke, "revoke", "", "Hostname for which to revoke the certificate")
}

func main() {
	flag.Parse()

	if version {
		fmt.Printf("%s %s\n", app.Name, app.Version)
		os.Exit(0)
	}
	if revoke != "" {
		err := letsencrypt.Revoke(revoke)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Printf("Revoked certificate for %s\n", revoke)
		os.Exit(0)
	}

	// Set CPU cap
	err := app.SetCPU(cpu)
	if err != nil {
		log.Fatal(err)
	}

	// Load config from file
	groupings, err := loadConfigs()
	if err != nil {
		log.Fatal(err)
	}

	// Start each server with its one or more configurations
	for i, group := range groupings {
		s, err := server.New(group.BindAddr.String(), group.Configs)
		if err != nil {
			log.Fatal(err)
		}
		s.HTTP2 = app.HTTP2 // TODO: This setting is temporary

		app.Wg.Add(1)
		go func(s *server.Server, i int) {
			defer app.Wg.Done()

			if os.Getenv("CADDY_RESTART") == "true" {
				file := os.NewFile(uintptr(3+i), "")
				ln, err := net.FileListener(file)
				if err != nil {
					log.Fatal("FILE LISTENER:", err)
				}

				lnf, ok := ln.(server.ListenerFile)
				if !ok {
					log.Fatal("Listener was not a ListenerFile")
				}

				err = s.Serve(lnf)
				// TODO: Better error logging... also, is it even necessary?
				if err != nil {
					log.Println(err)
				}
			} else {
				err := s.ListenAndServe()
				// TODO: Better error logging... also, is it even necessary?
				// For example, "use of closed network connection" is normal if doing graceful shutdown...
				if err != nil {
					log.Println(err)
				}
			}
		}(s, i)

		app.ServersMutex.Lock()
		app.Servers = append(app.Servers, s)
		app.ServersMutex.Unlock()
	}

	// Show initialization output
	if !app.Quiet {
		var checkedFdLimit bool
		for _, group := range groupings {
			for _, conf := range group.Configs {
				// Print address of site
				fmt.Println(conf.Address())

				// Note if non-localhost site resolves to loopback interface
				if group.BindAddr.IP.IsLoopback() && !isLocalhost(conf.Host) {
					fmt.Printf("Notice: %s is only accessible on this machine (%s)\n",
						conf.Host, group.BindAddr.IP.String())
				}
				if !checkedFdLimit && !group.BindAddr.IP.IsLoopback() && !isLocalhost(conf.Host) {
					checkFdlimit()
					checkedFdLimit = true
				}
			}
		}
	}

	// TODO: Temporary; testing restart
	if os.Getenv("CADDY_RESTART") != "true" {
		go func() {
			time.Sleep(5 * time.Second)
			fmt.Println("restarting")
			log.Println("RESTART ERR:", app.Restart([]byte{}))
		}()
	}

	// Wait for all servers to be stopped
	app.Wg.Wait()
}

// checkFdlimit issues a warning if the OS max file descriptors is below a recommended minimum.
func checkFdlimit() {
	const min = 4096

	// Warn if ulimit is too low for production sites
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
		out, err := exec.Command("sh", "-c", "ulimit -n").Output() // use sh because ulimit isn't in Linux $PATH
		if err == nil {
			// Note that an error here need not be reported
			lim, err := strconv.Atoi(string(bytes.TrimSpace(out)))
			if err == nil && lim < min {
				fmt.Printf("Warning: File descriptor limit %d is too low for production sites. At least %d is recommended. Set with \"ulimit -n %d\".\n", lim, min, min)
			}
		}
	}
}

// isLocalhost returns true if the string looks explicitly like a localhost address.
func isLocalhost(s string) bool {
	return s == "localhost" || s == "::1" || strings.HasPrefix(s, "127.")
}

// loadConfigs loads configuration from a file or stdin (piped).
// The configurations are grouped by bind address.
// Configuration is obtained from one of four sources, tried
// in this order: 1. -conf flag, 2. stdin, 3. command line argument 4. Caddyfile.
// If none of those are available, a default configuration is loaded.
func loadConfigs() (config.Group, error) {
	// -conf flag
	if conf != "" {
		file, err := os.Open(conf)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		return config.Load(path.Base(conf), file)
	}

	// stdin
	fi, err := os.Stdin.Stat()
	if err == nil && fi.Mode()&os.ModeCharDevice == 0 {
		// Note that a non-nil error is not a problem. Windows
		// will not create a stdin if there is no pipe, which
		// produces an error when calling Stat(). But Unix will
		// make one either way, which is why we also check that
		// bitmask.
		confBody, err := ioutil.ReadAll(os.Stdin)
		if err != nil {
			log.Fatal(err)
		}
		if len(confBody) > 0 {
			return config.Load("stdin", bytes.NewReader(confBody))
		}
	}

	// Command line args
	if flag.NArg() > 0 {
		confBody := ":" + config.DefaultPort + "\n" + strings.Join(flag.Args(), "\n")
		return config.Load("args", bytes.NewBufferString(confBody))
	}

	// Caddyfile
	file, err := os.Open(config.DefaultConfigFile)
	if err != nil {
		if os.IsNotExist(err) {
			return config.Default()
		}
		return nil, err
	}
	defer file.Close()

	return config.Load(config.DefaultConfigFile, file)
}
