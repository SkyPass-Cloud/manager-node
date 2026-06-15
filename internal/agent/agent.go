package agent

import (
	"context"
	"log"
	"time"

	"github.com/SkyPass-Cloud/manager-node/internal/api"
	"github.com/SkyPass-Cloud/manager-node/internal/config"
	"github.com/SkyPass-Cloud/manager-node/internal/executor"
	"github.com/SkyPass-Cloud/manager-node/internal/server"
	"github.com/SkyPass-Cloud/manager-node/internal/system"
)

// Agent ties the pieces together: it runs the local push server and the
// heartbeat/poll loop against the site.
type Agent struct {
	cfg     *config.Config
	client  *api.Client
	exec    *executor.Executor
	version string
}

// New builds an agent from a loaded config.
func New(cfg *config.Config, version string) *Agent {
	ex := executor.New(version)
	ex.SetACMEEmail(cfg.AcmeEmail)
	return &Agent{
		cfg:     cfg,
		client:  api.New(cfg.SiteBaseURL, cfg.Token, cfg.AgentID),
		exec:    ex,
		version: version,
	}
}

// Run starts the local server and the heartbeat loop, blocking until ctx is
// cancelled (e.g. systemd stop / SIGTERM).
func (a *Agent) Run(ctx context.Context) error {
	// Make sure the site knows about us before we start reporting.
	a.ensureRegistered(ctx)

	srv := server.New(a.cfg.ListenPort, a.cfg.Token, a.version, a.exec)
	srv.Start(ctx)

	interval := time.Duration(a.cfg.HeartbeatSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// First beat right away so the dashboard shows us connected immediately.
	a.beat(ctx)

	for {
		select {
		case <-ctx.Done():
			log.Printf("agent shutting down")
			return ctx.Err()
		case <-ticker.C:
			a.beat(ctx)
		}
	}
}

// ensureRegistered registers with the site if we have no NodeID yet, retrying
// a few times so a transient site outage at boot does not strand the node.
func (a *Agent) ensureRegistered(ctx context.Context) {
	if a.cfg.NodeID != "" {
		return
	}
	for attempt := 0; attempt < 5; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		resp, err := a.client.Register(rctx, api.RegisterRequest{
			AgentID:    a.cfg.AgentID,
			ListenPort: a.cfg.ListenPort,
			Version:    a.version,
		})
		cancel()
		if err == nil {
			a.cfg.NodeID = resp.NodeID
			if resp.Token != "" {
				a.cfg.Token = resp.Token
				a.client = api.New(a.cfg.SiteBaseURL, a.cfg.Token, a.cfg.AgentID)
			}
			if err := a.cfg.Save(); err != nil {
				log.Printf("warning: could not persist node id: %v", err)
			}
			log.Printf("registered with site as node %s", a.cfg.NodeID)
			return
		}
		log.Printf("register attempt %d failed: %v", attempt+1, err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Duration(attempt+1) * 3 * time.Second):
		}
	}
	log.Printf("could not register yet; will keep sending heartbeats and retry implicitly")
}

// beat sends one heartbeat and runs any commands the site returns.
func (a *Agent) beat(ctx context.Context) {
	hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	resp, err := a.client.Heartbeat(hctx, api.HeartbeatRequest{
		AgentID:    a.cfg.AgentID,
		NodeID:     a.cfg.NodeID,
		ListenPort: a.cfg.ListenPort,
		Status:     system.Collect(a.version),
	})
	if err != nil {
		log.Printf("heartbeat failed: %v", err)
		return
	}

	for _, cmd := range resp.Commands {
		a.handleCommand(ctx, cmd)
	}
}

// handleCommand executes one queued command and reports the result back.
func (a *Agent) handleCommand(ctx context.Context, cmd api.Command) {
	log.Printf("running command %s (%s)", cmd.ID, cmd.Type)
	res := a.exec.Run(ctx, cmd)

	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := a.client.ReportResult(rctx, api.CommandResult{
		AgentID:   a.cfg.AgentID,
		NodeID:    a.cfg.NodeID,
		CommandID: cmd.ID,
		OK:        res.OK,
		Output:    res.Output,
		Data:      res.Data,
		Error:     res.Err,
	}); err != nil {
		log.Printf("report result for %s failed: %v", cmd.ID, err)
	}
}
