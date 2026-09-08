#!/usr/bin/env bash
# Gated commit for one PLAN.md Phase. Mac/bash port of commit.ps1 -- same four conditions, same
# PLAN.md status-cell update, same commit-message tail.
#
# Conditions (all must hold unless --override is given for the review condition):
#   1. .loop/state.json exists and state.phase == --phase
#   2. state.lastTest.result == "pass"
#   3. current working-tree hash == state.lastTest.treeHash   (nothing changed since the tests ran)
#   4. state.lastReview.verdict == "PASS"                       (or --override "<reason>")
# Phase 1..3: print the git commands and exit 0 without committing (a human reviews the diff and commits).
# Phase 4+  : update the Phase's 상태 cell in PLAN.md (warn-only if the row is not found), then git add -A, git commit.
# --override records {reason, at} in state.json and adds an "Override: <reason>" line to the commit body.
#
# The PreToolUse hook (scripts/hooks/deny-direct-commit.sh) blocks `git commit` issued directly from
# the agent's shell tool. This script's own `git commit` is a child process of this script, not a
# tool call, so the hook never sees it; additionally GH_ARS_LOOP_COMMIT=1 is set in the child
# environment as a marker for any future git-side hook.
#
# Usage:
#   ./scripts/commit.sh --phase 3 --message "feat(plan): capacity, desired, spread, reconcile (Phase 3, SPEC §8)"
set -e

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
. "$SCRIPT_DIR/loop-common.sh"
REPO="$(repo_root)"
assert_git_repo
cd "$REPO"

PHASE=""
MESSAGE=""
OVERRIDE=""

while [ $# -gt 0 ]; do
    case "$1" in
        --phase) PHASE="$2"; shift 2 ;;
        --message) MESSAGE="$2"; shift 2 ;;
        --override) OVERRIDE="$2"; shift 2 ;;
        *) echo "commit: unknown argument: $1" >&2; exit 1 ;;
    esac
done
[ -z "$PHASE" ] && { echo "commit: --phase is required" >&2; exit 1; }
[ -z "$MESSAGE" ] && { echo "commit: --message is required" >&2; exit 1; }

STATE="$(read_state)"
PROBLEMS=()
if [ -z "$STATE" ]; then
    PROBLEMS+=("state.json 없음: 먼저 ./scripts/gate.sh --phase $PHASE ... 를 실행")
else
    STATE_PHASE="$(state_get "$STATE" "phase" 2>/dev/null || echo "")"
    if [ "$STATE_PHASE" != "$PHASE" ]; then
        PROBLEMS+=("state.phase=$STATE_PHASE 이지만 요청은 --phase $PHASE")
    fi
    LAST_TEST_RESULT="$(state_get "$STATE" "lastTest.result" 2>/dev/null || echo "")"
    if [ "$LAST_TEST_RESULT" != "pass" ]; then
        PROBLEMS+=("lastTest 가 pass 가 아님 (gate.sh 을 다시 실행)")
    else
        NOW_HASH="$(get_tree_hash)"
        PREV_HASH="$(state_get "$STATE" "lastTest.treeHash" 2>/dev/null || echo "")"
        if [ "$NOW_HASH" != "$PREV_HASH" ]; then
            PROBLEMS+=("작업 트리가 마지막 테스트 이후 바뀜 (treeHash ${PREV_HASH:0:12} → ${NOW_HASH:0:12}). gate.sh 을 다시 실행")
        fi
    fi
    if [ -n "$OVERRIDE" ]; then
        if ! state_get "$STATE" "lastReview" >/dev/null 2>&1; then
            PROBLEMS+=("--override 는 리뷰가 한 번 이상 돈 뒤에만 허용")
        fi
    else
        VERDICT="$(state_get "$STATE" "lastReview.verdict" 2>/dev/null || echo "(없음)")"
        if [ "$VERDICT" != "PASS" ]; then
            PROBLEMS+=("lastReview.verdict=$VERDICT (PASS 필요. 지적을 해결하고 gate.sh 재실행, 또는 --override '<이유>')")
        fi
    fi
fi
if [ "${#PROBLEMS[@]}" -gt 0 ]; then
    echo "commit: 거부"
    for p in "${PROBLEMS[@]}"; do echo "  - $p"; done
    exit 1
fi

BODY="$MESSAGE"
if [ -n "$OVERRIDE" ]; then
    STATE="$(state_set_override "$STATE" "$OVERRIDE" "$(now_iso)")"
    write_state "$STATE"
    BODY="$MESSAGE

Override: $OVERRIDE"
fi
# 모델명은 적지 않는다. 세션마다 달라지고 커밋 기록만 낡는다.
# 에이전트 세션 URL(Claude-Session 등)도 커밋 메시지에 넣지 않는다. 세션은 사라지고 링크만 기록에 남으며,
# 커밋의 근거는 SPEC 절 번호와 PLAN.md 로 충분하다. --message 에 세션 URL 이 있으면 거부한다.
if printf '%s' "$MESSAGE" | grep -qiE 'claude-session|claude\.ai/code/session'; then
    echo "commit: 거부, --message 에 세션 URL 이 들어 있다 (커밋 메시지에 세션 URL 을 넣지 않는다)"
    exit 1
fi
BODY="$BODY

Co-Authored-By: Claude <noreply@anthropic.com>"

if [ "$PHASE" -le 3 ]; then
    echo "commit: Phase $PHASE 은 사람이 diff 확인 후 직접 커밋한다. 게이트 조건은 충족됨. 실행할 명령:"
    echo "  git add -A"
    echo "  git commit -F <메시지 파일>   # 메시지:"
    while IFS= read -r line; do echo "    | $line"; done <<<"$BODY"
    exit 0
fi

# --- PLAN.md: update the 상태 cell of row "| N | ..." in the Phase table BEFORE staging, so the
#     status change rides in the same commit. Warn-only if the row is not found.
PLAN_PATH="$REPO/PLAN.md"
UPDATED=0
PLAN_TMP="$(mktemp)"
while IFS= read -r line; do
    if [ "$UPDATED" -eq 0 ] && printf '%s\n' "$line" | grep -qE "^\|[[:space:]]*${PHASE}[[:space:]]*\|"; then
        line="$(printf '%s' "$line" | awk -F'|' -v today="$(date +%Y-%m-%d)" '
            BEGIN { OFS = "|" }
            {
                if (NF >= 7) { $(NF-1) = " 완료 " today " " }
                out = $1
                for (i = 2; i <= NF; i++) out = out OFS $i
                print out
            }
        ')"
        UPDATED=1
    fi
    printf '%s\n' "$line"
done <"$PLAN_PATH" >"$PLAN_TMP"
if [ "$UPDATED" -eq 1 ]; then
    mv "$PLAN_TMP" "$PLAN_PATH"
    echo "commit: PLAN.md Phase $PHASE 상태 갱신"
else
    rm -f "$PLAN_TMP"
    echo "commit: 경고, PLAN.md 에서 Phase $PHASE 행을 찾지 못해 상태를 갱신하지 않음"
fi

MSG_FILE="$REPO/.loop/commit-msg.txt"
mkdir -p "$REPO/.loop"
printf '%s' "$BODY" >"$MSG_FILE"
export GH_ARS_LOOP_COMMIT=1
cleanup() { rm -f "$MSG_FILE"; unset GH_ARS_LOOP_COMMIT; }
trap cleanup EXIT

git add -A
git commit -F "$MSG_FILE"

echo "commit: 완료 ($(git rev-parse --short HEAD))"
