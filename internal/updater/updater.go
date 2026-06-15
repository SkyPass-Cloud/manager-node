// Package updater self-updates the skypassd binary in place from the same URL it
// was installed from. It downloads the arch-matched binary to a temp file, makes
// it executable, atomically renames it over the running binary (allowed on Linux
// even while it executes), then restarts the systemd service so the new code runs.
package updater

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// selfPath is where the installed binary lives. install.sh puts it here.
const selfPath = "/usr/local/bin/skypassd"

// SelfUpdate downloads the latest binary from binaryURL (with {arch} substituted),
// replaces the on-disk binary and restarts serviceName. It returns the version
// string reported by the freshly downloaded binary.
func SelfUpdate(ctx context.Context, binaryURL, serviceName string) (string, error) {
	url := substituteArch(binaryURL)
	if url == "" {
		return "", fmt.Errorf("empty binary URL")
	}

	tmp, err := download(ctx, url)
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp)

	// Stage next to the target FIRST, before running it. /tmp is often mounted
	// noexec, so we must verify the binary from a normally-executable location
	// (the same dir as the live binary). Rename within that dir is then atomic.
	staged := selfPath + ".new"
	if err := copyFile(tmp, staged); err != nil {
		return "", fmt.Errorf("stage new binary: %w", err)
	}
	if err := os.Chmod(staged, 0o755); err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("chmod staged binary: %w", err)
	}

	// Sanity-check the staged binary actually runs and reports a version before we
	// overwrite the working binary. Guards against a truncated/HTML error body.
	newVer, err := binVersion(ctx, staged)
	if err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("downloaded binary is not runnable: %w", err)
	}

	// Atomic replace within the same filesystem.
	if err := os.Rename(staged, selfPath); err != nil {
		os.Remove(staged)
		return "", fmt.Errorf("replace binary: %w", err)
	}

	// Restart the service so the new binary is the running process. Best-effort:
	// if systemd is absent the operator can restart manually.
	restartCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_ = exec.CommandContext(restartCtx, "systemctl", "restart", serviceName).Run()

	return newVer, nil
}

func substituteArch(url string) string {
	arch := "amd64"
	switch runtime.GOARCH {
	case "arm64":
		arch = "arm64"
	case "amd64":
		arch = "amd64"
	}
	return strings.ReplaceAll(url, "{arch}", arch)
}

func download(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	client := &http.Client{Timeout: 3 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("download %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}

	f, err := os.CreateTemp("", "skypassd-update-*")
	if err != nil {
		return "", err
	}
	tmp := f.Name()
	n, err := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("write download: %w", err)
	}
	if closeErr != nil {
		os.Remove(tmp)
		return "", closeErr
	}
	if n < 1024 {
		os.Remove(tmp)
		return "", fmt.Errorf("download too small (%d bytes); not a valid binary", n)
	}
	return tmp, nil
}

func binVersion(ctx context.Context, path string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(cctx, path, "version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	// `skypassd version` prints "skypassd <ver>".
	fields := strings.Fields(strings.TrimSpace(string(out)))
	if len(fields) >= 2 {
		return fields[len(fields)-1], nil
	}
	return strings.TrimSpace(string(out)), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
