// Package cli renders the interactive `skypass-manager` terminal interface.
//
// It is intentionally dependency-free (stdlib only) and works over a plain TTY:
// a numbered menu the operator drives with the keyboard. It wraps systemd and
// the agent's own subcommands so an operator never has to remember the
// underlying journalctl / systemctl incantations.
package cli

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/SkyPass-Cloud/manager-node/internal/config"
	"github.com/SkyPass-Cloud/manager-node/internal/updater"
)

// ANSI colours. Kept minimal so the UI still reads fine on a dumb terminal.
const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	cyan   = "\033[36m"
	green  = "\033[32m"
	red    = "\033[31m"
	yellow = "\033[33m"
)

const serviceName = "skypassd"

// Run shows the interactive menu loop. version is the running binary's version.
func Run(version string) {
	in := bufio.NewReader(os.Stdin)
	for {
		clear()
		header(version)
		printStatusBlock(version)
		fmt.Println()
		fmt.Println(bold + "  Menu" + reset)
		fmt.Println("    1) Service status")
		fmt.Println("    2) Live logs (follow)")
		fmt.Println("    3) Recent logs (last 200 lines)")
		fmt.Println("    4) Restart agent")
		fmt.Println("    5) Stop agent")
		fmt.Println("    6) Start agent")
		fmt.Println("    7) Update to latest version")
		fmt.Println("    8) Show config")
		fmt.Println("    9) Uninstall agent")
		fmt.Println("    0) Exit")
		fmt.Print("\n  " + cyan + "Select: " + reset)

		choice, _ := in.ReadString('\n')
		switch strings.TrimSpace(choice) {
		case "1":
			runService("status", "--no-pager")
			pause(in)
		case "2":
			fmt.Println(dim + "  (Ctrl+C to stop following)" + reset)
			follow()
		case "3":
			recentLogs()
			pause(in)
		case "4":
			runService("restart")
			fmt.Println(green + "  restarted" + reset)
			pause(in)
		case "5":
			runService("stop")
			fmt.Println(yellow + "  stopped" + reset)
			pause(in)
		case "6":
			runService("start")
			fmt.Println(green + "  started" + reset)
			pause(in)
		case "7":
			doUpdate(version)
			pause(in)
		case "8":
			showConfig()
			pause(in)
		case "9":
			confirmUninstall(in)
			pause(in)
		case "0", "q", "quit", "exit":
			fmt.Println("  bye")
			return
		default:
			fmt.Println(red + "  invalid choice" + reset)
			pause(in)
		}
	}
}

func header(version string) {
	fmt.Println(bold + cyan + "  ┌─────────────────────────────────────────────┐" + reset)
	fmt.Println(bold + cyan + "  │            SkyPass Node Manager              │" + reset)
	fmt.Println(bold + cyan + "  └─────────────────────────────────────────────┘" + reset)
	fmt.Printf("  %sversion%s %s\n", dim, reset, version)
}

// printStatusBlock shows a quick at-a-glance summary: role, registration, the
// systemd active state and the configured listen port.
func printStatusBlock(version string) {
	cfg, err := config.Load(config.DefaultPath)
	fmt.Println()
	if err == config.ErrNotFound {
		fmt.Println("  " + yellow + "not configured (run install first)" + reset)
		return
	} else if err != nil {
		fmt.Printf("  %sconfig error: %v%s\n", red, err, reset)
		return
	}
	role := cfg.Role
	if role == "" {
		role = "node"
	}
	active := serviceActiveState()
	stateColor := green
	if active != "active" {
		stateColor = red
	}
	reg := cfg.NodeID
	if reg == "" {
		reg = yellow + "unregistered" + reset
	}
	fmt.Printf("  %-10s %s\n", "role:", role)
	fmt.Printf("  %-10s %s%s%s\n", "service:", stateColor, active, reset)
	fmt.Printf("  %-10s %s\n", "registered:", reg)
	fmt.Printf("  %-10s %d\n", "port:", cfg.ListenPort)
	fmt.Printf("  %-10s %s\n", "site:", cfg.SiteBaseURL)
}

