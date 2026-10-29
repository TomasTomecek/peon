// Command peon runs human-approved commands from Forgejo issues. See SPEC.md.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/TomasTomecek/peon/internal/config"
	"github.com/TomasTomecek/peon/internal/daemon"
	"github.com/TomasTomecek/peon/internal/executor"
	"github.com/TomasTomecek/peon/internal/forgejo"
	"github.com/TomasTomecek/peon/internal/loki"
	"github.com/TomasTomecek/peon/internal/state"
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
	cfgPath := flag.String("config", config.DefaultPath, "path to config.yaml")
	stateDir := flag.String("state-dir", defaultStateDir(), "directory for state and the audit spool")
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
		if err := run(c, *stateDir); err != nil {
			fmt.Fprintf(os.Stderr, "peon: %v\n", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
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

	d := daemon.New(c, fj, loki.New(c.Loki.URL, c.Loki.User, password, ua), spool, &executor.Systemd{}, st, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	log.Info("peon starting", "version", version, "host", c.Host, "repos", c.Forgejo.Repos)
	err = d.Run(ctx)
	log.Info("peon stopped")
	return err
}
