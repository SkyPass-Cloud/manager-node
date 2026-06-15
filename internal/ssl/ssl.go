package ssl

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// acme.sh is installed per-user under ~/.acme.sh. The agent runs as root, so
// HOME is /root. This mirrors how 3x-ui issues certs.
const (
	acmeHome  = "/root/.acme.sh"
	acmeBin   = "/root/.acme.sh/acme.sh"
	certsRoot = "/etc/skypassd/certs"
)

// CertPaths is where an issued certificate's files live on the VPS.
type CertPaths struct {
	Domain   string `json:"domain"`
	CertFile string `json:"certFile"` // fullchain.cer
	KeyFile  string `json:"keyFile"`  // <domain>.key
	CABundle string `json:"caBundle"` // ca.cer
	Issued   bool   `json:"issued"`
}

// DNSCheck reports whether a domain resolves to one of this server's public IPs.
type DNSCheck struct {
	Domain      string   `json:"domain"`
	ResolvedIPs []string `json:"resolvedIps"`
	ServerIPs   []string `json:"serverIps"`
	PointsHere  bool     `json:"pointsHere"`
	Message     string   `json:"message"`
}

// EnsureACME installs acme.sh if it is missing. Idempotent.
func EnsureACME(ctx context.Context, accountEmail string) error {
	if _, err := os.Stat(acmeBin); err == nil {
		return nil
	}
	// curl https://get.acme.sh | sh — the official bootstrap.
	if _, err := exec.LookPath("curl"); err != nil {
		return fmt.Errorf("curl is required to install acme.sh: %w", err)
	}
	email := accountEmail
	if email == "" {
		email = "admin@" + hostnameOrLocal()
	}
	script := fmt.Sprintf("curl -fsSL https://get.acme.sh | sh -s email=%s", shellQuote(email))
	if out, err := runCtx(ctx, 3*time.Minute, "/bin/sh", "-c", script); err != nil {
		return fmt.Errorf("install acme.sh: %v: %s", err, out)
	}
	// Use Let's Encrypt as the default CA.
	_, _ = runCtx(ctx, time.Minute, acmeBin, "--set-default-ca", "--server", "letsencrypt")
	return nil
}

// CheckDNS resolves the domain and compares against the server's public IPs.
// It is the gate the frontend hits before showing the "Get SSL" button.
func CheckDNS(ctx context.Context, domain string) DNSCheck {
	res := DNSCheck{Domain: domain}
	domain = strings.TrimSpace(strings.ToLower(domain))
	if !validDomain(domain) {
		res.Message = "invalid domain"
		return res
	}

	var r net.Resolver
	ips, err := r.LookupHost(ctx, domain)
	if err != nil {
		res.Message = "domain does not resolve: " + err.Error()
		return res
	}
	res.ResolvedIPs = ips
	res.ServerIPs = publicIPs(ctx)

	serverSet := map[string]bool{}
	for _, ip := range res.ServerIPs {
		serverSet[ip] = true
	}
	for _, ip := range ips {
		if serverSet[ip] {
			res.PointsHere = true
			break
		}
	}
	if res.PointsHere {
		res.Message = "domain points to this server"
	} else {
		res.Message = "domain does not point to this server's IP"
	}
	return res
}

