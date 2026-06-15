// Package sshexec lets an ssh-handler connect to a target VPS over SSH
// (password auth) and run the node install command on it. The handler does the
// SSH work so the website server does not have to.
package sshexec

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Result is the outcome of one remote command.
type Result struct {
	Code   int    `json:"code"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
}

// Run dials host:port with password auth as user, runs command, and returns its
// combined exit code and streams. The host key is not verified: targets are
// freshly-provisioned boxes whose key changes per rebuild (same trust model the
// website used when it SSHed directly). connectTimeout bounds the dial+auth;
// runTimeout bounds command execution.
func Run(ctx context.Context, host string, port int, user, password, command string,
	connectTimeout, runTimeout time.Duration) (*Result, error) {

	if port <= 0 {
		port = 22
	}
	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.Password(password)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(), //nolint:gosec // fresh boxes, rotating keys
		Timeout:         connectTimeout,
	}

	addr := net.JoinHostPort(host, fmt.Sprintf("%d", port))
	d := net.Dialer{Timeout: connectTimeout}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}

	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake/auth %s: %w", addr, err)
	}
	client := ssh.NewClient(sshConn, chans, reqs)
	defer client.Close()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new session: %w", err)
	}
	defer session.Close()

	var stdout, stderr strings.Builder
	session.Stdout = &stdout
	session.Stderr = &stderr

	// Enforce the run timeout by closing the session if the command overruns.
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()

	timer := time.NewTimer(runTimeout)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		_ = session.Signal(ssh.SIGKILL)
		return nil, ctx.Err()
	case <-timer.C:
		_ = session.Signal(ssh.SIGKILL)
		return &Result{Code: -1, Stdout: stdout.String(), Stderr: stderr.String()},
			fmt.Errorf("command timed out after %s", runTimeout)
	case runErr := <-done:
		res := &Result{Stdout: stdout.String(), Stderr: stderr.String()}
		if runErr == nil {
			res.Code = 0
			return res, nil
		}
		// A non-zero exit is reported via *ssh.ExitError, not a transport failure.
		if ee, ok := runErr.(*ssh.ExitError); ok {
			res.Code = ee.ExitStatus()
			return res, nil
		}
		res.Code = -1
		return res, runErr
	}
}
