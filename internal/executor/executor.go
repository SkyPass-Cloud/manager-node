package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SkyPass-Cloud/manager-node/internal/api"
	"github.com/SkyPass-Cloud/manager-node/internal/sshexec"
	"github.com/SkyPass-Cloud/manager-node/internal/ssl"
	"github.com/SkyPass-Cloud/manager-node/internal/system"
	"github.com/SkyPass-Cloud/manager-node/internal/xui"
)

// Executor runs commands the site sends. Each command Type maps to a host
// capability: SSL (acme.sh) or panel (3x-ui) management, plus diagnostics.
// In ssh-handler mode it also runs ssh.install jobs against target VPSes.
type Executor struct {
	version string
	// acmeEmail is used when registering with Let's Encrypt. Set from config.
	acmeEmail string
	// activeJobs counts in-flight ssh.install jobs so the handler can report its
	// live load to the site on each heartbeat (for load balancing).
	activeJobs int64
}

// New returns an executor.
func New(version string) *Executor {
	return &Executor{version: version}
}

// ActiveJobs returns the number of ssh.install jobs currently running.
func (e *Executor) ActiveJobs() int { return int(atomic.LoadInt64(&e.activeJobs)) }

// SetACMEEmail sets the email acme.sh registers with.
func (e *Executor) SetACMEEmail(email string) { e.acmeEmail = email }

// Result is the structured outcome of running one command. Data carries a
// typed JSON payload (e.g. cert paths, panel settings) the site can store.
type Result struct {
	OK     bool
	Output string
	Data   json.RawMessage
	Err    string
}

func ok(output string) Result          { return Result{OK: true, Output: output} }
func okData(v any) Result              { b, _ := json.Marshal(v); return Result{OK: true, Data: b} }
func fail(format string, a ...any) Result {
	return Result{OK: false, Err: fmt.Sprintf(format, a...)}
}

// Run dispatches a command by type. Unknown types are reported as an error so a
// newer site can queue commands an older agent does not yet understand.
func (e *Executor) Run(ctx context.Context, cmd api.Command) Result {
	switch cmd.Type {

	case "ping":
		return ok("pong")

	case "status":
		return okData(system.Collect(e.version))

	// ── SSL (acme.sh / Let's Encrypt) ───────────────────────────────────────
	case "ssl.check_dns":
		var p struct {
			Domain string `json:"domain"`
		}
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return fail("bad payload: %v", err)
		}
		return okData(ssl.CheckDNS(ctx, p.Domain))

	case "ssl.issue":
		var p struct {
			Domain string `json:"domain"`
		}
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return fail("bad payload: %v", err)
		}
		// Stop the panel briefly so acme.sh standalone can bind :80 if the panel
		// or anything else holds it. Best-effort; ignore errors.
		if xui.Installed() {
			_, _ = xui.Stop(ctx)
			defer func() { _, _ = xui.Start(context.Background()) }()
		}
		paths, err := ssl.Issue(ctx, p.Domain, e.acmeEmail)
		if err != nil {
			return fail("%v", err)
		}
		return okData(paths)

	case "ssl.paths":
		var p struct {
			Domain string `json:"domain"`
		}
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return fail("bad payload: %v", err)
		}
		return okData(ssl.Paths(p.Domain))

	case "ssl.remove":
		var p struct {
			Domain string `json:"domain"`
		}
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return fail("bad payload: %v", err)
		}
		if err := ssl.Remove(ctx, p.Domain); err != nil {
			return fail("%v", err)
		}
		return ok("removed")

	// ── Panel (3x-ui) ───────────────────────────────────────────────────────
	case "panel.install":
		var p struct {
			Version  string `json:"version"`
			Username string `json:"username"`
			Password string `json:"password"`
			Port     int    `json:"port"`
		}
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return fail("bad payload: %v", err)
		}
		if out, err := xui.Install(ctx, p.Version); err != nil {
			// Surface the installer's own output (tail) so the site/user sees WHY it
			// failed, not just "exit status 1".
			return Result{OK: false, Output: tail(out, 1500), Err: err.Error()}
		}
		if p.Username != "" && p.Password != "" {
			if out, err := xui.SetCredentials(ctx, p.Username, p.Password); err != nil {
				return fail("installed but setting credentials failed: %v: %s", err, out)
			}
		}
		if p.Port > 0 {
			if out, err := xui.SetPort(ctx, p.Port); err != nil {
				return fail("installed but setting port failed: %v: %s", err, out)
			}
		}
		s, err := xui.GetSettings(ctx)
		if err != nil {
			return fail("%v", err)
		}
		return okData(s)

	case "panel.uninstall":
		if _, err := xui.Uninstall(ctx); err != nil {
			return fail("%v", err)
		}
		return ok("uninstalled")

	case "panel.settings":
		s, err := xui.GetSettings(ctx)
		if err != nil {
			return fail("%v", err)
		}
		return okData(s)

	case "panel.status":
		out, err := xui.Status(ctx)
		if err != nil {
			return fail("%v", err)
		}
		return ok(out)

	case "panel.set_credentials":
		var p struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return fail("bad payload: %v", err)
		}
		if out, err := xui.SetCredentials(ctx, p.Username, p.Password); err != nil {
			return fail("%v: %s", err, out)
		}
		return ok("credentials updated")

	case "panel.set_port":
		var p struct {
			Port int `json:"port"`
		}
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return fail("bad payload: %v", err)
		}
		if out, err := xui.SetPort(ctx, p.Port); err != nil {
			return fail("%v: %s", err, out)
		}
		return ok("port updated")

	case "panel.set_cert":
		var p struct {
			CertFile string `json:"certFile"`
			KeyFile  string `json:"keyFile"`
			// If Domain is given instead of explicit files, resolve to the acme paths.
			Domain string `json:"domain"`
		}
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return fail("bad payload: %v", err)
		}
		if p.CertFile == "" && p.Domain != "" {
			cp := ssl.Paths(p.Domain)
			if !cp.Issued {
				return fail("no issued certificate for %s", p.Domain)
			}
			p.CertFile, p.KeyFile = cp.CertFile, cp.KeyFile
		}
		if out, err := xui.SetCert(ctx, p.CertFile, p.KeyFile); err != nil {
			return fail("%v: %s", err, out)
		}
		return ok("panel certificate set")

	case "panel.restart":
		if out, err := xui.Restart(ctx); err != nil {
			return fail("%v: %s", err, out)
		}
		return ok("restarted")

	// ── SSH handler: install the node agent onto a target VPS ────────────────
	case "ssh.install":
		return e.runSSHInstall(ctx, cmd.Payload)

	// ── Raw shell (privileged; only the authenticated site can send it) ──────
	case "shell":
		return e.runShell(ctx, cmd.Payload)

	default:
		return fail("unknown command type %q", cmd.Type)
	}
}

