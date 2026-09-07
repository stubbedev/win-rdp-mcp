# win-rdp-mcp

Control a Windows machine over the [Model Context Protocol](https://modelcontextprotocol.io/):
screenshots, synthetic mouse and keyboard, window management, PowerShell, file
transfer, processes, services, the registry, scheduled tasks, the event log and
network checks.

A Go port of [dddabtc/winremote-mcp](https://github.com/dddabtc/winremote-mcp) —
same tools, same tier model, same config file, as a single static binary with no
Python runtime to install on the target machine.

**Run it on the Windows machine you want to control.**

```powershell
# one-off, no install
npx -y @stubbedev/win-rdp-mcp

# or grab the binary from the releases page and run it
.\win-rdp-mcp.exe
```

That starts a Streamable HTTP MCP server on `http://127.0.0.1:8090/mcp`.

---

## Contents

- [Install](#install)
- [Connect a client](#connect-a-client)
- [Security model](#security-model)
- [Tools](#tools)
- [Configuration](#configuration)
- [Running as a service](#running-as-a-service)
- [Development](#development)
- [Differences from winremote-mcp](#differences-from-winremote-mcp)

---

## Install

### One-click, for Claude Desktop

Download `win-rdp-mcp_windows_amd64.mcpb` from the
[latest release](https://github.com/stubbedev/win-rdp-mcp/releases/latest) and
open it. Claude Desktop installs the bundled binary and wires up the stdio
transport itself — no JSON editing, no PATH.

### npm / npx

```powershell
npx -y @stubbedev/win-rdp-mcp            # always the newest release
npm install -g @stubbedev/win-rdp-mcp    # or keep it around
```

The wrapper downloads the prebuilt binary for your platform on first run and
then hands stdio straight to it, so it adds no per-message latency.

### Prebuilt binary

Grab `win-rdp-mcp_windows_amd64.exe` (or `arm64`, `386`) from the
[releases page](https://github.com/stubbedev/win-rdp-mcp/releases/latest).
It is a single static file with no dependencies.

### Go

```sh
go install github.com/stubbedev/win-rdp-mcp@latest
```

### Nix

```sh
nix run github:stubbedev/win-rdp-mcp
nix profile install github:stubbedev/win-rdp-mcp
```

Builds are pushed to a public binary cache. Add `--accept-flake-config` to use
it, or put these in your `nix.conf`:

```
extra-substituters = https://nix.stubbe.dev/default
extra-trusted-public-keys = default:9P4FePqHV1rGv5NDBun0GN26y83pcaaMr/NHZrxKaac=
```

> The server builds and its tests run on Linux and macOS too — that is how CI
> checks it — but the desktop tools only *do* anything on Windows. Everywhere
> else they return "this tool is only available when the server runs on Windows".

---

## Connect a client

### Claude Code / Claude Desktop — local, over stdio

```json
{
  "mcpServers": {
    "windows": {
      "command": "win-rdp-mcp",
      "args": ["-transport", "stdio"]
    }
  }
}
```

### Any client — remote, over HTTP

On the Windows machine:

```powershell
.\win-rdp-mcp.exe -host 0.0.0.0 -port 8090 -auth-key YOUR_SECRET_KEY
```

Then point the client at it:

```json
{
  "mcpServers": {
    "windows": {
      "type": "http",
      "url": "http://192.168.1.100:8090/mcp",
      "headers": { "Authorization": "Bearer YOUR_SECRET_KEY" }
    }
  }
}
```

### HTTPS

```powershell
# self-signed, for a LAN
openssl req -x509 -newkey rsa:4096 -keyout key.pem -out cert.pem -days 365 -nodes

.\win-rdp-mcp.exe -host 0.0.0.0 -port 8090 `
  -auth-key YOUR_SECRET_KEY `
  -ssl-certfile cert.pem -ssl-keyfile key.pem
```

### OAuth

For clients that speak OAuth rather than a static key. The client is
pre-provisioned: you configure the same ID and secret on both ends, and dynamic
registration stays disabled.

```powershell
.\win-rdp-mcp.exe -host 0.0.0.0 -port 8090 `
  -ssl-certfile cert.pem -ssl-keyfile key.pem `
  -oauth-client-id my-client -oauth-client-secret my-secret
```

The server publishes RFC 8414 metadata at
`/.well-known/oauth-authorization-server` and runs the authorization-code flow
with PKCE (S256 only). Redirect URIs must be loopback.

---

## Security model

This server hands an AI agent a keyboard, a mouse and — if you let it — a
PowerShell prompt on a real machine. The defaults reflect that.

### Tiers

Tools are grouped by how much damage they can do. **Tier 1 and 2 are on by
default; tier 3 is not.**

| Tier | What it does | Enable with |
|------|--------------|-------------|
| **1** — read-only | Screenshots, OCR, screen recording, window and process listing, registry reads, service and task listing, event log, network checks | on by default |
| **2** — desktop interaction | Click, Type, Move, Scroll, Shortcut, FocusWindow, MinimizeAll, Scrape, ReconnectSession | on by default (`-disable-tier2` to turn off) |
| **3** — destructive | Shell, App, PlaySound, FileRead/Write/Download/Upload, KillProcess, RegWrite, ServiceStart/Stop, TaskCreate/Delete, SetClipboard, LockScreen | `-enable-tier3` or `-enable-all` |

Or bypass tiers entirely:

```powershell
.\win-rdp-mcp.exe -tools Snapshot,Click,Type      # exactly these
.\win-rdp-mcp.exe -enable-all -exclude-tools Shell  # everything but one
```

An unknown name in either list is a hard error, not a silent no-op.

### Refusals

The server will not start in these configurations:

- **A non-loopback bind with no authentication.** Add `-auth-key`, configure
  OAuth, or bind to `127.0.0.1`. `-allow-insecure-remote` overrides this for a
  trusted lab LAN.
- **Tier 3 on a non-loopback bind with no authentication.** `-allow-insecure-remote`
  does *not* override this one. Shell plus an open port is pre-auth remote code
  execution.

### Other guards

- **IP allowlist** — `-ip-allowlist 192.168.1.0/24` restricts who may connect at
  all. `/health` stays reachable so a load-balancer probe needs no entry.
- **SSRF** — `Scrape` and `PlaySound` refuse non-public targets (loopback,
  private ranges, link-local, CGNAT) and refuse to follow redirects, so neither
  can be used to probe the Windows host's own network.
- **PowerShell injection** — every value interpolated into a PowerShell command
  is wrapped as a single-quoted string with its quotes doubled, and `TaskCreate`
  accepts only a fixed vocabulary of schedule types.
- **Killing by name is exact.** Upstream matched process names at a similarity
  threshold, which scores `notepad` against `notepad++` at 87 — high enough to
  kill software you never named. Here only the exact name matches, with or
  without `.exe`.

---

## Tools

45 tools. Everything a call returns is prefixed with `[task:<id>]`, which
`GetTaskStatus` and `CancelTask` take.

### Desktop

| Tool | What it does |
|------|--------------|
| `Snapshot` | Screenshot plus the window list and the foreground window's controls |
| `AnnotatedSnapshot` | The same, with numbered red boxes drawn on each control |
| `Click` | Click, double-click or hover at a coordinate |
| `Type` | Type text, optionally clicking first, clearing, or pressing Enter |
| `Scroll` | Scroll vertically or horizontally |
| `Move` | Move the pointer, or drag |
| `Shortcut` | A key chord, e.g. `ctrl+shift+esc` |
| `Wait` | Pause between UI actions |
| `OCR` | Read text off the screen or a region |
| `ScreenRecord` | Record up to 10s as an animated GIF |
| `LockScreen` | Lock the workstation |
| `ReconnectSession` | Attach a disconnected session to the console via `tscon` |

### Windows and apps

`FocusWindow`, `MinimizeAll`, `App` (launch / switch / resize),
`GetClipboard`, `SetClipboard`, `Notification`, `PlaySound`

### System

`Shell` (PowerShell), `ListProcesses`, `KillProcess`, `GetSystemInfo`,
`ServiceList`, `ServiceStart`, `ServiceStop`, `TaskList`, `TaskCreate`,
`TaskDelete`, `EventLog`, `RegRead`, `RegWrite`

### Files

`FileRead`, `FileWrite`, `FileList`, `FileSearch`, `FileDownload`, `FileUpload`

### Network

`Ping`, `PortCheck`, `NetConnections`, `Scrape`

### Tasks

`GetTaskStatus`, `GetRunningTasks`, `CancelTask`

### Concurrency

Tools are grouped by what they contend on, and each group has its own budget.
Desktop tools hold an **exclusive** lock — two synthetic clicks at once are two
clicks in the wrong places — while queries, file operations and network checks
run in parallel.

| Category | Concurrent |
|----------|-----------|
| desktop | 1 |
| shell | 3 |
| file | 5 |
| network | 5 |
| query | 10 |

A tool that waits more than 30s for its slot fails rather than hanging the
client.

---

## Configuration

Flags, environment and a TOML file all work. Precedence, lowest to highest:
**built-in default → config file → environment → command-line flag.**

The config file is looked up as `-config <path>`, then `./win-rdp-mcp.toml`,
then `~/.config/win-rdp-mcp/win-rdp-mcp.toml`. See
[`win-rdp-mcp.example.toml`](win-rdp-mcp.example.toml) for the annotated
version.

```toml
[server]
host = "0.0.0.0"
port = 8090
auth_key = "change-me"

[security]
ip_allowlist = ["192.168.1.0/24"]
enable_tier3 = true

[tools]
exclude = ["ScreenRecord"]
```

An unknown key in the file is an error — a typo must not silently leave the
server less locked down than you meant.

### Flags

```
-transport stdio|streamable-http   default streamable-http
-host, -port                       default 127.0.0.1:8090
-config <path>                     explicit config file
-auth-key <key>                    also WIN_RDP_MCP_AUTH_KEY
-allow-insecure-remote             non-loopback bind with no auth (dangerous)
-ssl-certfile, -ssl-keyfile        enable HTTPS
-oauth-client-id, -oauth-client-secret
                                   also WIN_RDP_MCP_OAUTH_CLIENT_ID / _SECRET
-enable-all, -enable-tier3, -disable-tier2
-tools, -exclude-tools             comma-separated
-ip-allowlist                      comma-separated IPs/CIDRs
-debug                             log every request
```

Subcommands: `install`, `uninstall`, `health`.

---

## Running as a service

```powershell
.\win-rdp-mcp.exe install      # scheduled task, starts at boot as you
.\win-rdp-mcp.exe uninstall
```

It registers as the logged-on user rather than SYSTEM on purpose: a SYSTEM
session has no desktop to screenshot.

### Screenshots with nobody logged in over RDP

When an RDP client disconnects, the session is left with no console to draw to
and screenshots come back black or fail. `ReconnectSession` runs `tscon` to
attach it back to the console; `Snapshot` already retries once behind it
automatically.

---

## Development

```sh
nix develop        # Go 1.27, gopls, staticcheck, just, node, zip
just               # list every recipe
```

| Recipe | What it does |
|--------|--------------|
| `just build` / `just build-windows` | build for this platform / cross-compile the Windows binary |
| `just check` | the full merge gate: gofmt, vet (both platforms), tests, both builds |
| `just test` / `just test-race` | tests |
| `just run` / `just run-http` | run over stdio / on loopback with every tool enabled |
| `just tools` | list the tools the server would expose for a given set of flags |
| `just smoke` | drive the real binary over stdio and assert `tools/list` |
| `just bundle` | pack and validate the `.mcpb` |
| `just nix-build` / `just nix-check` | build and check the flake |
| `just nix-vendor-hash` | recompute the flake's `vendorHash` locally |
| `just install-hooks` | enable the pre-commit gofmt + vet gate |
| `just release-preview` / `release-patch` / `release-minor` / `release-major` | cut a release |

### Layout

| File | Holds |
|------|-------|
| `tools.json` | every tool's JSON Schema, embedded into the binary |
| `tools.go` | registration, argument validation, dispatch |
| `desktop.go` | screenshots, input, OCR, recording, session reconnect |
| `system.go` | shell, processes, services, files, network, tasks |
| `platform_windows.go` | the Win32 layer: GDI capture, `SendInput`, window enumeration, clipboard |
| `platform_other.go` | stubs, so everything builds and tests off Windows |
| `tiers.go` | the tier definitions and selection logic |
| `security.go` | bind checks, IP allowlist, SSRF guard, auth middleware |
| `oauth.go` | the minimal authorization server |
| `tasks.go` | per-category concurrency and the task registry |

### CI

- **CI** — gofmt, vet (host and Windows cross-compile), race tests, both
  builds, `.mcpb` pack and validate, and a stdio smoke test; plus the full test
  suite and smoke on a real `windows-latest` runner.
- **Flake** — recomputes `vendorHash` on any Go change and commits it, builds
  the flake (which runs the tests), runs `nix flake check`, and pushes the
  closure to the binary cache.
- **Flake update** — weekly `nix flake update`, committed only if the flake
  still builds and its tests still pass.
- **Dependabot** — weekly Go and Actions updates, auto-merged once green.

Nothing about the flake needs hand-maintenance: the version comes from
`package.json`, `vendorHash` and `flake.lock` are maintained by CI.

---

## Differences from winremote-mcp

Same tool names, same tiers, same TOML shape. What changed:

- **A single static binary.** No Python, no pip, no `pywin32`/`pyautogui`/`Pillow`
  on the target machine. Screen capture, synthetic input, window enumeration and
  the clipboard are direct Win32 calls.
- **Killing a process by name is exact**, not fuzzy — see
  [Refusals](#security-model).
- **The SSRF guard also blocks CGNAT (`100.64.0.0/10`) and `0.0.0.0/8`.**
- **Config keys are validated.** An unknown key is an error rather than being
  ignored.
- **Env vars are `WIN_RDP_MCP_*`** rather than `WINREMOTE_*`, and the config
  file is `win-rdp-mcp.toml`.
- **OCR** uses `tesseract` when it is on PATH and the built-in Windows OCR
  engine otherwise — the same order, with no Python OCR package needed.

---

## License

MIT — see [LICENSE](LICENSE).
