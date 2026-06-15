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

// acme.sh is installed UNDER /etc/skypassd, NOT the default /root/.acme.sh.
// Reason: the systemd unit hardens the sandbox with ProtectHome=true /
// ProtectSystem=full, which makes /root a read-only tmpfs inside the service —
// so acme.sh's default install fails with "cannot create /root/.acme.sh:
// Read-only file system". /etc/skypassd is listed in the unit's ReadWritePaths,
// so installing acme.sh there works inside the sandbox without weakening it.
const (
	acmeHome  = "/etc/skypassd/acme"
	acmeBin   = "/etc/skypassd/acme/acme.sh"
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

// EnsureACME installs acme.sh if it is missing. Idempotent. After installing it
// VERIFIES the acme.sh binary actually exists, because the official installer can
// exit 0 without installing (missing deps, or a no-op invocation) — which later
// surfaces as "fork/exec /root/.acme.sh/acme.sh: no such file or directory".
func EnsureACME(ctx context.Context, accountEmail string) error {
	if _, err := os.Stat(acmeBin); err == nil {
		return nil
	}
	// acme.sh needs curl/wget plus socat for the standalone HTTP-01 challenge.
	// Install them best-effort so a minimal box still works.
	ensureDeps(ctx)

	if _, err := exec.LookPath("curl"); err != nil {
		if _, werr := exec.LookPath("wget"); werr != nil {
			return fmt.Errorf("curl or wget is required to install acme.sh")
		}
	}
	email := accountEmail
	if email == "" {
		email = "admin@" + hostnameOrLocal()
	}
	// Make sure the writable install dir exists (it lives under /etc/skypassd,
	// which the systemd sandbox grants RW via ReadWritePaths).
	if err := os.MkdirAll(acmeHome, 0o700); err != nil {
		return fmt.Errorf("create acme home %s: %w", acmeHome, err)
	}
	// Bootstrap acme.sh INTO our writable home. The get.acme.sh online installer
	// does NOT reliably accept --home through the pipe (that's the git/--install
	// path), but it DOES honour the LE_WORKING_DIR / LE_CONFIG_HOME env vars,
	// which runCtx exports. Without them acme.sh defaults to /root/.acme.sh, which
	// is a read-only tmpfs inside the hardened service (ProtectHome=true), causing
	// "cannot create /root/.acme.sh: Read-only file system". email= is the
	// installer's accepted key=value form; --nocron is honoured by get.acme.sh.
	script := fmt.Sprintf("curl -fsSL https://get.acme.sh | sh -s -- --nocron email=%s", shellQuote(email))
	if _, err := exec.LookPath("curl"); err != nil {
		script = fmt.Sprintf("wget -qO- https://get.acme.sh | sh -s -- --nocron email=%s", shellQuote(email))
	}
	out, err := runCtx(ctx, 3*time.Minute, "/bin/sh", "-c", script)
	if err != nil {
		return fmt.Errorf("install acme.sh: %v: %s", err, strings.TrimSpace(out))
	}
	// Verify the installer actually produced the binary. If not, the install was a
	// no-op (e.g. missing git/socat) — fail loudly with the installer output rather
	// than letting a later exec fail with a confusing "no such file or directory".
	if _, statErr := os.Stat(acmeBin); statErr != nil {
		return fmt.Errorf("acme.sh did not install to %s after bootstrap; installer output: %s",
			acmeBin, strings.TrimSpace(out))
	}
	// Use Let's Encrypt as the default CA.
	_, _ = runCtx(ctx, time.Minute, acmeBin, "--set-default-ca", "--server", "letsencrypt")
	return nil
}

// ensureDeps best-effort installs curl, socat and the CA store using whatever
// package manager the distro has. Errors are ignored: if a tool is already
// present or the box has no internet, EnsureACME's own checks will catch it.
func ensureDeps(ctx context.Context) {
	// Already have everything we need?
	_, curlErr := exec.LookPath("curl")
	_, socatErr := exec.LookPath("socat")
	if curlErr == nil && socatErr == nil {
		return
	}
	switch {
	case hasCmd("apt-get"):
		_, _ = runCtx(ctx, 2*time.Minute, "/bin/sh", "-c",
			"DEBIAN_FRONTEND=noninteractive apt-get update -y && DEBIAN_FRONTEND=noninteractive apt-get install -y curl socat ca-certificates")
	case hasCmd("dnf"):
		_, _ = runCtx(ctx, 2*time.Minute, "dnf", "install", "-y", "curl", "socat", "ca-certificates")
	case hasCmd("yum"):
		_, _ = runCtx(ctx, 2*time.Minute, "yum", "install", "-y", "curl", "socat", "ca-certificates")
	case hasCmd("apk"):
		_, _ = runCtx(ctx, 2*time.Minute, "apk", "add", "--no-cache", "curl", "socat", "ca-certificates", "openssl")
	}
}

func hasCmd(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
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
	// Point acme.sh at our writable home in every way it looks for it: HOME (the
	// bootstrap installer uses it), LE_WORKING_DIR and LE_CONFIG_HOME (the acme.sh
	// binary uses these). HOME=acmeHome — NOT /root — because /root is read-only
	// inside the systemd sandbox (ProtectHome/ProtectSystem).
	cmd.Env = append(os.Environ(),
		"HOME="+acmeHome,
		"LE_WORKING_DIR="+acmeHome,
		"LE_CONFIG_HOME="+acmeHome,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}
