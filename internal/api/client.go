package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/SkyPass-Cloud/manager-node/internal/system"
)

// Client talks to the website API. All calls authenticate with the node token
// via the Authorization: Bearer header.
type Client struct {
	baseURL string
	token   string
	agentID string
	http    *http.Client
}

// New builds an API client. baseURL is the site root (no trailing slash needed).
func New(baseURL, token, agentID string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		agentID: agentID,
		http:    &http.Client{Timeout: 20 * time.Second},
	}
}

// RegisterRequest is sent once when the node first comes online.
type RegisterRequest struct {
	AgentID    string `json:"agentId"`
	ListenPort int    `json:"listenPort"`
	PublicIP   string `json:"publicIp,omitempty"`
	Version    string `json:"version"`
}

// RegisterResponse carries the node identity the site assigns.
type RegisterResponse struct {
	NodeID string `json:"nodeId"`
	// Token, if non-empty, is a rotated per-node secret that replaces the
	// bootstrap token. The agent persists it and uses it from then on.
	Token string `json:"token,omitempty"`
}

// Register announces this node to the site. Call after install / first boot.
func (c *Client) Register(ctx context.Context, req RegisterRequest) (*RegisterResponse, error) {
	var resp RegisterResponse
	if err := c.do(ctx, http.MethodPost, "/api/nodes/register", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// HeartbeatRequest pushes the latest status and the node's reachable address.
type HeartbeatRequest struct {
	AgentID    string        `json:"agentId"`
	NodeID     string        `json:"nodeId"`
	ListenPort int           `json:"listenPort"`
	Status     system.Status `json:"status"`
}

// Command is a unit of work the site wants the node to perform. The Payload is
// left opaque here; the executor package decides how to interpret each Type.
type Command struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// HeartbeatResponse returns any pending commands queued for this node, so a
// single round trip both reports status and pulls work (NAT-friendly).
type HeartbeatResponse struct {
	Commands []Command `json:"commands"`
}

// Heartbeat reports status and pulls any queued commands.
func (c *Client) Heartbeat(ctx context.Context, req HeartbeatRequest) (*HeartbeatResponse, error) {
	var resp HeartbeatResponse
	if err := c.do(ctx, http.MethodPost, "/api/nodes/heartbeat", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// CommandResult reports the outcome of a command back to the site. Data carries
// a typed JSON payload (cert paths, panel settings, ...) when the command
// produced structured output.
type CommandResult struct {
	AgentID   string          `json:"agentId"`
	NodeID    string          `json:"nodeId"`
	CommandID string          `json:"commandId"`
	OK        bool            `json:"ok"`
	Output    string          `json:"output,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Error     string          `json:"error,omitempty"`
}

// ReportResult sends a command's result to the site.
func (c *Client) ReportResult(ctx context.Context, res CommandResult) error {
	return c.do(ctx, http.MethodPost, "/api/nodes/command-result", res, nil)
}

// do performs an authenticated JSON request. If out is non-nil the response
// body is decoded into it. Non-2xx responses become errors.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("X-Agent-Id", c.agentID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(rb)))
	}
	if out != nil && len(rb) > 0 {
		if err := json.Unmarshal(rb, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}
