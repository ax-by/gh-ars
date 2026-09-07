#!/usr/bin/env bash
# Implementation-loop gate: go test -> go vet -> Codex review -> .loop/state.json.
# Mac/bash port of gate.ps1 -- same steps, same state.json schema, same verdict parsing.
#
# Runs the deterministic checks for one PLAN.md Phase and records the outcome in .loop/state.json
# so that commit.sh (and the next session) can verify the gate without trusting memory.
#
# Review round: increments each time a review runs for the same --phase; resets on PASS or when
# --phase changes. Verdict parsing: exactly one "## Verdict" heading followed by a line that is
# PASS or BLOCK. Missing, ambiguous, or duplicated verdicts are treated as BLOCK (fail closed).
#
# Usage:
#   ./scripts/gate.sh --phase 3 --packages "internal/plan" --spec "S7.2-3, S8.1-8.3" --design "S5" --base HEAD
#   ./scripts/gate.sh --phase 3 --packages "internal/plan" --spec "S8" --design "S5" --test-only
#   ./scripts/gate.sh --phase 3 ... --review-from-file .codex-review/last.md   # re-parse a saved review
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/loop-common.sh"
REPO="$(repo_root)"
assert_git_repo
cd "$REPO"

PHASE=""
PACKAGES=""
SPEC=""
DESIGN=""
BASE="none"
TEST_ONLY=0
REVIEW_FROM_FILE=""

while [ $# -gt 0 ]; do
    case "$1" in
        --phase) PHASE="$2"; shift 2 ;;
        --packages) PACKAGES="$2"; shift 2 ;;
        --spec) SPEC="$2"; shift 2 ;;
        --design) DESIGN="$2"; shift 2 ;;
        --base) BASE="$2"; shift 2 ;;
        --test-only) TEST_ONLY=1; shift ;;
        --review-from-file) REVIEW_FROM_FILE="$2"; shift 2 ;;
        *) echo "gate: unknown argument: $1" >&2; exit 1 ;;
    esac
done
[ -z "$PHASE" ] && { echo "gate: --phase is required" >&2; exit 1; }
[ -z "$PACKAGES" ] && { echo "gate: --packages is required" >&2; exit 1; }
[ -z "$SPEC" ] && { echo "gate: --spec is required" >&2; exit 1; }
[ -z "$DESIGN" ] && { echo "gate: --design is required" >&2; exit 1; }

GO_SH="$SCRIPT_DIR/go.sh"
REVIEW_SH="$SCRIPT_DIR/codex-review.sh"

# --- state: load or create; reset review round when the phase changes
STATE="$(read_state)"
if [ -z "$STATE" ] || [ "$(state_get "$STATE" "phase" 2>/dev/null || echo x)" != "$PHASE" ]; then
    STATE="$(new_state "$PHASE" "$PACKAGES" "$SPEC" "$DESIGN" "$BASE")"
else
    STATE="$(state_set_args "$STATE" "$PACKAGES" "$SPEC" "$DESIGN" "$BASE")"
fi

# --- tests
echo "gate: phase=$PHASE packages=$PACKAGES"
echo "gate: go test ./..."
TEST_OUT="$("$GO_SH" test ./... 2>&1)" && TEST_CODE=0 || TEST_CODE=$?
echo "$TEST_OUT"
# Bootstrap case: a repo with no Go packages yet. `go test ./...` exits 1 with "no packages to test";
# treat that as pass so the gate is usable before the first package lands. Never triggers once code exists.
if [ "$TEST_CODE" -ne 0 ] && echo "$TEST_OUT" | grep -q "no packages to test"; then
    echo "gate: no Go packages yet, treating tests as pass"
    TEST_CODE=0
fi
if [ "$TEST_CODE" -eq 0 ]; then
    echo "gate: go vet ./..."
    VET_OUT="$("$GO_SH" vet ./... 2>&1)" && TEST_CODE=0 || TEST_CODE=$?
    echo "$VET_OUT"
    if [ "$TEST_CODE" -ne 0 ] && echo "$VET_OUT" | grep -q "no packages to vet"; then
        echo "gate: no Go packages yet, treating vet as pass"
        TEST_CODE=0
    fi
