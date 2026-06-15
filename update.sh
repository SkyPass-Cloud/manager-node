#!/usr/bin/env bash
# skypassd updater — swaps the binary ONLY. Never touches the config.
#
# This is the one-liner an operator runs on any node OR handler to update it to
# the latest version WITHOUT re-entering the token or any other setting:
#
#   curl -fsSL https://raw.githubusercontent.com/SkyPass-Cloud/manager-node/main/update.sh | bash
#
# It downloads the latest arch-matched binary, atomically replaces
# /usr/local/bin/skypassd, and restarts the service. /etc/skypassd/config.json
# (token, port, agentId, nodeId, role, ...) is left completely untouched.
#
# Works on the OLD binary too (which has no `skypassd update` subcommand), so it
# is the bootstrap path to get nodes onto the new self-updating binary.
set -euo pipefail

BIN_PATH="/usr/local/bin/skypassd"
CONFIG="/etc/skypassd/config.json"
SERVICE="skypassd"

# Default release URL. Override by exporting SKYPASS_NODE_BINARY_URL before
# running, or by passing it as the first argument.
BINARY_URL="${1:-${SKYPASS_NODE_BINARY_URL:-https://github.com/SkyPass-Cloud/manager-node/releases/latest/download/skypassd-linux-{arch}}}"

if [[ "$(id -u)" -ne 0 ]]; then
  echo "error: must run as root" >&2
  exit 1
fi

if [[ ! -f "$CONFIG" ]]; then
  echo "error: no config at $CONFIG — this box is not installed. Use install.sh instead." >&2
  exit 1
fi

case "$(uname -m)" in
  x86_64|amd64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac
URL="${BINARY_URL//\{arch\}/$ARCH}"

echo "==> downloading latest binary from ${URL}"
TMP="$(mktemp)"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$URL" -o "$TMP"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$TMP" "$URL"
else
  echo "error: need curl or wget" >&2
  exit 1
fi

# Sanity check: must be a non-trivial file before we trust it.
if [[ "$(stat -c%s "$TMP" 2>/dev/null || echo 0)" -lt 1024 ]]; then
  echo "error: download too small; not a valid binary" >&2
  rm -f "$TMP"
  exit 1
fi
chmod 0755 "$TMP"

# Verify it runs and prints a version before overwriting the live binary.
if ! "$TMP" version >/dev/null 2>&1; then
  echo "error: downloaded file is not a runnable skypassd binary" >&2
  rm -f "$TMP"
  exit 1
fi
NEWVER="$("$TMP" version 2>/dev/null | awk '{print $NF}')"

echo "==> installing ${NEWVER:-new version} to ${BIN_PATH} (config untouched)"
# install(1) replaces the file atomically and preserves nothing of the config.
install -m 0755 "$TMP" "$BIN_PATH"
rm -f "$TMP"

# Keep the friendly alias in sync.
ln -sf "$BIN_PATH" /usr/local/bin/skypass-manager

echo "==> restarting ${SERVICE}"
systemctl restart "$SERVICE" || true

echo "==> done. status:"
systemctl --no-pager status "$SERVICE" || true
echo
echo "Updated to ${NEWVER:-latest}. Config left unchanged."
