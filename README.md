# SkyPass

[**skypass.cloud**](https://skypass.cloud) — pay-as-you-go VPS and VPN.

This is the node agent that handles things on SkyPass VPS instances.

## Layout

```
cmd/skypassd/main.go            CLI: install | run | status | uninstall | version
internal/config                 on-disk config (/etc/skypassd/config.json)
internal/system                 host status snapshot (cpu/mem/disk/uptime/load)
internal/portpick               picks a free port from the allowed ranges
internal/firewall               detects ufw/firewalld/iptables, opens/closes the port
internal/api                    site API client (register / heartbeat / result)
internal/server                 local HTTP server the site pushes commands to
internal/executor               dispatches command types (extension point)
internal/agent                  ties it together: register + heartbeat loop
systemd/skypassd.service systemd unit
install.sh                      GitHub-based installer run over SSH
build.sh                        cross-compile to dist/
```

## Allowed listen ports

The local server only ever binds a port in these ranges (policy):
`19302-19309` and `27014-27050`. `install` picks a free one automatically.

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

## Publishing the binary

`git tag v1.0.0 && git push origin v1.0.0` triggers GitHub Actions, which builds
both binaries and attaches them to a Release at:

```
https://github.com/SkyPass-Cloud/manager-node/releases/latest/download/skypassd-linux-{arch}
```

## Install on a VPS (what the site does over SSH)

The backend runs this automatically (it sources `--binary-url` from its own
`SKYPASS_NODE_BINARY_URL` env). To install by hand:

```bash
curl -fsSL https://raw.githubusercontent.com/SkyPass-Cloud/manager-node/main/install.sh \
  | bash -s -- \
      --site https://api.skypass.cloud \
      --token <PER_NODE_TOKEN> \
      --binary-url 'https://github.com/SkyPass-Cloud/manager-node/releases/latest/download/skypassd-linux-{arch}'
```

This downloads the binary, installs to `/usr/local/bin/skypassd`, writes
`/etc/skypassd/config.json`, opens the firewall port, and enables the
systemd service.

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
