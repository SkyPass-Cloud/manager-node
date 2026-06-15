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

// isHandler reports whether this install runs as an ssh-handler.
func (a *Agent) isHandler() bool { return a.cfg.Role == "ssh-handler" }

// Run starts the local server and the heartbeat loop, blocking until ctx is
// cancelled (e.g. systemd stop / SIGTERM). Both roles run the local HTTP server
// (a node receives panel/ssl commands; a handler receives ssh.install jobs);
// they differ only in which register/heartbeat endpoints they call.
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
// a few times so a transient site outage at boot does not strand the agent.
// Handlers register on the handler endpoint and store the id in NodeID too.
func (a *Agent) ensureRegistered(ctx context.Context) {
	if a.cfg.NodeID != "" {
		return
	}
	for attempt := 0; attempt < 5; attempt++ {
		rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		var id, rotated string
		var err error
		if a.isHandler() {
			var resp *api.HandlerRegisterResponse
			resp, err = a.client.RegisterHandler(rctx, api.HandlerRegisterRequest{
				AgentID:    a.cfg.AgentID,
				ListenPort: a.cfg.ListenPort,
				Version:    a.version,
			})
			if resp != nil {
				id = resp.HandlerID
			}
		} else {
			var resp *api.RegisterResponse
			resp, err = a.client.Register(rctx, api.RegisterRequest{
				AgentID:    a.cfg.AgentID,
				ListenPort: a.cfg.ListenPort,
				Version:    a.version,
			})
			if resp != nil {
				id, rotated = resp.NodeID, resp.Token
			}
		}
		cancel()
		if err == nil {
			a.cfg.NodeID = id
			if rotated != "" {
				a.cfg.Token = rotated
				a.client = api.New(a.cfg.SiteBaseURL, a.cfg.Token, a.cfg.AgentID)
			}
			if err := a.cfg.Save(); err != nil {
				log.Printf("warning: could not persist id: %v", err)
			}
			log.Printf("registered with site as %s %s", a.roleName(), a.cfg.NodeID)
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

func (a *Agent) roleName() string {
	if a.isHandler() {
		return "ssh-handler"
	}
	return "node"
}

// beat sends one heartbeat. For a node it also runs any commands the site
// returns; a handler reports its live job load and receives jobs via the local
// HTTP server (push), so it pulls no commands here.
func (a *Agent) beat(ctx context.Context) {
	hctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	if a.isHandler() {
		if err := a.client.HandlerHeartbeat(hctx, api.HandlerHeartbeatRequest{
			AgentID:    a.cfg.AgentID,
			HandlerID:  a.cfg.NodeID,
			ListenPort: a.cfg.ListenPort,
			ActiveJobs: a.exec.ActiveJobs(),
			Status:     system.Collect(a.version),
		}); err != nil {
			log.Printf("handler heartbeat failed: %v", err)
		}
		return
	}

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
