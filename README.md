# win-rdp-mcp

Control a Windows machine over RDP, exposed as [Model Context Protocol](https://modelcontextprotocol.io/)
tools — screenshots, mouse and keyboard, PowerShell, files, processes, services,
the registry, scheduled tasks, the event log and network checks.

**You give it only RDP credentials.** It connects, drives the desktop directly,
pushes a small agent into the session for the system internals, and exposes the
whole thing to your AI client. Nothing to pre-install on the target — no SMB, no
WinRM, no open port beyond RDP.

Originally a Go port of [dddabtc/winremote-mcp](https://github.com/dddabtc/winremote-mcp);
it has since grown the RDP controller so the target needs no agent installed.
It is pure Go: one static binary per side, with no external programs needed on
either machine.

---

## How it works

Two halves, one binary:

```
  your machine (any OS)                         the Windows target
┌──────────────────────────────┐              ┌──────────────────────────┐
│ win-rdp-mcp control          │              │                          │
│  the MCP server your AI uses │              │                          │
│                              │── RDP/3389 ──▶│  live desktop session    │
│  • a built-in RDP client     │   screenshots │  (logged in for you)     │
│    holds the session         │   + input     │                          │
│  • screenshots + mouse/keys  │              │                          │
│    driven in-process         │              │                          │
│                              │── drive ─────▶│  win-rdp-mcp.exe (agent) │
│  • pushes the agent over the │   redirection │   pushed + run in-session│
│    redirected drive, talks   │◀── virtual ──▶│   window tree, shell,    │
│    to it over a virtual chan.│    channel    │   registry, files, ...   │
└──────────────────────────────┘              └──────────────────────────┘
```

- The **desktop tier** — `Snapshot`, `Click`, `Type`, `Move`, `Scroll`,
  `Shortcut` — is driven straight over RDP with **no footprint on the target**.
- The **system tier** — window enumeration with coordinates, `Shell`, files,
  registry, services, tasks, event log, OCR — is served by an agent the
  controller **pushes into the RDP session over drive redirection** and talks to
  over an RDP dynamic virtual channel. Still nothing but RDP on the wire.

The agent copies itself out of the redirected drive into
`%LOCALAPPDATA%\win-rdp-mcp` and runs from there with no console window, so it
survives RDP reconnects. Calls are pushed over the channel (a round trip is a few
milliseconds) and can run concurrently. If the agent dies, the next call
relaunches it; an agent left over from an earlier controller is told to exit.

The agent can also run standalone on the target (stdio or HTTP) if you'd rather
install it — see [Running the agent directly](#running-the-agent-directly).

---

## Quickstart

On your machine (Linux, macOS or Windows), describe the target once in
`~/.config/win-rdp-mcp/win-rdp-mcp.toml`:

```toml
[target]
host = "192.168.1.50"
user = "administrator"
password = "..."        # or pass_file = "~/.config/win-rdp-mcp/target.pass"
```

```sh
chmod 600 ~/.config/win-rdp-mcp/win-rdp-mcp.toml   # a readable password is refused
npx -y @stubbedev/win-rdp-mcp control
```

Or skip the file and use flags:

```sh
export WIN_RDP_TARGET_PASS='...'   # kept out of your shell history and argv
npx -y @stubbedev/win-rdp-mcp control --target 192.168.1.50 --user administrator
```

Either way, that starts an MCP server on stdio. Point your client at it (below). The RDP
session opens on the first tool call, and reopens on its own if it drops —
Windows gives a user one session, so another client logging in as the same user
takes it. The first system-tool call waits a few seconds while the agent
bootstraps; the desktop tools work immediately.

### Configuration

Every controller setting can come from a flag, from the config file, or both. A
flag always wins over the file. The file is the first of:

1. the path given to `--config` / `-c`
2. `win-rdp-mcp.toml` in the current directory
3. `~/.config/win-rdp-mcp/win-rdp-mcp.toml` (on Windows,
   `%USERPROFILE%\.config\win-rdp-mcp\win-rdp-mcp.toml`)

The controller reads the `[target]` table for the connection, and `[security]`
and `[tools]` for which tools to expose:

```toml
[target]
host = "192.168.1.50"   # host or host:port; the port defaults to 3389
user = "administrator"
domain = "CORP"         # leave out for a local account
pass_file = "~/.config/win-rdp-mcp/target.pass"
width = 1600            # session size, default 1280x800
height = 900

[security]
enable_tier3 = true     # Shell, file writes, registry writes, ...

[tools]
exclude = ["ScreenRecord"]
```

The password is looked up in this order: `--pass-file`, then
`$WIN_RDP_TARGET_PASS`, then `password` or `pass_file` under `[target]`. An
inline `password` is refused unless the file is `chmod 600`. Unknown keys are an
error, so a typo fails loudly instead of being ignored.
[`win-rdp-mcp.example.toml`](win-rdp-mcp.example.toml) lists every key.

### Requirements

Nothing beyond the binary. The controller speaks RDP itself (TLS + NLA/CredSSP,
the bitmap and RemoteFX codecs, and drive redirection), so there is no FreeRDP,
X server or screenshot tool to install. Drive the controller from Linux, macOS
or Windows.

The agent is launched by pasting its command into the Run dialog over RDP
clipboard redirection, so the target must allow both drive and clipboard
redirection (the Windows defaults).

The target account must be able to log in over RDP. That's it — no admin share,
no WinRM, no firewall change.

---

## Connect an MCP client

The controller *is* the MCP server; your client launches it. It runs on your
machine, not the target.

### Claude Code / Claude Desktop

With the target in the [config file](#configuration), the client entry carries
no connection details or secrets:

```json
{
  "mcpServers": {
    "windows": {
      "command": "npx",
      "args": ["-y", "@stubbedev/win-rdp-mcp", "control"]
    }
  }
}
```

In Claude Code that is one line:

```sh
claude mcp add windows -s user -- npx -y @stubbedev/win-rdp-mcp control
```

Swap `npx -y @stubbedev/win-rdp-mcp` for `win-rdp-mcp` if the binary is on
your `PATH`, or for `nix run github:stubbedev/win-rdp-mcp --`.

To drive several machines, give each its own file and entry:

```json
"args": ["-y", "@stubbedev/win-rdp-mcp", "control",
         "--config", "/home/you/.config/win-rdp-mcp/lab.toml"]
```

Without a config file, put everything in the arguments and the password in the
environment:

```json
{
  "mcpServers": {
    "windows": {
      "command": "npx",
      "args": ["-y", "@stubbedev/win-rdp-mcp", "control",
               "--target", "192.168.1.50", "--user", "administrator"],
      "env": { "WIN_RDP_TARGET_PASS": "your-password" }
    }
  }
}
```

---

## Install

### npm / npx

```sh
npx -y @stubbedev/win-rdp-mcp control
```

Downloads the prebuilt binary for your platform on first run, and for `control`
also the Windows agent it pushes into the session.

### Nix

```sh
nix run github:stubbedev/win-rdp-mcp -- control
nix profile install github:stubbedev/win-rdp-mcp
```

Builds are pushed to a public binary cache. Add `--accept-flake-config`, or put
these in your `nix.conf`:

```
extra-substituters = https://nix.stubbe.dev/default
extra-trusted-public-keys = default:9P4FePqHV1rGv5NDBun0GN26y83pcaaMr/NHZrxKaac=
```

### Go

```sh
go install github.com/stubbedev/win-rdp-mcp@latest
GOOS=windows GOARCH=amd64 go build -o win-rdp-mcp.exe github.com/stubbedev/win-rdp-mcp  # the agent to push
```

### Release binaries

Every [release](https://github.com/stubbedev/win-rdp-mcp/releases) carries
binaries for Linux, macOS and Windows, and `.mcpb` bundles for one-click Claude
Desktop install of the standalone agent.

> Everything builds and tests on Linux, macOS and Windows — that is how CI checks
> it. The **controller runs anywhere**; the **agent binary is for Windows**, and
> the controller looks for it as `win-rdp-mcp.exe` next to itself (or pass
> `--agent-exe` / `agent_exe`). The npm and Nix packages ship it there.

---

## Flags

GNU-style: every option has a `--long` form; the common ones a `-short` alias.
The right-hand column is the matching config-file key.

### Controller (`win-rdp-mcp control`)

```
-t, --target HOST[:PORT]   target RDP host (required)           [target] host
-u, --user NAME            RDP username (required)              [target] user
-D, --domain NAME          RDP domain (blank for local account) [target] domain
-p, --pass-file PATH       file holding the password            [target] pass_file / password
-W, --width N              session width  (default 1280)        [target] width
-H, --height N             session height (default 800)         [target] height
    --agent-exe PATH       Windows agent to push                [target] agent_exe
                           (default: win-rdp-mcp.exe next to this binary, else ./win-rdp-mcp.exe)
    --no-agent             desktop tools only; no agent          [target] no_agent
-c, --config PATH          config file (default: see Configuration)
-d, --debug                log RDP + bootstrap detail
-a, --enable-all           enable every tool, including destructive tier 3
-3, --enable-tier3         enable the destructive tier 3 tools  [security] enable_tier3
-2, --disable-tier2        disable the interactive tier 2 tools [security] disable_tier2
                                                                [tools] enable / exclude
```

### Agent (`win-rdp-mcp`, runs on the target)

```
-t, --transport stdio|streamable-http|rdpvc default streamable-http (rdpvc is set by the controller)
-H, --host / -p, --port                     bind address (default 127.0.0.1:8090)
-c, --config PATH                           config file ([server], [security], [tools])
-k, --auth-key KEY                          also $WIN_RDP_MCP_AUTH_KEY
    --allow-insecure-remote                 non-loopback bind with no auth (danger)
    --ssl-certfile / --ssl-keyfile          enable HTTPS
    --oauth-client-id / --oauth-client-secret
    --oauth-redirect-uris                   comma-separated loopback callbacks
-a, --enable-all  -3, --enable-tier3  -2, --disable-tier2
    --tools / -x, --exclude-tools           comma-separated
    --ip-allowlist                          comma-separated IPs/CIDRs
-d, --debug
```

Subcommands: `control`, `install`, `uninstall`, `health`.

---

## Tools

45 tools. Every result is prefixed `[task:<id>]`, which `GetTaskStatus` and
`CancelTask` take.

When running as a controller, the split below is what has a footprint on the
target and what does not. When running the agent standalone on the target, every
tool is served by the agent and the distinction disappears.

**Served locally by the controller** — driven straight over RDP by the
built-in client, **no footprint on the target**:
`Snapshot`, `Click`, `Type`, `Move`, `Scroll`, `Shortcut`, `Wait`, and
`ReconnectSession` (which re-establishes the RDP connection).

> `Snapshot` returns the screenshot on its own (or a `left`/`top`/`right`/`bottom`
> region of it); once the agent is up it also carries the window list and the
> foreground window's controls, read through UI Automation (named buttons,
> fields, menu items, list items). `Click` takes `element=N` from that list, so
> a model can aim by control rather than by guessing pixels.
>
> `Type` pastes long single-line text in one go and restores the clipboard
> afterwards; `FileUpload` and `FileDownload` take a `local_path` on your
> machine and move files of any size through the redirected drive.

**Served by the in-session agent** — everything else, including the desktop
tools that need Win32 (window management, OCR, recording, the annotated view):

- *Desktop/Win32:* `AnnotatedSnapshot`, `FocusWindow`, `MinimizeAll`, `App`,
  `OCR`, `ScreenRecord`, `LockScreen`, `GetClipboard`,
  `SetClipboard`, `Notification`, `PlaySound`
- *System:* `Shell`, `ListProcesses`, `KillProcess`, `GetSystemInfo`,
  `ServiceList/Start/Stop`, `TaskList/Create/Delete`, `EventLog`, `RegRead`,
  `RegWrite`, `FileRead/Write/List/Search/Download/Upload`
- *Network:* `Ping`, `PortCheck`, `NetConnections`, `Scrape`
- *Tasks:* `GetTaskStatus`, `GetRunningTasks`, `CancelTask`

The first agent-served call after connect waits a few seconds while the agent
starts in the session; the locally-served tools work immediately.

---

## Security model

This hands an AI a keyboard, a mouse and — if you let it — a PowerShell prompt on
a real machine. The defaults reflect that.

### Tiers

**Tier 1 (read-only) and tier 2 (desktop interaction) are on by default; tier 3
is not.**

| Tier | What it does | Enable with |
|------|--------------|-------------|
| **1** — read-only | screenshots, OCR, recording, listings, registry reads, event log, network checks | on by default |
| **2** — desktop | Click, Type, Move, Scroll, Shortcut, FocusWindow, MinimizeAll, Scrape, ReconnectSession | on by default (`--disable-tier2` / `-2` to turn off) |
| **3** — destructive | Shell, App, PlaySound, File writes/reads, KillProcess, RegWrite, Service control, Task create/delete, SetClipboard, LockScreen | `--enable-tier3` / `-3`, or `--enable-all` / `-a` |

The tier chosen on the controller is reproduced on the pushed agent.

### Credentials

Pass the RDP password by `--pass-file`, `$WIN_RDP_TARGET_PASS` or the config
file, never on the command line (argv is world-readable via `ps`). There is no
password flag. A config file holding an inline `password` must be `chmod 600`,
or the controller refuses to start.

### Other guards

- **SSRF** — `Scrape` and `PlaySound` refuse non-public targets (loopback,
  private ranges, link-local, CGNAT) and refuse redirects.
- **PowerShell injection** — every value interpolated into a PowerShell command
  is single-quoted with its quotes doubled; `TaskCreate` takes a closed set of
  schedule types.
- **Killing by name is exact**, not fuzzy — upstream scored `notepad` against
  `notepad++` at 87, high enough to kill the wrong process.
- When the agent is run **standalone over the network**, it refuses a
  non-loopback bind with no authentication, and refuses tier 3 on such a bind
  outright.

### Two things to know

- **Defender.** Pushing an `.exe` into a session and driving input is,
  mechanically, what lateral-movement malware does. Defender may quarantine the
  agent. If the system tier never comes up, that's the first thing to check
  (run with `--debug`).
- **Authorization.** This is a remote-control pattern. Use it only on machines
  you own or are authorized to administer.

---

## Running the agent directly

If you'd rather install the agent on the target (and you have a way in — RDP
drive, SMB, a file copy), it's a standalone MCP server too:

```powershell
# local, over stdio (Claude Desktop launches it)
.\win-rdp-mcp.exe --transport stdio

# over the network, authenticated
.\win-rdp-mcp.exe --host 0.0.0.0 --port 8090 --auth-key YOUR_SECRET_KEY

# auto-start at boot as the logged-on user (needs a desktop for the GUI tools)
.\win-rdp-mcp.exe install
```

In that mode you connect your client straight to the agent and skip the
controller entirely.

---

## Development

```sh
nix develop        # Go 1.27, gopls, staticcheck, delve, just, node (for the npm wrapper)
just               # list every recipe
```

| Recipe | What it does |
|--------|--------------|
| `just build` / `just build-windows` | build for this platform / the Windows agent |
| `just check` | the full merge gate: gofmt, vet (both platforms), tests, both builds |
| `just run` / `just run-http` | run the agent over stdio / on loopback |
| `just tools` | list the tools for a given set of flags |
| `just smoke` | drive the agent over stdio and assert `tools/list` |
| `just e2e` | full end-to-end run against a real Windows machine (set `WIN_RDP_E2E_TARGET`, `_USER`, `_PASS_FILE`; a [dockur/windows](https://github.com/dockur/windows) VM works) |
| `just bundle` | pack and validate the `.mcpb` |
| `just nix-build` / `just nix-check` / `just nix-vendor-hash` | flake build / check / refresh vendorHash |
| `just release-patch` / `-minor` / `-major` | cut a release |

### Layout

| File | Holds |
|------|-------|
| `controller.go` | the controller: flags and config, MCP registration and routing |
| `controller_session.go` | the RDP session: connect/reconnect, framebuffer, clipboard, disconnect reasons |
| `controller_input.go` | the desktop tools as RDP keyboard/mouse input |
| `controller_agent.go` | launching the agent in the session, the RPC client, file transfers |
| `screendiff.go` | framebuffer diffs that confirm the Run dialog is up before the agent command runs |
| `keys.go` | the key table shared by the controller (scancodes) and agent (virtual keys) |
| `elements.go` / `uia_windows.go` | the UI Automation element list behind `Snapshot` and `Click element=N` |
| `sysadmin*.go` | services, scheduled tasks, event log and ping via Win32/COM |
| `internal/grdp` | the RDP client, vendored from [nakagami/grdp](https://github.com/nakagami/grdp) v0.9.12 (GPL-3.0) with audio and cgo stripped and hooks added for drive redirection, dynamic virtual channels, direct input and clipboard paste |
| `internal/rdpdr` | the redirected drive (MS-RDPEFS) that carries the agent binary and file transfers |
| `rpc.go` | the controller↔agent protocol: framed JSON calls over the virtual channel |
| `agent_channel*.go` | the agent side of the virtual channel, and its self-relocation |
| `tools.json` / `tools.go` | tool schemas + agent registration/dispatch |
| `desktop.go` / `system.go` | the agent's Win32 and system tools |
| `platform_windows.go` / `platform_other.go` | the Win32 layer + off-Windows stubs |
| `tiers.go` `security.go` `oauth.go` `tasks.go` `config.go` | tiers, gates, OAuth, concurrency, config |

### CI

- **CI** — gofmt, vet (host + Windows cross), race tests (including the stdio
  smoke test), Linux/Windows/macOS builds, `.mcpb` validate, plus the suite on a
  real `windows-latest` runner.
- **Flake** — recomputes `vendorHash` on any Go change and commits it, builds
  the flake, runs `nix flake check`, pushes the closure to the cache.
- **Flake update** — weekly `nix flake update`, committed only if it still builds.
- **security** — CodeQL for Go and Actions plus a Grype vulnerability scan, on every push and nightly.
- **Publish** — on a version tag: release binaries, `.mcpb` bundles and the npm
  package with provenance.
- **Dependabot** — weekly Go and Actions updates, auto-merged once green.

---

## Differences from winremote-mcp

Same tool names, same tiers, same TOML config shape. What changed: it's a single
static Go binary (no Python/pywin32 on the target); it can drive the target over
**RDP alone** with nothing installed; killing a process by name is exact, not
fuzzy; the SSRF guard also blocks CGNAT and `0.0.0.0/8`; unknown config keys are
an error; env vars are `WIN_RDP_MCP_*` and the config file is `win-rdp-mcp.toml`.

## License

GPL-3.0 — see [LICENSE](LICENSE). The vendored RDP client in `internal/grdp` is
GPL-3.0, which is why the project as a whole is.
