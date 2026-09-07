#!/usr/bin/env bash
# Shared helpers for the implementation-loop gate (gate.sh, commit.sh, hooks). Mac/bash port of
# loop-common.ps1 -- same state.json schema and tree-hash definition. Requires bash, git, node.
# Dot-source this file: . "$(dirname "$0")/loop-common.sh"

LOOP_SCHEMA_VERSION=1

repo_root() {
    # scripts/ lives directly under the repo root.
    cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd
}

state_path() {
    echo "$(repo_root)/.loop/state.json"
}

# Prints the current state.json as JSON on stdout, or nothing if it does not exist.
read_state() {
    local p
    p="$(state_path)"
    if [ ! -f "$p" ]; then
        return 0
    fi
    cat "$p"
}

# new_state <phase> <packages> <spec> <design> <base>
# Prints a fresh state object (lastTest/lastReview/override = null).
new_state() {
    local phase="$1" packages="$2" spec="$3" design="$4" base="$5"
    node -e '
        const [schemaVersion, phase, packages, spec, design, base] = process.argv.slice(1);
        console.log(JSON.stringify({
            schemaVersion: Number(schemaVersion),
            phase: Number(phase),
            args: { packages, spec, design, base },
            lastTest: null,
            lastReview: null,
            override: null,
        }, null, 2));
    ' "$LOOP_SCHEMA_VERSION" "$phase" "$packages" "$spec" "$design" "$base"
}

# state_set_args <state-json> <packages> <spec> <design> <base>
state_set_args() {
    local state_json="$1" packages="$2" spec="$3" design="$4" base="$5"
    node -e '
        const state = JSON.parse(process.argv[1]);
        const [, , packages, spec, design, base] = process.argv;
        state.args = { packages, spec, design, base };
        console.log(JSON.stringify(state, null, 2));
    ' "$state_json" "$packages" "$spec" "$design" "$base"
}

# state_set_last_test <state-json> <result: pass|fail> <treeHash> <at>
state_set_last_test() {
    local state_json="$1" result="$2" tree_hash="$3" at="$4"
    node -e '
        const state = JSON.parse(process.argv[1]);
        const [, , result, treeHash, at] = process.argv;
        state.lastTest = { result, treeHash, at };
        console.log(JSON.stringify(state, null, 2));
    ' "$state_json" "$result" "$tree_hash" "$at"
}

# state_set_last_review <state-json> <verdict: PASS|BLOCK> <findings> <round> <at>
state_set_last_review() {
    local state_json="$1" verdict="$2" findings="$3" round="$4" at="$5"
    node -e '
        const state = JSON.parse(process.argv[1]);
        const [, , verdict, findings, round, at] = process.argv;
        state.lastReview = { verdict, findings: Number(findings), round: Number(round), at };
        state.override = null;
        console.log(JSON.stringify(state, null, 2));
    ' "$state_json" "$verdict" "$findings" "$round" "$at"
}

# state_set_override <state-json> <reason> <at>
state_set_override() {
    local state_json="$1" reason="$2" at="$3"
    node -e '
        const state = JSON.parse(process.argv[1]);
        const [, , reason, at] = process.argv;
        state.override = { reason, at };
        console.log(JSON.stringify(state, null, 2));
    ' "$state_json" "$reason" "$at"
}

# state_get <state-json> <field, e.g. "phase" or "lastTest.result">
state_get() {
    local state_json="$1" field="$2"
    node -e '
        const state = JSON.parse(process.argv[1]);
        const path = process.argv[2].split(".");
        let v = state;
        for (const k of path) { v = (v === null || v === undefined) ? undefined : v[k]; }
        if (v === undefined || v === null) { process.exit(1); }
        console.log(typeof v === "string" ? v : JSON.stringify(v));
    ' "$state_json" "$field"
}

write_state() {
    local state_json="$1"
    local p dir
    p="$(state_path)"
    dir="$(dirname "$p")"
    mkdir -p "$dir"
    printf '%s' "$state_json" >"$p"
}

sha256_hex() {
    shasum -a 256 | awk '{print $1}'
}

# Working-tree hash, as defined in gate.ps1's header comment:
#   tracked changes : SHA-256 of `git diff HEAD --binary` ("" if HEAD does not exist yet)
#   untracked files : per file (gitignored excluded), SHA-256 of "<relative path>\n<content bytes>"
#   result          : SHA-256 of the sorted component hashes joined by "\n"
#   .loop/ itself is excluded, so writing state.json never changes the hash.
get_tree_hash() {
    local root parts_file
    root="$(repo_root)"
    parts_file="$(mktemp)"
    (
        cd "$root" || exit 1
        if git rev-parse --verify HEAD >/dev/null 2>&1; then
            diff_hash="$(git diff HEAD --binary -- . ":(exclude).loop" | sha256_hex)"
        else
            diff_hash="$(printf '' | sha256_hex)"
        fi
        echo "tracked:$diff_hash"

        git ls-files --others --exclude-standard | while IFS= read -r rel; do
            [ -z "$rel" ] && continue
            case "$rel" in .loop/*) continue ;; esac
            [ -f "$rel" ] || continue
            h="$( { printf '%s\n' "$rel"; cat "$rel"; } | sha256_hex )"
            echo "untracked:$h"
        done
    ) >"$parts_file"
    sort "$parts_file" | sha256_hex
    rm -f "$parts_file"
}

now_iso() {
    date -u +%Y-%m-%dT%H:%M:%SZ
}

assert_git_repo() {
    if ! git -C "$(repo_root)" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
        echo "not a git repository: run 'git init' in the repo root first" >&2
        exit 1
    fi
}
