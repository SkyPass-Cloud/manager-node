package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/SkyPass-Cloud/manager-node/internal/agent"
	"github.com/SkyPass-Cloud/manager-node/internal/config"
	"github.com/SkyPass-Cloud/manager-node/internal/firewall"
	"github.com/SkyPass-Cloud/manager-node/internal/portpick"
)

// version is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	// Tag every log line with [node-manager] so the agent's journald output can
	// be filtered alongside the server side with the same grep token:
	//   journalctl -u skypassd | grep node-manager
	log.SetPrefix("[node-manager] ")
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "run":
		cmdRun(os.Args[2:])
	case "install":
		cmdInstall(os.Args[2:])
	case "uninstall":
		cmdUninstall(os.Args[2:])
	case "status":
		cmdStatus(os.Args[2:])
	case "version", "-v", "--version":
		fmt.Println("skypassd", version)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Print(`skypassd - SkyPass node agent

Usage:
  skypassd install --site <url> --token <token> [--config <path>]
      Register config, pick a port, open the firewall. Run once at setup.
  skypassd run [--config <path>]
      Run the agent in the foreground (systemd ExecStart target).
  skypassd status [--config <path>]
      Print local config and a one-shot host status snapshot.
  skypassd uninstall [--config <path>]
      Close the firewall port and remove local config.
  skypassd version
      Print the agent version.
`)
}

// cmdInstall writes the config, generates an agent id, picks a port and opens
// the firewall. It does NOT install the systemd unit — install.sh does that and
// then calls this. Safe to re-run; it preserves an existing port/agent id.
func cmdInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	site := fs.String("site", "", "site base URL, e.g. https://panel.example.com")
	token := fs.String("token", "", "bootstrap auth token")
	path := fs.String("config", config.DefaultPath, "config file path")
	port := fs.Int("port", 0, "force a specific port (must be in allowed ranges)")
	acmeEmail := fs.String("acme-email", "", "contact email for Let's Encrypt")
	_ = fs.Parse(args)

	if *site == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "install: --site and --token are required")
		os.Exit(2)
	}

	cfg, err := config.Load(*path)
	if err != nil && err != config.ErrNotFound {
		fatal("load config", err)
	}
	cfg.SiteBaseURL = *site
	cfg.Token = *token
	if *acmeEmail != "" {
		cfg.AcmeEmail = *acmeEmail
	}
	cfg.EnsureAgentID()

	// Pick a port only if we do not already have a valid one.
	if !portpick.Allowed(cfg.ListenPort) {
		chosen := *port
		if chosen == 0 {
			chosen, err = portpick.Pick()
			if err != nil {
				fatal("pick port", err)
			}
		} else if !portpick.Allowed(chosen) {
			fmt.Fprintf(os.Stderr, "install: port %d is not in the allowed ranges\n", chosen)
			os.Exit(2)
		}
		cfg.ListenPort = chosen
	}

	if err := cfg.Save(); err != nil {
		fatal("save config", err)
	}

	backend, err := firewall.OpenPort(cfg.ListenPort)
	if err != nil {
		// Not fatal: the node can still reach the site outbound; only direct
		// push from the site would be blocked. Surface it clearly.
		fmt.Fprintf(os.Stderr, "warning: could not open firewall port %d via %s: %v\n", cfg.ListenPort, backend, err)
	} else {
		fmt.Printf("firewall (%s): opened port %d/tcp\n", backend, cfg.ListenPort)
	}

	fmt.Printf("installed: agentId=%s port=%d config=%s\n", cfg.AgentID, cfg.ListenPort, cfg.Path())
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	path := fs.String("config", config.DefaultPath, "config file path")
	_ = fs.Parse(args)

	cfg, err := config.Load(*path)
	if err == config.ErrNotFound {
		fatal("run", fmt.Errorf("no config at %s; run `skypassd install` first", *path))
	} else if err != nil {
		fatal("load config", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	a := agent.New(cfg, version)
	if err := a.Run(ctx); err != nil && err != context.Canceled {
		fatal("run", err)
	}
}

func cmdUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	path := fs.String("config", config.DefaultPath, "config file path")
	_ = fs.Parse(args)

	cfg, err := config.Load(*path)
	if err == config.ErrNotFound {
		fmt.Println("nothing to uninstall (no config)")
		return
	} else if err != nil {
		fatal("load config", err)
	}
	if cfg.ListenPort != 0 {
		if backend, err := firewall.ClosePort(cfg.ListenPort); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not close port %d via %s: %v\n", cfg.ListenPort, backend, err)
		} else {
			fmt.Printf("firewall (%s): closed port %d/tcp\n", backend, cfg.ListenPort)
		}
	}
	if err := os.Remove(cfg.Path()); err != nil && !os.IsNotExist(err) {
		fatal("remove config", err)
	}
	fmt.Println("uninstalled local config")
}

func cmdStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	path := fs.String("config", config.DefaultPath, "config file path")
	_ = fs.Parse(args)

	cfg, err := config.Load(*path)
	if err == config.ErrNotFound {
		fmt.Printf("no config at %s\n", *path)
		return
	} else if err != nil {
		fatal("load config", err)
	}
	fmt.Printf("site:    %s\n", cfg.SiteBaseURL)
	fmt.Printf("nodeId:  %s\n", emptyDash(cfg.NodeID))
	fmt.Printf("agentId: %s\n", cfg.AgentID)
	fmt.Printf("port:    %d\n", cfg.ListenPort)
	fmt.Printf("firewall:%s\n", firewall.Detect())
}

func emptyDash(s string) string {
	if s == "" {
		return "(unregistered)"
	}
	return s
}

func fatal(ctx string, err error) {
	fmt.Fprintf(os.Stderr, "%s: %v\n", ctx, err)
	os.Exit(1)
}
