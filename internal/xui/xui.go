package xui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// 3x-ui install layout. The management script lives at /usr/local/x-ui and the
// service is managed by systemd as "x-ui".
const (
	xuiFolder = "/usr/local/x-ui"
	xuiBin    = "/usr/local/x-ui/x-ui"
)

// SupportedVersions are the only versions we let users install. "latest" maps
// to the project's master install script with no version arg.
var SupportedVersions = map[string]bool{
	"latest": true,
	"v2.9.4": true,
}

// Settings is the parsed output of `x-ui setting -show true` plus the access URL
// derived from `x-ui settings`.
type Settings struct {
	Installed       bool   `json:"installed"`
	Running         bool   `json:"running"`
	Port            int    `json:"port"`
	WebBasePath     string `json:"webBasePath"`
	HasSSL          bool   `json:"hasSsl"`
	CertFile        string `json:"certFile"`
	KeyFile         string `json:"keyFile"`
	AccessURL       string `json:"accessUrl"`
	HasDefaultCreds bool   `json:"hasDefaultCredential"`
	Version         string `json:"version"`
}

// Installed reports whether 3x-ui is present on the host.
func Installed() bool {
	_, err := os.Stat(xuiBin)
	return err == nil
}

// Install runs the official 3x-ui installer for the given version. version must
// be one of SupportedVersions. It is a long operation (downloads, compiles deps).
func Install(ctx context.Context, version string) (string, error) {
	if !SupportedVersions[version] {
		return "", fmt.Errorf("unsupported version %q", version)
	}
	if Installed() {
		return "", fmt.Errorf("a panel is already installed; uninstall it first")
	}

	var script string
	if version == "latest" {
		script = `bash <(curl -Ls https://raw.githubusercontent.com/mhsanaei/3x-ui/master/install.sh)`
	} else {
		// VERSION=vX && bash <(curl -Ls ".../$VERSION/install.sh") $VERSION
		script = fmt.Sprintf(
			`VERSION=%s && bash <(curl -Ls "https://raw.githubusercontent.com/mhsanaei/3x-ui/$VERSION/install.sh") $VERSION`,
			version,
		)
	}
	// The installer is interactive at the very end (asks to set user/port). We
	// answer "n" to its prompt so it keeps random defaults; the agent then sets
	// the user-chosen credentials/port explicitly afterward.
	full := "yes n | " + script
	out, err := run(ctx, 10*time.Minute, "/bin/bash", "-c", full)
	if err != nil {
		return out, fmt.Errorf("3x-ui install failed: %w", err)
	}
	if !Installed() {
		return out, fmt.Errorf("installer finished but x-ui binary not found")
	}
	return out, nil
}

// Uninstall removes 3x-ui via its own uninstall path.
func Uninstall(ctx context.Context) (string, error) {
	if !Installed() {
		return "already uninstalled", nil
	}
	// `x-ui uninstall` is interactive (confirm y). Feed yes.
	out, err := run(ctx, 5*time.Minute, "/bin/bash", "-c", "yes y | x-ui uninstall")
	if err != nil && Installed() {
		return out, fmt.Errorf("uninstall failed: %w", err)
	}
	return out, nil
}

// SetCredentials resets the panel username and password.
func SetCredentials(ctx context.Context, username, password string) (string, error) {
	if err := requireInstalled(); err != nil {
		return "", err
	}
	if username == "" || password == "" {
		return "", fmt.Errorf("username and password are required")
	}
	out, err := run(ctx, time.Minute, xuiBin, "setting", "-username", username, "-password", password)
	if err != nil {
		return out, fmt.Errorf("set credentials failed: %w", err)
	}
	return restart(ctx)
}

// SetPort changes the panel port.
func SetPort(ctx context.Context, port int) (string, error) {
	if err := requireInstalled(); err != nil {
		return "", err
	}
	if port < 1 || port > 65535 {
		return "", fmt.Errorf("invalid port %d", port)
	}
	out, err := run(ctx, time.Minute, xuiBin, "setting", "-port", fmt.Sprintf("%d", port))
	if err != nil {
		return out, fmt.Errorf("set port failed: %w", err)
	}
	return restart(ctx)
}

