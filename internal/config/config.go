package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// DefaultPath is where the agent stores its config on the VPS.
const DefaultPath = "/etc/skypassd/config.json"

// Config is the on-disk state of the agent. It is written once at install
// time (bootstrap) and then updated as the node registers and rotates secrets.
type Config struct {
	// Role selects what this install does. "node" (default, empty) = manage the
	// VPS it runs on. "ssh-handler" = accept install jobs from the site and SSH
	// into *other* user VPSes to install the node agent on them.
	Role string `json:"role,omitempty"`

	// SiteBaseURL is the root of the website API, e.g. https://panel.example.com
	SiteBaseURL string `json:"siteBaseUrl"`

	// Token is the long-lived auth token used to talk to the site. It is the
	// bootstrap token at first boot and may be rotated to a per-node secret
	// after the node registers.
	Token string `json:"token"`

	// NodeID is assigned by the site after the first successful registration.
	NodeID string `json:"nodeId"`

	// AgentID is a stable local identifier generated on first run. It lets the
	// site recognise the same install across token rotations / reinstalls.
	AgentID string `json:"agentId"`

	// ListenPort is the port the local HTTP server binds to so the site can
	// push commands directly to the node. Chosen from the allowed ranges.
	ListenPort int `json:"listenPort"`

	// HeartbeatSeconds controls how often the agent reports status / polls.
	HeartbeatSeconds int `json:"heartbeatSeconds"`

	// AcmeEmail is the contact email acme.sh / Let's Encrypt registers with.
	AcmeEmail string `json:"acmeEmail"`

	// BinaryURL is the download URL the agent was installed from (may contain a
	// literal {arch} token). Stored so `skypassd update` can self-update without
	// the operator re-supplying it. Set by install.sh / the install command.
	BinaryURL string `json:"binaryUrl,omitempty"`

	path string     // not serialised: where this config was loaded from
	mu   sync.Mutex // guards Save against concurrent writers
}

// New returns an empty config bound to the given path.
func New(path string) *Config {
	if path == "" {
		path = DefaultPath
	}
	return &Config{path: path, HeartbeatSeconds: 30}
}

// Load reads the config from disk. If the file does not exist it returns a
// fresh config bound to that path with ErrNotFound so callers can decide to
// run the bootstrap flow.
func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			c := New(path)
			return c, ErrNotFound
		}
		return nil, fmt.Errorf("read config: %w", err)
	}
	c := New(path)
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.path = path
	if c.HeartbeatSeconds <= 0 {
		c.HeartbeatSeconds = 30
	}
	return c, nil
}

// ErrNotFound is returned by Load when no config file exists yet.
var ErrNotFound = errors.New("config not found")

// Save atomically writes the config to disk with 0600 perms (it holds a token).
func (c *Config) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.path == "" {
		c.path = DefaultPath
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return fmt.Errorf("mkdir config dir: %w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := os.Rename(tmp, c.path); err != nil {
		return fmt.Errorf("rename config: %w", err)
	}
	return nil
}

// Path returns the file path this config is bound to.
func (c *Config) Path() string { return c.path }

// EnsureAgentID generates a random agent id if one is not set yet. Returns true
// if a new id was generated (caller should Save).
func (c *Config) EnsureAgentID() bool {
	if c.AgentID != "" {
		return false
	}
	c.AgentID = randomHex(16)
	return true
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// rand should never fail; fall back to a fixed-length zero string only
		// to avoid panicking the agent at startup.
		return "0000000000000000"
	}
	return hex.EncodeToString(b)
}
