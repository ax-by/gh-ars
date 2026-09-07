#!/usr/bin/env bash
# Run a Codex (gpt-6-astra, medium) read-only code review against docs/SPEC.md and docs/DESIGN.md.
# Mac/bash port of codex-review.ps1.
#
# Usage:
#   ./scripts/codex-review.sh --phase "1 pure core" --packages "internal/resource,internal/domain" \
#       --spec "S4, S8.1, S9.3, R25" --design "S3.1, S5" --base HEAD~1
#
#   Omit --base (or pass "none") to review the listed paths in full instead of a diff.
#   Add --background to detach; then use /codex:status and /codex:result.
set -e

REPO="$(cd "$(dirname "$0")/.." && pwd)"

PHASE=""
PACKAGES=""
SPEC=""
DESIGN=""
BASE="none"
MODEL="gpt-6-astra"
EFFORT="medium"
BACKGROUND=0

while [ $# -gt 0 ]; do
    case "$1" in
        --phase) PHASE="$2"; shift 2 ;;
        --packages) PACKAGES="$2"; shift 2 ;;
        --spec) SPEC="$2"; shift 2 ;;
        --design) DESIGN="$2"; shift 2 ;;
        --base) BASE="$2"; shift 2 ;;
        --model) MODEL="$2"; shift 2 ;;
        --effort) EFFORT="$2"; shift 2 ;;
        --background) BACKGROUND=1; shift ;;
        *) echo "codex-review: unknown argument: $1" >&2; exit 1 ;;
    esac
done
[ -z "$PHASE" ] && { echo "codex-review: --phase is required" >&2; exit 1; }
[ -z "$PACKAGES" ] && { echo "codex-review: --packages is required" >&2; exit 1; }
[ -z "$SPEC" ] && { echo "codex-review: --spec is required" >&2; exit 1; }
[ -z "$DESIGN" ] && { echo "codex-review: --design is required" >&2; exit 1; }

COMPANION="$HOME/.claude/plugins/cache/openai-codex/codex/1.0.6/scripts/codex-companion.mjs"
if [ ! -f "$COMPANION" ]; then
    echo "codex companion not found: $COMPANION" >&2
    exit 1
fi

# Section references may be passed in ASCII as "S8.1" (for "§8.1") -- kept as a shared convention
# with the Windows/PowerShell side rather than a technical requirement on Mac.
SPEC="$(echo "$SPEC" | sed 's/S\([0-9]\)/§\1/g')"
DESIGN="$(echo "$DESIGN" | sed 's/S\([0-9]\)/§\1/g')"

TEMPLATE="$(cat "$REPO/docs/review/PROMPT.md")"
OUT_DIR="$REPO/.codex-review"
mkdir -p "$OUT_DIR"
PROMPT_PATH="$OUT_DIR/prompt.md"

node -e '
    const fs = require("fs");
    const [, outPath, phase, packages, spec, design, base] = process.argv;
    let t = fs.readFileSync(0, "utf8");
    t = t.replace(/\{\{PHASE\}\}/g, phase)
         .replace(/\{\{PACKAGES\}\}/g, packages)
         .replace(/\{\{SPEC_SECTIONS\}\}/g, spec)
         .replace(/\{\{DESIGN_SECTIONS\}\}/g, design)
         .replace(/\{\{BASE\}\}/g, base);
    fs.writeFileSync(outPath, t);
' "$PROMPT_PATH" "$PHASE" "$PACKAGES" "$SPEC" "$DESIGN" "$BASE" <<<"$TEMPLATE"

# The full contract lives in a file so that XML tags and quotes never pass through a shell.
INSTRUCTION="Read the file .codex-review/prompt.md in this repository and carry out the review it describes exactly, including its output contract. Do not modify any files."

ARGS=(task --model "$MODEL" --effort "$EFFORT")
if [ "$BACKGROUND" -eq 1 ]; then ARGS+=(--background); fi
ARGS+=("$INSTRUCTION")

echo "codex-review: phase=$PHASE packages=$PACKAGES base=$BASE model=$MODEL effort=$EFFORT"
cd "$REPO"
# Mirrors codex-review.ps1: the companion prints the rendered result only after `codex app-server`
# exits. When the review used tools, codex leaves code-mode host / node_repl grandchildren holding the
# app-server pipes, so the companion can wait forever. Run node in the background, watch stderr for the
# completion line, and if node is still alive after a grace period, kill its descendants (deepest
# first). The app-server's exit unblocks the companion, which flushes the result and exits by itself.
OUT_DIR="$REPO/.codex-review"
STDOUT_PATH="$OUT_DIR/companion.stdout.txt"
STDERR_PATH="$OUT_DIR/companion.stderr.txt"
rm -f "$STDOUT_PATH" "$STDERR_PATH"
node "$COMPANION" "${ARGS[@]}" >"$STDOUT_PATH" 2>"$STDERR_PATH" &
NODE_PID=$!

COMPLETION_MARKER="Turn completion inferred"
GRACE_AFTER_DONE=30
HARD_LIMIT=$((45 * 60))
DONE_AT=""
STARTED=$(date +%s)
KILLED_TREE=0

descendant_pids() {   # deepest first
    local p
    for p in $(pgrep -P "$1" 2>/dev/null); do
        descendant_pids "$p"
        echo "$p"
    done
}

while kill -0 "$NODE_PID" 2>/dev/null; do
    sleep 2
    NOW=$(date +%s)
    if [ -z "$DONE_AT" ] && grep -q "$COMPLETION_MARKER" "$STDERR_PATH" 2>/dev/null; then DONE_AT=$NOW; fi
    OVER_GRACE=0; OVER_LIMIT=0
    [ -n "$DONE_AT" ] && [ $((NOW - DONE_AT)) -ge "$GRACE_AFTER_DONE" ] && OVER_GRACE=1
    [ $((NOW - STARTED)) -ge "$HARD_LIMIT" ] && OVER_LIMIT=1
    if { [ "$OVER_GRACE" -eq 1 ] || [ "$OVER_LIMIT" -eq 1 ]; } && [ "$KILLED_TREE" -eq 0 ]; then
        KILLED_TREE=1
        DESC=$(descendant_pids "$NODE_PID")
        if [ "$OVER_LIMIT" -eq 1 ]; then WHY="hard limit"; else WHY="after completion"; fi
        echo "codex-review: companion did not exit ($WHY); terminating $(echo "$DESC" | grep -c .) descendant process(es)"
        for d in $DESC; do kill -9 "$d" 2>/dev/null; done
        for _ in $(seq 1 15); do kill -0 "$NODE_PID" 2>/dev/null || break; sleep 2; done
        kill -0 "$NODE_PID" 2>/dev/null && kill -9 "$NODE_PID" 2>/dev/null
    fi
done
wait "$NODE_PID"; NODE_RC=$?
[ -f "$STDOUT_PATH" ] && cat "$STDOUT_PATH"
[ -f "$STDERR_PATH" ] && cat "$STDERR_PATH"
exit $NODE_RC