// SetWebBasePath changes the panel web base path (e.g. /abc123/).
func SetWebBasePath(ctx context.Context, path string) (string, error) {
	if err := requireInstalled(); err != nil {
		return "", err
	}
	out, err := run(ctx, time.Minute, xuiBin, "setting", "-webBasePath", path)
	if err != nil {
		return out, fmt.Errorf("set webBasePath failed: %w", err)
	}
	return restart(ctx)
}

// SetCert points the panel at a certificate/key pair (e.g. the ones issued by
// the ssl package). Restarts the panel so it serves the new cert.
func SetCert(ctx context.Context, certFile, keyFile string) (string, error) {
	if err := requireInstalled(); err != nil {
		return "", err
	}
	if _, err := os.Stat(certFile); err != nil {
		return "", fmt.Errorf("cert file not found: %s", certFile)
	}
	if _, err := os.Stat(keyFile); err != nil {
		return "", fmt.Errorf("key file not found: %s", keyFile)
	}
	out, err := run(ctx, time.Minute, xuiBin, "cert", "-webCert", certFile, "-webCertKey", keyFile)
	if err != nil {
		return out, fmt.Errorf("set cert failed: %w", err)
	}
	return restart(ctx)
}

// ResetCert clears the panel certificate (back to self-signed / none).
func ResetCert(ctx context.Context) (string, error) {
	if err := requireInstalled(); err != nil {
		return "", err
	}
	out, err := run(ctx, time.Minute, xuiBin, "cert", "-reset")
	if err != nil {
		return out, fmt.Errorf("reset cert failed: %w", err)
	}
	return restart(ctx)
}

// GetSettings parses current panel settings into a struct for the dashboard.
func GetSettings(ctx context.Context) (*Settings, error) {
	s := &Settings{Installed: Installed()}
	if !s.Installed {
		return s, nil
	}
	s.Running = isRunning(ctx)

	show, _ := run(ctx, 30*time.Second, xuiBin, "setting", "-show", "true")
	s.Port = atoiField(show, "port")
	s.WebBasePath = strField(show, "webBasePath")
	if v := strField(show, "hasDefaultCredential"); v != "" {
		s.HasDefaultCreds = strings.EqualFold(v, "true")
	}

	cert, _ := run(ctx, 30*time.Second, xuiBin, "setting", "-getCert", "true")
	s.CertFile = strField(cert, "cert")
	s.KeyFile = strField(cert, "key")
	s.HasSSL = s.CertFile != ""

	// `x-ui settings` prints a human "Access URL: ..." line we can surface as-is.
	human, _ := run(ctx, 30*time.Second, "/bin/bash", "-c", "x-ui settings")
	s.AccessURL = grabAccessURL(human)

	return s, nil
}

// Status returns the raw `x-ui status` output for display.
func Status(ctx context.Context) (string, error) {
	if err := requireInstalled(); err != nil {
		return "", err
	}
	return run(ctx, 30*time.Second, "/bin/bash", "-c", "x-ui status")
}

func Start(ctx context.Context) (string, error)   { return svc(ctx, "start") }
func Stop(ctx context.Context) (string, error)     { return svc(ctx, "stop") }
func Restart(ctx context.Context) (string, error)  { return restart(ctx) }

// --- helpers ---

func requireInstalled() error {
	if !Installed() {
		return fmt.Errorf("3x-ui is not installed")
	}
	return nil
}

func restart(ctx context.Context) (string, error) { return svc(ctx, "restart") }

func svc(ctx context.Context, action string) (string, error) {
	if err := requireInstalled(); err != nil {
		return "", err
	}
	return run(ctx, 90*time.Second, "/bin/bash", "-c", "x-ui "+action)
}

func isRunning(ctx context.Context) bool {
	out, _ := run(ctx, 15*time.Second, "systemctl", "is-active", "x-ui")
	return strings.TrimSpace(out) == "active"
}

var fieldRe = func(key string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(key) + `:\s*(.+?)\s*$`)
}

func strField(blob, key string) string {
	m := fieldRe(key).FindStringSubmatch(blob)
	if len(m) == 2 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

func atoiField(blob, key string) int {
	v := strField(blob, key)
	n := 0
	for _, c := range v {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

var accessURLRe = regexp.MustCompile(`Access URL:\s*(\S+)`)

func grabAccessURL(blob string) string {
	m := accessURLRe.FindStringSubmatch(blob)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

func run(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Env = append(os.Environ(), "HOME=/root")
	out, err := cmd.CombinedOutput()
	return string(out), err
}
