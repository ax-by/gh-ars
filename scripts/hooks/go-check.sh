#!/usr/bin/env bash
# Claude Code PostToolUse hook (Edit|Write): gofmt -w the edited .go file, then go vet its package.
# Mac/bash port of go-check.ps1. Non-.go files: exit 0 silently. vet failure: message on stderr +
# exit 2 so Claude sees it. Invoked in exec form from settings (no shell); cwd is the project root.
RAW="$(cat)"
F="$(printf '%s' "$RAW" | node -e '
    let d = "";
    process.stdin.on("data", c => d += c);
    process.stdin.on("end", () => {
        try {
            const j = JSON.parse(d);
            process.stdout.write(String(j.tool_input && j.tool_input.file_path || ""));
        } catch (e) { /* nothing on parse failure */ }
    });
' 2>/dev/null)"
[ -z "$F" ] && exit 0
case "$F" in *.go) ;; *) exit 0 ;; esac

GO_BIN="$(command -v go || true)"
if [ -z "$GO_BIN" ] && [ -x "/opt/homebrew/bin/go" ]; then GO_BIN="/opt/homebrew/bin/go"; fi
if [ -z "$GO_BIN" ] && [ -x "/usr/local/go/bin/go" ]; then GO_BIN="/usr/local/go/bin/go"; fi
[ -z "$GO_BIN" ] && exit 0
BIN_DIR="$(dirname "$GO_BIN")"

"$BIN_DIR/gofmt" -l -w "$F"
cd "$(dirname "$F")" || exit 0
"$BIN_DIR/go" vet .
CODE=$?
[ "$CODE" -ne 0 ] && exit 2
exit 0
