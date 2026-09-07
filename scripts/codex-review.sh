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
exec node "$COMPANION" "${ARGS[@]}"
