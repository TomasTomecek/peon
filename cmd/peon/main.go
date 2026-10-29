// Command peon runs human-approved commands from Forgejo issues. See SPEC.md.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/TomasTomecek/peon/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: peon [--config PATH] <command>

Commands:
  version        print the version
  check-config   validate the configuration file and secret files
  run            run the daemon

Options:
`)
	flag.PrintDefaults()
}

func main() {
	cfgPath := flag.String("config", config.DefaultPath, "path to config.toml")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 {
		usage()
		os.Exit(2)
	}

	switch flag.Arg(0) {
	case "version":
		fmt.Println("peon", version)
	case "check-config":
		c, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "peon: invalid configuration:\n%v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%s: OK (host %s, %d repo(s), approver %d)\n",
			*cfgPath, c.Host, len(c.Forgejo.Repos), c.Forgejo.ApproverID)
	case "run":
		c, err := config.Load(*cfgPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "peon: invalid configuration:\n%v\n", err)
			os.Exit(1)
		}
		// The daemon loop (SPEC.md §5-§9) is not implemented yet. Refuse to
		// pretend otherwise: exit non-zero so systemd shows the unit as failed.
		fmt.Fprintf(os.Stderr, "peon %s: config for host %s loaded; daemon not implemented yet\n", version, c.Host)
		os.Exit(1)
	default:
		usage()
		os.Exit(2)
	}
}
