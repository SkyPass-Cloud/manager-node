# SkyPass

[**skypass.cloud**](https://skypass.cloud) — pay-as-you-go VPS and VPN.

This is the node agent that handles things on SkyPass VPS instances.

## Layout

```
cmd/skypassd/main.go            CLI: install | run | status | update | uninstall | version | (no args = menu)
internal/config                 on-disk config (/etc/skypassd/config.json)
internal/cli                    interactive 'skypass-manager' menu (status/logs/update/restart)
internal/updater                self-update: download latest binary, atomic swap, restart
internal/system                 host status snapshot (cpu/mem/disk/uptime/load)
internal/portpick               picks a free port from the allowed ranges
internal/firewall               detects ufw/firewalld/iptables, opens/closes the port
internal/api                    site API client (register / heartbeat / result)
internal/server                 local HTTP server the site pushes commands to
internal/executor               dispatches command types (extension point)
internal/ssl                    acme.sh wrapper (installs acme.sh + deps, issues certs)
internal/agent                  ties it together: register + heartbeat loop
systemd/skypassd.service systemd unit
install.sh                      GitHub-based installer run over SSH (idempotent: re-run = update)
update.sh                       one-line updater: swaps binary only, never touches config
build.sh                        cross-compile to dist/
```

## Allowed listen ports

The local server only ever binds a port in these ranges (policy):
`19302-19309` and `27014-27050`. `install` picks a free one automatically.

## Managing the node (interactive)

On the VPS, run `skypass-manager` (or `skypassd` with no args) for a menu:
service status, live/recent logs, restart/stop/start, **update to latest**,
show config, and uninstall. It wraps `systemctl` and `journalctl -u skypassd`
so the operator never has to remember them. Logs are tagged `[node-manager]`.

### Updating

One line, no token, config never changes — works on any node OR handler:

```
curl -fsSL https://raw.githubusercontent.com/SkyPass-Cloud/manager-node/main/update.sh | bash
```

`update.sh` downloads the latest arch-matched binary, verifies it runs, swaps it
over `/usr/local/bin/skypassd`, and restarts the service. It only requires that a
config already exists; it never reads or rewrites `/etc/skypassd/config.json`
(token, port, agentId, nodeId, role all preserved). This also works on the OLD
binary, so it's the bootstrap to get nodes onto the new self-updating one.

Equivalent paths once the new binary is installed:
- `skypassd update` — self-update from the `binaryUrl` recorded in config.
- `skypass-manager` → menu option 7.
- Re-running `install.sh` (detects an existing install and preserves config).

## Auth

Install passes a `--token`. The agent stores it and sends it as
`Authorization: Bearer <token>` to the site, and requires the same token on its
own local `/v1/*` endpoints. The site may hand back a rotated per-node token in
the register response; the agent persists and uses it from then on.

## Build

Needs Go 1.22+. From this directory:

```bash
./build.sh v1.0.0      # produces dist/skypassd-linux-{amd64,arm64}
```

On Windows use Git Bash, or run the equivalent `go build` with
`GOOS=linux GOARCH=amd64 CGO_ENABLED=0`.


## Roles: node vs ssh-handler

The same binary runs in two roles, chosen with `--role` at install:

- `--role node` (default): manages the VPS it runs on (SSL + 3x-ui panel).
- `--role ssh-handler`: a dedicated server that accepts `ssh.install` jobs from
  the site and SSHes into *target* user VPSes to install the node agent on them.
  The site load-balances installs across all online handlers (least-loaded,
  per-handler `maxConcurrent` cap). Add a handler in Admin → Settings → "SSH
  handler", which mints a token; then install on the handler server:

```bash
curl -fsSL <INSTALL_SH_URL> | bash -s -- \
    --site https://api.skypass.cloud \
    --token <HANDLER_TOKEN> \
    --binary-url '<BINARY_URL>' \
    --role ssh-handler
```

A handler registers at `/api/handlers/register` and heartbeats its live job
load at `/api/handlers/heartbeat`.

## Site API the agent expects

A node (`--role node`) calls:

- `POST /api/nodes/register`        -> `{ nodeId, token? }`
- `POST /api/nodes/heartbeat`       -> `{ commands: [...] }`
- `POST /api/nodes/command-result`  -> 2xx

An ssh-handler (`--role ssh-handler`) calls:

- `POST /api/handlers/register`     -> `{ handlerId }`
- `POST /api/handlers/heartbeat`    -> 2xx (body reports `activeJobs`)

The site can also call the node directly:

- `GET  http://<vps-ip>:<port>/healthz`   (unauthenticated liveness)
- `GET  http://<vps-ip>:<port>/v1/status` (bearer auth)
- `POST http://<vps-ip>:<port>/v1/command` (bearer auth, body = one command)

## Command types (executor)

Implemented: `ping`, `status`, `shell`. Add new actions in
`internal/executor/executor.go` as we define what the node should do.
