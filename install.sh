#!/usr/bin/env bash
# skypassd installer.
#
# Usage (the website runs this over SSH on the user's VPS):
#   curl -fsSL https://raw.githubusercontent.com/SkyPass-Cloud/manager-node/main/install.sh \
#     | bash -s -- --site https://api.skypass.cloud --token <NODE_TOKEN> \
#         --binary-url 'https://github.com/SkyPass-Cloud/manager-node/releases/latest/download/skypassd-linux-{arch}'
#
# It downloads the matching binary from GitHub Releases, installs it to
# /usr/local/bin, writes config + opens the firewall (skypassd install),
# then installs and starts the systemd unit.
set -euo pipefail

BIN_DIR="/usr/local/bin"
BIN_PATH="${BIN_DIR}/skypassd"
CONFIG_DIR="/etc/skypassd"
UNIT_PATH="/etc/systemd/system/skypassd.service"

SITE=""
TOKEN=""
ACME_EMAIL=""
ROLE="node"
# BINARY_URL is the direct download URL for the prebuilt Linux binary. The
# website backend passes it (sourced from its own SKYPASS_NODE_BINARY_URL
# env var) so the GitHub location is configurable and never hardcoded here.
BINARY_URL="${SKYPASS_NODE_BINARY_URL:-}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --site)       SITE="$2"; shift 2 ;;
    --token)      TOKEN="$2"; shift 2 ;;
    --binary-url) BINARY_URL="$2"; shift 2 ;;
    --acme-email) ACME_EMAIL="$2"; shift 2 ;;
    --role)       ROLE="$2"; shift 2 ;;
    *) echo "unknown arg: $1" >&2; exit 2 ;;
  esac
done

if [[ -z "$SITE" || -z "$TOKEN" ]]; then
  echo "error: --site and --token are required" >&2
  exit 2
fi
if [[ -z "$BINARY_URL" ]]; then
  echo "error: --binary-url (or SKYPASS_NODE_BINARY_URL) is required" >&2
  exit 2
fi

if [[ "$(id -u)" -ne 0 ]]; then
  echo "error: must run as root" >&2
  exit 1
fi

# The backend may template {arch} into the URL so one env var serves both
# architectures. If present, substitute it; otherwise use the URL verbatim.
case "$(uname -m)" in
  x86_64|amd64) ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "unsupported arch: $(uname -m)" >&2; exit 1 ;;
esac
URL="${BINARY_URL//\{arch\}/$ARCH}"

# Install runtime deps NOW, while we are real root outside the systemd sandbox.
# acme.sh needs curl + socat (standalone HTTP-01). The service later runs under
# ProtectSystem=full, where /usr is read-only and apt/dnf CANNOT install — so
# this must happen here, not at runtime. Best-effort: skip if already present.
if ! command -v socat >/dev/null 2>&1 || ! command -v curl >/dev/null 2>&1; then
  echo "==> installing dependencies (curl, socat)"
  if command -v apt-get >/dev/null 2>&1; then
    DEBIAN_FRONTEND=noninteractive apt-get update -y || true
    DEBIAN_FRONTEND=noninteractive apt-get install -y curl socat ca-certificates || true
  elif command -v dnf >/dev/null 2>&1; then
    dnf install -y curl socat ca-certificates || true
  elif command -v yum >/dev/null 2>&1; then
    yum install -y curl socat ca-certificates || true
  elif command -v apk >/dev/null 2>&1; then
    apk add --no-cache curl socat ca-certificates openssl || true
  fi
fi

echo "==> downloading binary from ${URL}"
TMP="$(mktemp)"
if command -v curl >/dev/null 2>&1; then
  curl -fsSL "$URL" -o "$TMP"
elif command -v wget >/dev/null 2>&1; then
  wget -qO "$TMP" "$URL"
else
  echo "error: need curl or wget" >&2
  exit 1
fi

# Re-run detection: if config already exists this is an UPDATE, not a fresh
# install. The install subcommand below is idempotent and preserves the existing
# port / agentId / nodeId, so re-running this script simply swaps the binary and
# restarts — exactly the "run again to update" behaviour we want.
if [[ -f "${CONFIG_DIR}/config.json" ]]; then
  echo "==> existing install found — updating in place (config preserved)"
fi

echo "==> installing binary to ${BIN_PATH}"
install -m 0755 "$TMP" "$BIN_PATH"
rm -f "$TMP"

# Friendly alias: 'skypass-manager' opens the interactive menu.
ln -sf "$BIN_PATH" "${BIN_DIR}/skypass-manager"

echo "==> writing config and opening firewall"
mkdir -p "$CONFIG_DIR"
chmod 700 "$CONFIG_DIR"
# Pass --binary-url so the agent records it for self-update (skypassd update).
INSTALL_ARGS=(--site "$SITE" --token "$TOKEN" --role "$ROLE" --binary-url "$BINARY_URL")
[[ -n "$ACME_EMAIL" ]] && INSTALL_ARGS+=(--acme-email "$ACME_EMAIL")
"$BIN_PATH" install "${INSTALL_ARGS[@]}"

echo "==> installing systemd unit"
cat > "$UNIT_PATH" <<'UNIT'
[Unit]
Description=SkyPass node agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/skypassd run --config /etc/skypassd/config.json
Restart=always
RestartSec=5
User=root
ProtectSystem=full
ProtectHome=true
ReadWritePaths=/etc/skypassd

[Install]
WantedBy=multi-user.target
UNIT

systemctl daemon-reload
systemctl enable skypassd
systemctl restart skypassd

echo "==> done. status:"
systemctl --no-pager status skypassd || true

echo
echo "Manage this node anytime with:  skypass-manager"
