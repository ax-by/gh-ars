#!/usr/bin/env bash
# Wrapper for the Go toolchain. Mac port of go.ps1 -- go is on PATH via Homebrew here, unlike the
# Windows machine, so we prefer PATH and only fall back to well-known install locations.
# Usage: ./scripts/go.sh test ./...
set -e

if command -v go >/dev/null 2>&1; then
    GO_BIN="$(command -v go)"
elif [ -x "/opt/homebrew/bin/go" ]; then
    GO_BIN="/opt/homebrew/bin/go"
elif [ -x "/usr/local/go/bin/go" ]; then
    GO_BIN="/usr/local/go/bin/go"
else
    echo "go not found: not on PATH, /opt/homebrew/bin/go, or /usr/local/go/bin/go" >&2
    exit 1
fi

exec "$GO_BIN" "$@"
