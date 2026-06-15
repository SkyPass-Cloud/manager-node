package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/SkyPass-Cloud/manager-node/internal/api"
	"github.com/SkyPass-Cloud/manager-node/internal/executor"
	"github.com/SkyPass-Cloud/manager-node/internal/system"
)

// Server is the local HTTP listener the site can push commands to directly,
// in addition to the heartbeat poll. It binds the agent's chosen port.
type Server struct {
	port    int
	token   string
	version string
	exec    *executor.Executor
	srv     *http.Server
}

// New builds the local server. token is compared against the bearer token the
// site presents, so only the site can drive the node.
func New(port int, token, version string, ex *executor.Executor) *Server {
	return &Server{port: port, token: token, version: version, exec: ex}
}

// Start begins listening. It returns immediately; the server runs in a
// goroutine until ctx is cancelled.
func (s *Server) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/v1/status", s.auth(s.handleStatus))
	mux.HandleFunc("/v1/command", s.auth(s.handleCommand))

	s.srv = &http.Server{
		Addr:              fmt.Sprintf(":%d", s.port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
	}()

	go func() {
		log.Printf("local server listening on :%d", s.port)
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("local server error: %v", err)
		}
	}()
}

// auth wraps a handler with bearer-token checking using a constant-time compare.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok := strings.TrimPrefix(h, "Bearer ")
		if tok == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(s.token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleHealth is unauthenticated and used by the site / load checks to confirm
// the node's port is reachable. It deliberately leaks nothing sensitive.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "version": s.version})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, system.Collect(s.version))
}

// handleCommand runs a single pushed command synchronously and returns its
// result. The site can use this for low-latency actions instead of waiting for
// the next heartbeat poll.
func (s *Server) handleCommand(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var cmd api.Command
	if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	res := s.exec.Run(r.Context(), cmd)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     res.OK,
		"output": res.Output,
		"data":   res.Data,
		"error":  res.Err,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
