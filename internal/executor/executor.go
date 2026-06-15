package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"

	"github.com/SkyPass-Cloud/manager-node/internal/api"
	"github.com/SkyPass-Cloud/manager-node/internal/ssl"
	"github.com/SkyPass-Cloud/manager-node/internal/system"
	"github.com/SkyPass-Cloud/manager-node/internal/xui"
)

// Executor runs commands the site sends. Each command Type maps to a host
// capability: SSL (acme.sh) or panel (3x-ui) management, plus diagnostics.
type Executor struct {
	version string
	// acmeEmail is used when registering with Let's Encrypt. Set from config.
	acmeEmail string
}

// New returns an executor.
func New(version string) *Executor {
	return &Executor{version: version}
}

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
		if _, err := xui.Install(ctx, p.Version); err != nil {
			return fail("%v", err)
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

	// ── Raw shell (privileged; only the authenticated site can send it) ──────
	case "shell":
		return e.runShell(ctx, cmd.Payload)

	default:
		return fail("unknown command type %q", cmd.Type)
	}
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
