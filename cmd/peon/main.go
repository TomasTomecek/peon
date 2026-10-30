// Command peon runs human-approved commands from Forgejo issues. See SPEC.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/TomasTomecek/peon/internal/config"
	"github.com/TomasTomecek/peon/internal/daemon"
	"github.com/TomasTomecek/peon/internal/executor"
	"github.com/TomasTomecek/peon/internal/forgejo"
	"github.com/TomasTomecek/peon/internal/loki"
	"github.com/TomasTomecek/peon/internal/state"
	"github.com/TomasTomecek/peon/internal/status"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func usage() {
	fmt.Fprintf(os.Stderr, `Usage: peon [--config PATH] <command>

Commands:
  version          print the version
  check-config     validate the configuration file and secret files
  status [--json]  show running, queued and waiting jobs, and health
  run              run the daemon

Options:
`)
	flag.PrintDefaults()
}

func main() {
	cfgPath := flag.String("config", config.DefaultPath, "path to config.yaml")
	stateDir := flag.String("state-dir", defaultStateDir(), "directory for state and the audit spool")
	flag.Usage = usage
	flag.Parse()

	if flag.NArg() != 1 && !(flag.NArg() == 2 && flag.Arg(0) == "status" && flag.Arg(1) == "--json") {
		usage()
		os.Exit(2)
	}

	switch flag.Arg(0) {
	case "version":
		fmt.Println("peon", version)
	case "check-config":
		c, err := config.Load(*cfgPath)
		if err == nil {
			err = c.CheckHost(lookupUser, config.LingerDir)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "peon: invalid configuration:\n%v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%s: OK (host %s, %d repo(s), approver %d, jobs as %s, default %s)\n",
			*cfgPath, c.Host, len(c.Forgejo.Repos), c.Forgejo.ApproverID,
			strings.Join(c.Jobs.AllowedUsers, "/"), c.Jobs.DefaultUser)
	case "status":
		// Best effort: the host name is nice to show but not required.
		host := ""
		if c, err := config.Load(*cfgPath); err == nil {
			host = c.Host
		}
		r, err := status.Collect(*stateDir, host, time.Now().UTC())
		if err != nil {
			fmt.Fprintf(os.Stderr, "peon: %v\n", err)
			os.Exit(1)
		}
		if flag.NArg() == 2 {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(r); err != nil {
				fmt.Fprintf(os.Stderr, "peon: %v\n", err)
				os.Exit(1)
			}
			return
		}
		fmt.Print(r.Text())
	case "run":
		c, err := config.Load(*cfgPath)
		if err == nil {
			err = c.CheckHost(lookupUser, config.LingerDir)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "peon: invalid configuration:\n%v\n", err)
			os.Exit(1)
		}
		if err := run(c, *stateDir); err != nil {
			fmt.Fprintf(os.Stderr, "peon: %v\n", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func lookupUser(name string) error {
	_, err := user.Lookup(name)
	return err
}

// defaultStateDir follows systemd's StateDirectory= when set.
func defaultStateDir() string {
	if d := os.Getenv("STATE_DIRECTORY"); d != "" {
		return d
	}
	return "/var/lib/peon"
}

func run(c *config.Config, stateDir string) error {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ua := "peon/" + version

	token, err := config.ReadSecret(c.Forgejo.BotTokenFile)
	if err != nil {
		return err
	}
	password, err := config.ReadSecret(c.Loki.PasswordFile)
	if err != nil {
		return err
	}
	fj, err := forgejo.New(c.Forgejo.URL, token, ua)
	if err != nil {
		return err
	}
	spool, err := loki.NewSpool(filepath.Join(stateDir, "spool"))
	if err != nil {
		return err
	}
	st, err := state.Open(filepath.Join(stateDir, "state.json"))
	if err != nil {
		return err
	}
	defer st.Close()

	d := daemon.New(c, fj, loki.New(c.Loki.URL, c.Loki.User, password, ua), spool, &executor.Systemd{OutputDir: filepath.Join(stateDir, "output")}, st, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log.Info("peon starting", "version", version, "host", c.Host, "repos", c.Forgejo.Repos)
	err = d.Run(ctx)
	log.Info("peon stopped")
	return err
}