// Issue obtains a certificate for domain using HTTP-01 standalone. It returns
// the on-disk paths. acme.sh handles renewal via its own installed cron.
//
// standalone binds port 80, so anything using it (the panel, nginx) must be
// briefly free. The caller decides whether to stop such services first.
func Issue(ctx context.Context, domain, accountEmail string) (*CertPaths, error) {
	domain = strings.TrimSpace(strings.ToLower(domain))
	if !validDomain(domain) {
		return nil, fmt.Errorf("invalid domain %q", domain)
	}
	if err := EnsureACME(ctx, accountEmail); err != nil {
		return nil, err
	}

	check := CheckDNS(ctx, domain)
	if !check.PointsHere {
		return nil, fmt.Errorf("%s", check.Message)
	}

	dir := filepath.Join(certsRoot, domain)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create cert dir: %w", err)
	}

	// Issue with standalone HTTP-01.
	if out, err := runCtx(ctx, 4*time.Minute, acmeBin,
		"--issue", "-d", domain, "--standalone", "--keylength", "ec-256",
	); err != nil {
		// acme.sh returns non-zero if the cert already exists and is valid; treat
		// "Domains not changed" / "next renewal" as success and fall through to install.
		low := strings.ToLower(out)
		if !strings.Contains(low, "next renewal") && !strings.Contains(low, "domains not changed") {
			return nil, fmt.Errorf("acme issue failed: %v: %s", err, out)
		}
	}

	paths := &CertPaths{
		Domain:   domain,
		CertFile: filepath.Join(dir, "fullchain.cer"),
		KeyFile:  filepath.Join(dir, domain+".key"),
		CABundle: filepath.Join(dir, "ca.cer"),
	}

	// Install (copy) the cert to our stable, predictable location.
	if out, err := runCtx(ctx, time.Minute, acmeBin,
		"--install-cert", "-d", domain, "--ecc",
		"--key-file", paths.KeyFile,
		"--fullchain-file", paths.CertFile,
		"--ca-file", paths.CABundle,
	); err != nil {
		return nil, fmt.Errorf("acme install-cert failed: %v: %s", err, out)
	}

	if _, err := os.Stat(paths.CertFile); err != nil {
		return nil, fmt.Errorf("cert file missing after issue: %w", err)
	}
	paths.Issued = true
	return paths, nil
}

// Remove revokes/removes a domain's cert from acme.sh and deletes local files.
func Remove(ctx context.Context, domain string) error {
	domain = strings.TrimSpace(strings.ToLower(domain))
	if !validDomain(domain) {
		return fmt.Errorf("invalid domain %q", domain)
	}
	_, _ = runCtx(ctx, time.Minute, acmeBin, "--remove", "-d", domain, "--ecc")
	_ = os.RemoveAll(filepath.Join(certsRoot, domain))
	return nil
}

// Paths returns the predictable cert paths for a domain without issuing.
func Paths(domain string) CertPaths {
	domain = strings.TrimSpace(strings.ToLower(domain))
	dir := filepath.Join(certsRoot, domain)
	cp := CertPaths{
		Domain:   domain,
		CertFile: filepath.Join(dir, "fullchain.cer"),
		KeyFile:  filepath.Join(dir, domain+".key"),
		CABundle: filepath.Join(dir, "ca.cer"),
	}
	if _, err := os.Stat(cp.CertFile); err == nil {
		cp.Issued = true
	}
	return cp
}

// --- helpers ---

func validDomain(d string) bool {
	if d == "" || len(d) > 253 || strings.Contains(d, " ") {
		return false
	}
	if !strings.Contains(d, ".") {
		return false
	}
	for _, r := range d {
		if !(r == '.' || r == '-' || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

// publicIPs returns the server's outward-facing IPs. It first asks external
// echo services (most reliable behind NAT) and falls back to local interfaces.
func publicIPs(ctx context.Context) []string {
	seen := map[string]bool{}
	var out []string
	add := func(ip string) {
		ip = strings.TrimSpace(ip)
		if ip != "" && !seen[ip] {
			seen[ip] = true
			out = append(out, ip)
		}
	}
	for _, url := range []string{"https://api4.ipify.org", "https://ipv4.icanhazip.com"} {
		if s, err := runCtx(ctx, 5*time.Second, "curl", "-fsS", "--max-time", "3", url); err == nil {
			add(strings.TrimSpace(s))
		}
	}
	// local interface fallback
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() {
				if v4 := ipn.IP.To4(); v4 != nil {
					add(v4.String())
				}
			}
		}
	}
	return out
}

func hostnameOrLocal() string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return "localhost"
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func runCtx(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, name, args...)
	cmd.Env = append(os.Environ(), "HOME=/root", "LE_WORKING_DIR="+acmeHome)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