// serviceActiveState returns systemd's active state ("active", "inactive", ...).
func serviceActiveState() string {
	out, _ := exec.Command("systemctl", "is-active", serviceName).Output()
	s := strings.TrimSpace(string(out))
	if s == "" {
		return "unknown"
	}
	return s
}

func runService(action string, extra ...string) {
	args := append([]string{action, serviceName}, extra...)
	cmd := exec.Command("systemctl", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

// follow streams journald output live until the operator hits Ctrl+C. The
// node-manager prefix lets the same grep token work everywhere.
func follow() {
	cmd := exec.Command("journalctl", "-u", serviceName, "-f", "-n", "50", "--no-pager")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	_ = cmd.Run()
}

func recentLogs() {
	cmd := exec.Command("journalctl", "-u", serviceName, "-n", "200", "--no-pager")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	_ = cmd.Run()
}

func showConfig() {
	cfg, err := config.Load(config.DefaultPath)
	if err == config.ErrNotFound {
		fmt.Println(yellow + "  no config (not installed)" + reset)
		return
	} else if err != nil {
		fmt.Printf("%s  config error: %v%s\n", red, err, reset)
		return
	}
	role := cfg.Role
	if role == "" {
		role = "node"
	}
	fmt.Printf("  role:        %s\n", role)
	fmt.Printf("  site:        %s\n", cfg.SiteBaseURL)
	fmt.Printf("  nodeId:      %s\n", dash(cfg.NodeID))
	fmt.Printf("  agentId:     %s\n", cfg.AgentID)
	fmt.Printf("  port:        %d\n", cfg.ListenPort)
	fmt.Printf("  acmeEmail:   %s\n", dash(cfg.AcmeEmail))
	fmt.Printf("  binaryUrl:   %s\n", dash(cfg.BinaryURL))
	fmt.Printf("  configPath:  %s\n", cfg.Path())
	// Token is intentionally NOT printed.
}

// doUpdate downloads the latest binary and restarts the service. It uses the
// BinaryURL stored at install time.
func doUpdate(currentVersion string) {
	cfg, err := config.Load(config.DefaultPath)
	if err != nil && err != config.ErrNotFound {
		fmt.Printf("%s  cannot read config: %v%s\n", red, err, reset)
		return
	}
	if cfg.BinaryURL == "" {
		fmt.Println(red + "  no binaryUrl recorded; re-run the install command to update." + reset)
		return
	}
	fmt.Printf("  current version: %s\n", currentVersion)
	fmt.Println("  downloading latest binary...")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	newVer, err := updater.SelfUpdate(ctx, cfg.BinaryURL, serviceName)
	if err != nil {
		fmt.Printf("%s  update failed: %v%s\n", red, err, reset)
		return
	}
	fmt.Printf("%s  updated to %s and restarted the service.%s\n", green, newVer, reset)
}

func confirmUninstall(in *bufio.Reader) {
	fmt.Print(red + "  Type 'yes' to stop and remove the agent: " + reset)
	ans, _ := in.ReadString('\n')
	if strings.TrimSpace(ans) != "yes" {
		fmt.Println("  cancelled")
		return
	}
	runService("stop")
	runService("disable")
	// Remove the unit + the self path + config.
	_ = exec.Command("/usr/local/bin/skypassd", "uninstall").Run()
	_ = os.Remove("/etc/systemd/system/skypassd.service")
	_ = exec.Command("systemctl", "daemon-reload").Run()
	fmt.Println(green + "  uninstalled. Binary at /usr/local/bin/skypassd left in place; remove it manually if desired." + reset)
}

// --- small helpers ---

func clear() { fmt.Print("\033[H\033[2J") }

func pause(in *bufio.Reader) {
	fmt.Print(dim + "\n  press Enter to continue..." + reset)
	_, _ = in.ReadString('\n')
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