fi
TREE_HASH="$(get_tree_hash)"
STATE="$(state_set_last_test "$STATE" "$([ "$TEST_CODE" -eq 0 ] && echo pass || echo fail)" "$TREE_HASH" "$(now_iso)")"
write_state "$STATE"
if [ "$TEST_CODE" -ne 0 ]; then
    echo "gate: BLOCK, tests or vet failed (exit $TEST_CODE). Review skipped."
    exit 1
fi
echo "gate: tests pass. treeHash=${TREE_HASH:0:12}"

if [ "$TEST_ONLY" -eq 1 ]; then
    echo "gate: --test-only, review skipped. state.phase=$PHASE lastTest=pass"
    exit 0
fi

# --- review
REVIEW_DIR="$REPO/.codex-review"
mkdir -p "$REVIEW_DIR"
LAST_REVIEW_PATH="$REVIEW_DIR/last.md"

if [ -n "$REVIEW_FROM_FILE" ]; then
    [ -f "$REVIEW_FROM_FILE" ] || { echo "gate: review file not found: $REVIEW_FROM_FILE" >&2; exit 1; }
    REVIEW_TEXT="$(cat "$REVIEW_FROM_FILE")"
else
    echo "gate: codex review (gpt-6-astra, medium) ..."
    REVIEW_TEXT="$("$REVIEW_SH" --phase "Phase $PHASE" --packages "$PACKAGES" --spec "$SPEC" --design "$DESIGN" --base "$BASE" 2>&1)" && REVIEW_CODE=0 || REVIEW_CODE=$?
    printf '%s' "$REVIEW_TEXT" >"$LAST_REVIEW_PATH"
    if [ "$REVIEW_CODE" -ne 0 ]; then
        echo "gate: review command exited $REVIEW_CODE (output saved to .codex-review/last.md)"
    fi
fi

# --- verdict parsing (fail closed)
VERDICT="BLOCK"
REASON=""
HEADING_COUNT="$(printf '%s\n' "$REVIEW_TEXT" | grep -c -E '^[[:space:]]*##[[:space:]]+Verdict[[:space:]]*$' || true)"
if [ "$HEADING_COUNT" -ne 1 ]; then
    REASON="verdict heading count = $HEADING_COUNT (expected 1)"
else
    V="$(printf '%s\n' "$REVIEW_TEXT" | awk '
        /^[[:space:]]*##[[:space:]]+Verdict[[:space:]]*$/ { found=1; next }
        found && NF>0 { print; exit }
    ' | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' -e 's/`//g')"
    if [ "$V" = "PASS" ]; then
        VERDICT="PASS"
    elif [ "$V" = "BLOCK" ]; then
        VERDICT="BLOCK"
    else
        REASON="verdict line unrecognized: '$V'"
    fi
fi
FINDINGS="$(printf '%s\n' "$REVIEW_TEXT" | grep -c -E '^[[:space:]]*-[[:space:]]+\*\*\[' || true)"

# --- round bookkeeping
ROUND=1
PREV_VERDICT="$(state_get "$STATE" "lastReview.verdict" 2>/dev/null || echo "")"
if [ -n "$PREV_VERDICT" ] && [ "$PREV_VERDICT" != "PASS" ]; then
    PREV_ROUND="$(state_get "$STATE" "lastReview.round" 2>/dev/null || echo 0)"
    ROUND=$((PREV_ROUND + 1))
fi
if [ "$VERDICT" = "PASS" ]; then ROUND=0; fi
STATE="$(state_set_last_review "$STATE" "$VERDICT" "$FINDINGS" "$ROUND" "$(now_iso)")"
write_state "$STATE"

if [ "$VERDICT" = "PASS" ]; then
    echo "gate: PASS, 커밋 가능 (findings=$FINDINGS). next: ./scripts/commit.sh --phase $PHASE --message '...'"
    exit 0
fi
ROUND_TEXT="회차 ${ROUND}/2"
if [ -n "$REASON" ]; then
    echo "gate: BLOCK (fail closed: $REASON), 지적 ${FINDINGS}개, $ROUND_TEXT"
else
    echo "gate: BLOCK, 지적 ${FINDINGS}개, $ROUND_TEXT"
fi
if [ "$ROUND" -ge 2 ]; then
    echo "gate: 2회차 초과. 사람 결정 필요 (commit.sh --override '<이유>' 또는 SPEC 개정)."
fi
echo "gate: review saved to .codex-review/last.md"
exit 1
