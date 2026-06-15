package firewall

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// Backend names the firewall manager detected on the host.
type Backend string

const (
	BackendNone      Backend = "none"      // no firewall tooling found
	BackendUFW       Backend = "ufw"       // Debian/Ubuntu default
	BackendFirewalld Backend = "firewalld" // RHEL/CentOS/Fedora
	BackendIptables  Backend = "iptables"  // raw fallback
)

// Detect figures out which firewall backend is active. It prefers higher-level
// managers (ufw, firewalld) over raw iptables, and only reports a manager as
// active if it is actually enabled — an installed-but-inactive ufw should not
// block us, but if it is active we must add a rule or our port stays closed.
func Detect() Backend {
	if path, err := exec.LookPath("ufw"); err == nil {
		out, _ := exec.Command(path, "status").CombinedOutput()
		if strings.Contains(strings.ToLower(string(out)), "status: active") {
			return BackendUFW
		}
	}
	if path, err := exec.LookPath("firewall-cmd"); err == nil {
		out, _ := exec.Command(path, "--state").CombinedOutput()
		if strings.Contains(strings.ToLower(string(out)), "running") {
			return BackendFirewalld
		}
	}
	if _, err := exec.LookPath("iptables"); err == nil {
		return BackendIptables
	}
	return BackendNone
}

// OpenPort opens the given TCP port on whatever firewall backend is active.
// It is idempotent where the backend supports it. Returns the backend used and
// any error. A BackendNone result with nil error means no firewall needed
// touching.
func OpenPort(port int) (Backend, error) {
	if port <= 0 || port > 65535 {
		return BackendNone, fmt.Errorf("invalid port %d", port)
	}
	b := Detect()
	switch b {
	case BackendUFW:
		return b, run("ufw", "allow", fmt.Sprintf("%d/tcp", port))
	case BackendFirewalld:
		// add a permanent rule then reload so it survives reboot.
		if err := run("firewall-cmd", "--permanent", "--add-port", fmt.Sprintf("%d/tcp", port)); err != nil {
			return b, err
		}
		return b, run("firewall-cmd", "--reload")
	case BackendIptables:
		// insert only if an identical rule does not already exist.
		if iptablesRuleExists(port) {
			return b, nil
		}
		return b, run("iptables", "-I", "INPUT", "-p", "tcp", "--dport", strconv.Itoa(port), "-j", "ACCEPT")
	default:
		return BackendNone, nil
	}
}

// ClosePort removes the rule that OpenPort added. Best-effort; used on uninstall.
func ClosePort(port int) (Backend, error) {
	b := Detect()
	switch b {
	case BackendUFW:
		return b, run("ufw", "delete", "allow", fmt.Sprintf("%d/tcp", port))
	case BackendFirewalld:
		if err := run("firewall-cmd", "--permanent", "--remove-port", fmt.Sprintf("%d/tcp", port)); err != nil {
			return b, err
		}
		return b, run("firewall-cmd", "--reload")
	case BackendIptables:
		return b, run("iptables", "-D", "INPUT", "-p", "tcp", "--dport", strconv.Itoa(port), "-j", "ACCEPT")
	default:
		return BackendNone, nil
	}
}

func iptablesRuleExists(port int) bool {
	err := exec.Command("iptables", "-C", "INPUT", "-p", "tcp", "--dport", strconv.Itoa(port), "-j", "ACCEPT").Run()
	return err == nil
}

func run(name string, args ...string) error {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}
