#!/usr/bin/env bash
# Run a Claude Code (opus 5, high effort) read-only code review against docs/SPEC.md and docs/DESIGN.md.
# Same contract as codex-review.sh: same prompt template (docs/review/PROMPT.md), same arguments,
# same output shape ("## Verdict" + findings), so gate.sh can consume either reviewer.
#
# The review runs in a NEW headless session (`claude -p`) that shares no context with the session
# that wrote the code -- the reviewer starts from the repo and the rendered prompt only.
#
# Usage:
#   ./scripts/claude-review.sh --phase "Phase 10" --packages "internal/machine" \
#       --spec "S7.1-3, S10.2" --design "S7" --base HEAD~1
#
#   Omit --base (or pass "none") to review the listed paths in full instead of a diff.
set -e

REPO="$(cd "$(dirname "$0")/.." && pwd)"

PHASE=""
PACKAGES=""
SPEC=""
DESIGN=""
BASE="none"
MODEL="claude-opus-5"
EFFORT="high"
TIMEOUT=$((45 * 60))

while [ $# -gt 0 ]; do
    case "$1" in
        --phase) PHASE="$2"; shift 2 ;;
        --packages) PACKAGES="$2"; shift 2 ;;
        --spec) SPEC="$2"; shift 2 ;;
        --design) DESIGN="$2"; shift 2 ;;
        --base) BASE="$2"; shift 2 ;;
        --model) MODEL="$2"; shift 2 ;;
        --effort) EFFORT="$2"; shift 2 ;;
        --timeout) TIMEOUT="$2"; shift 2 ;;
        *) echo "claude-review: unknown argument: $1" >&2; exit 1 ;;
    esac
done
[ -z "$PHASE" ] && { echo "claude-review: --phase is required" >&2; exit 1; }
[ -z "$PACKAGES" ] && { echo "claude-review: --packages is required" >&2; exit 1; }
[ -z "$SPEC" ] && { echo "claude-review: --spec is required" >&2; exit 1; }
[ -z "$DESIGN" ] && { echo "claude-review: --design is required" >&2; exit 1; }

command -v claude >/dev/null 2>&1 || { echo "claude CLI not found in PATH" >&2; exit 1; }

# Section references may be passed in ASCII as "S8.1" (for "§8.1") -- shared convention with the
# Windows/PowerShell side.
SPEC="$(echo "$SPEC" | sed 's/S\([0-9]\)/§\1/g')"
DESIGN="$(echo "$DESIGN" | sed 's/S\([0-9]\)/§\1/g')"

TEMPLATE="$(cat "$REPO/docs/review/PROMPT.md")"
OUT_DIR="$REPO/.codex-review"   # shared review artifact dir (gitignored); both reviewers write here
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
INSTRUCTION="Read the file .codex-review/prompt.md in this repository and carry out the review it describes exactly, including its output contract. Do not modify any files. Print only the review."

# Read-only tool set: the reviewer inspects the tree and the diff, and must not write. Anything not
# listed is denied by the headless session instead of prompting.
# 쉼표로 구분한다: 공백 구분은 셸이 `Bash(git log:*)` 를 두 인자로 쪼개 규칙이 깨진다.
ALLOWED="Read,Grep,Glob,Bash(git diff:*),Bash(git show:*),Bash(git log:*),Bash(git status:*),Bash(git ls-files:*),Bash(rg:*),Bash(cat:*),Bash(sed:*),Bash(head:*),Bash(tail:*),Bash(nl:*),Bash(wc:*),Bash(ls:*),Bash(find:*),Bash(go doc:*)"
DENIED="Write,Edit,NotebookEdit,Task,Agent,WebFetch,WebSearch"

echo "claude-review: phase=$PHASE packages=$PACKAGES base=$BASE model=$MODEL effort=$EFFORT"
cd "$REPO"
STDOUT_PATH="$OUT_DIR/claude.stdout.txt"
STDERR_PATH="$OUT_DIR/claude.stderr.txt"
rm -f "$STDOUT_PATH" "$STDERR_PATH"

# --strict-mcp-config keeps the reviewer hermetic (no user MCP servers). No --resume/--continue:
# the session starts empty, so the reviewer shares no context with the implementer.
claude -p "$INSTRUCTION" \
    --model "$MODEL" \
    --effort "$EFFORT" \
    --output-format text \
    --strict-mcp-config \
    --allowedTools "$ALLOWED" \
    --disallowedTools "$DENIED" \
    </dev/null >"$STDOUT_PATH" 2>"$STDERR_PATH" &
CLAUDE_PID=$!

# 감시자는 stdout 을 반드시 놓아야 한다. 호출자(gate.sh)는 이 스크립트를 `$( )` 로 받는데,
# 명령 치환은 파이프의 쓰기 끝이 전부 닫힐 때까지 읽는다 — 잠들어 있는 감시자가 stdout 을
# 물고 있으면 리뷰가 끝나도 게이트가 타임아웃까지 멈춰 선다.
( sleep "$TIMEOUT"; kill -9 "$CLAUDE_PID" 2>/dev/null ) >/dev/null 2>&1 &
WATCHDOG=$!

wait "$CLAUDE_PID" && RC=0 || RC=$?   # set -e 아래에서 비0 종료로 스크립트가 끊기지 않게
kill "$WATCHDOG" 2>/dev/null || true

[ -f "$STDOUT_PATH" ] && cat "$STDOUT_PATH"
if [ "$RC" -ne 0 ]; then
    # stderr only matters when the run failed; a successful review prints nothing there.
    [ -f "$STDERR_PATH" ] && cat "$STDERR_PATH" >&2
fi
exit $RC
