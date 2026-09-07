#!/usr/bin/env bash
# Claude Code PreToolUse hook (Bash): refuse `git commit` issued directly from the agent's shell tool.
# Mac/bash port of deny-direct-commit.ps1.
#
# Distinguishing mechanism: the hook only sees the *tool call* text on stdin (tool_input.command). A
# commit made by scripts/commit.sh runs `git commit` as a child process of that script, which is
# never a tool call, so it is invisible to this hook and is not blocked. The only tool-call form that
# is allowed through is one that invokes scripts/commit.sh itself. commit.sh additionally sets
# GH_ARS_LOOP_COMMIT=1 in its child environment as a marker for any future git-side (pre-commit)
# hook; tool-level hooks cannot observe the environment of a command that has not run yet, which is
# why the command text is the discriminator here.
RAW="$(cat)"
CMD="$(printf '%s' "$RAW" | node -e '
    let d = "";
    process.stdin.on("data", c => d += c);
    process.stdin.on("end", () => {
        try {
            const j = JSON.parse(d);
            process.stdout.write(String(j.tool_input && j.tool_input.command || ""));
        } catch (e) { /* nothing on parse failure, matches the .ps1 exit-0 fallback */ }
    });
' 2>/dev/null)"
[ -z "$CMD" ] && exit 0

IS_GIT_COMMIT=0
echo "$CMD" | grep -qE '(^|[[:space:];&|])git[[:space:]]+(-C[[:space:]]+[^[:space:]]+[[:space:]]+)?commit([[:space:]]|$)' && IS_GIT_COMMIT=1
VIA_SCRIPT=0
echo "$CMD" | grep -q 'commit\.sh' && VIA_SCRIPT=1

if [ "$IS_GIT_COMMIT" -eq 1 ] && [ "$VIA_SCRIPT" -eq 0 ]; then
    node -e '
        console.log(JSON.stringify({
            hookSpecificOutput: {
                hookEventName: "PreToolUse",
                permissionDecision: "deny",
                permissionDecisionReason: "직접 git commit 은 루프 게이트를 우회한다. ./scripts/gate.sh --phase N ... 로 검증한 뒤 ./scripts/commit.sh --phase N --message \"...\" 를 사용할 것.",
            },
        }));
    '
fi
exit 0
