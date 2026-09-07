# justfile for win-rdp-mcp
# Run `just` to see all available commands.

set shell := ["bash", "-euo", "pipefail", "-c"]

binary := "win-rdp-mcp"

# Default — list recipes.
default:
    @just --list --unsorted

# ─────────────────────────── Build & Test ───────────────────────────

# Build the binary for this platform.
build:
    go build -o {{binary}} .
    @echo "Built ./{{binary}}"

# Cross-compile the Windows binary — the one that actually gets deployed.
build-windows:
    GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o {{binary}}.exe .
    @echo "Built ./{{binary}}.exe"

# Install into $GOBIN (or ~/go/bin).
install:
    go install .

# Auto-fix formatting drift.
fmt:
    gofmt -w .

# Read-only gofmt + vet gate, for this platform and cross-compiled for Windows.
lint:
    #!/usr/bin/env bash
    set -euo pipefail
    # The Windows pass drops the unsafeptr analyzer: the clipboard code holds one
    # documented uintptr-to-pointer conversion, on memory outside the Go heap,
    # that vet cannot prove sound. See globalLock in platform_windows.go.
    out=$(gofmt -l .)
    if [ -n "$out" ]; then
        echo "code is not formatted; run 'just fmt':"
        printf '%s\n' "$out"
        exit 1
    fi
    go vet ./...
    GOOS=windows go vet -unsafeptr=false ./...

test:
    go test ./...

test-race:
    go test -race ./...

# End-to-end: drive the built binary over stdio and assert tools/list.
smoke:
    npm run smoke

# Pack and validate the .mcpb bundle for one-click Claude Desktop install.
bundle:
    npm run bundle

# Everything CI runs as the merge gate.
check: lint test build build-windows

# Enable the pre-commit gofmt + vet gate (git core.hooksPath = .githooks).
install-hooks:
    git config core.hooksPath .githooks
    @echo "pre-commit gofmt + vet gate is now active (bypass with --no-verify)."

clean:
    rm -f {{binary}} {{binary}}.exe
    rm -rf dist/ bin/{{binary}}-native bin/{{binary}}-native.exe result result-*

# ─────────────────────────── Run ───────────────────────────

# Run over stdio — how Claude Desktop and Claude Code launch it locally.
run *ARGS:
    go run . -transport stdio {{ARGS}}

# Run the HTTP transport on loopback with every tool enabled.
run-http *ARGS:
    go run . -host 127.0.0.1 -port 8090 -enable-all -debug {{ARGS}}

# List the tools the server would expose with the given flags.
tools *ARGS:
    #!/usr/bin/env bash
    set -euo pipefail
    go build -o {{binary}} .
    { printf '%s\n' \
        '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"just","version":"0"}}}' \
        '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
        '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}'; sleep 1; } \
    | ./{{binary}} -transport stdio {{ARGS}} 2>/dev/null \
    | node -e 'let d="";process.stdin.on("data",c=>d+=c);process.stdin.on("end",()=>{
        const r=d.trim().split("\n").map(JSON.parse).find(m=>m.id===2);
        console.log(r.result.tools.length+" tools:");
        for (const t of r.result.tools) console.log("  "+t.name);
      });'

# ─────────────────────────── Nix ───────────────────────────

nix-build:
    nix build .#default --print-build-logs

nix-check:
    nix flake check --print-build-logs

# Enter the dev shell (Go toolchain, gopls, just, node, zip).
nix-shell:
    nix develop

nix-update:
    nix flake update

# Recompute vendorHash locally, the same way .github/workflows/flake.yml does.
nix-vendor-hash:
    #!/usr/bin/env bash
    set -euo pipefail
    FAKE="sha256-AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
    CUR="$(grep -oP 'vendorHash = "\K[^"]+' flake.nix)"
    sed -i "s#vendorHash = \"${CUR}\"#vendorHash = \"${FAKE}\"#" flake.nix
    GOT="$(nix build .#default --no-link 2>&1 | grep -oP 'got:\s+\K(sha256-\S+)' | head -1 || true)"
    [ -z "$GOT" ] && GOT="$CUR"
    sed -i "s#vendorHash = \"${FAKE}\"#vendorHash = \"${GOT}\"#" flake.nix
    echo "vendorHash = ${GOT}"

# ─────────────────────────── Release ───────────────────────────
# package.json holds the version: flake.nix reads it, the Go binary embeds it,
# and publish.yml refuses a tag that disagrees with it. `npm version` bumps,
# commits and tags in one go (with `preversion` running vet + test + smoke
# first), so these are wrappers over the npm scripts rather than a second
# implementation of the same flow.

release-preview:
    #!/usr/bin/env bash
    set -euo pipefail
    CUR=$(node -p "require('./package.json').version")
    IFS=. read -r MAJOR MINOR PATCH <<<"$CUR"
    echo "Current version: v$CUR"
    echo "  release-major: v$((MAJOR + 1)).0.0"
    echo "  release-minor: v${MAJOR}.$((MINOR + 1)).0"
    echo "  release-patch: v${MAJOR}.${MINOR}.$((PATCH + 1))"

release-patch:
    npm run release:patch

release-minor:
    npm run release:minor

release-major:
    npm run release:major