// runSSHInstall is the ssh-handler's core job: SSH into a target VPS and run the
// install command the site supplies. The site builds the exact command (it holds
// the per-node token and install URLs); the handler just executes it over SSH and
// returns the exit code + output so the site can decide success/retry.
func (e *Executor) runSSHInstall(ctx context.Context, raw json.RawMessage) Result {
	var p struct {
		Host           string `json:"host"`
		Port           int    `json:"port"`
		User           string `json:"user"`
		Password       string `json:"password"`
		Command        string `json:"command"`
		ConnectTimeout int    `json:"connectTimeoutSec"`
		RunTimeout     int    `json:"runTimeoutSec"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return fail("bad ssh.install payload: %v", err)
	}
	if p.Host == "" || p.Password == "" || p.Command == "" {
		return fail("ssh.install requires host, password and command")
	}
	if p.User == "" {
		p.User = "root"
	}
	connectTimeout := time.Duration(p.ConnectTimeout) * time.Second
	if connectTimeout <= 0 || connectTimeout > 2*time.Minute {
		connectTimeout = 30 * time.Second
	}
	runTimeout := time.Duration(p.RunTimeout) * time.Second
	if runTimeout <= 0 || runTimeout > 30*time.Minute {
		runTimeout = 15 * time.Minute
	}

	atomic.AddInt64(&e.activeJobs, 1)
	defer atomic.AddInt64(&e.activeJobs, -1)

	res, err := sshexec.Run(ctx, p.Host, p.Port, p.User, p.Password, p.Command, connectTimeout, runTimeout)
	if err != nil {
		// Transport/auth/timeout failure: no exit code. Surface the error so the
		// site treats it as transient (box not up yet) and retries.
		out := ""
		if res != nil {
			out = res.Stderr + res.Stdout
		}
		return Result{OK: false, Output: out, Err: err.Error()}
	}
	// We got an exit code. Return it as structured data so the site can branch on
	// code != 0 exactly as it does for its own direct-SSH path.
	return Result{OK: res.Code == 0, Output: res.Stdout, Data: mustJSON(res), Err: errIfNonZero(res)}
}

func errIfNonZero(r *sshexec.Result) string {
	if r.Code == 0 {
		return ""
	}
	tail := r.Stderr
	if tail == "" {
		tail = r.Stdout
	}
	if len(tail) > 500 {
		tail = tail[len(tail)-500:]
	}
	return fmt.Sprintf("install exited %d: %s", r.Code, tail)
}

func mustJSON(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

// tail returns the last n bytes of s, so long installer logs don't flood the UI
// while still showing the part that usually contains the error.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n:]
}

func (e *Executor) runShell(ctx context.Context, raw json.RawMessage) Result {
	var p struct {
		Command    string `json:"command"`
		TimeoutSec int    `json:"timeoutSec"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return fail("invalid shell payload: %v", err)
	}
	if p.Command == "" {
		return fail("empty command")
	}
	timeout := time.Duration(p.TimeoutSec) * time.Second
	if timeout <= 0 || timeout > 10*time.Minute {
		timeout = 60 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := exec.CommandContext(cctx, "/bin/sh", "-c", p.Command).CombinedOutput()
	if err != nil {
		return Result{OK: false, Output: string(out), Err: err.Error()}
	}
	return ok(string(out))
}
