# skypassd

The node agent for [SkyPass](https://skypass.cloud).

`skypassd` is a small Go daemon that runs on a managed Linux server. It keeps
the server registered with the control plane, reports its health, and carries
out the work the control plane sends its way. One static binary, no runtime
dependencies, managed by systemd.

## What it does

- Registers the host and stays in touch with a lightweight heartbeat.
- Reports a status snapshot: CPU, memory, disk, uptime, load.
- Runs the actions it's asked to perform.
- Updates itself in place when a newer build is available.

## Building

You need Go 1.22 or newer. From the repo root:

```bash
./build.sh v1.0.0
```

This cross-compiles static Linux binaries for `amd64` and `arm64` into `dist/`.
It works from Linux, macOS, or Windows (use Git Bash). Under the hood it's a
plain `CGO_ENABLED=0 GOOS=linux go build`, so you can run that directly if you'd
rather not use the script.

## Layout

```
cmd/skypassd      entry point and CLI
internal/agent    registration + heartbeat loop
internal/system   host status snapshot
internal/server   local HTTP control endpoint
internal/executor command dispatch
internal/updater  in-place self-update
internal/config   on-disk configuration
systemd/          the systemd unit
build.sh          cross-compile to dist/
```

## Managing a node

Run `skypassd` on the host with no arguments for an interactive menu: service
status, logs, restart, update, and so on. It's a thin wrapper around the usual
`systemctl` and `journalctl` commands so you don't have to remember them.

## Releases

Pushing a version tag builds the binaries and publishes them to a GitHub
Release:

```bash
git tag v1.0.0
git push origin v1.0.0
```

