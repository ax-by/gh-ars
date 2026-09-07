#!/usr/bin/env bash
# Claude Code SessionStart hook: print a one-line summary of .loop/state.json. Silent if the file is
# absent. Mac/bash port of session-start.ps1.
STATE_PATH="$(cd "$(dirname "$0")/../.." && pwd)/.loop/state.json"
[ -f "$STATE_PATH" ] || exit 0

node -e '
    const fs = require("fs");
    let s;
    try { s = JSON.parse(fs.readFileSync(process.argv[1], "utf8")); } catch (e) { process.exit(0); }

    let test = "test=없음";
    if (s.lastTest) test = `test=${s.lastTest.result}@${s.lastTest.at}`;
    let review = "review=없음";
    if (s.lastReview) review = `review=${s.lastReview.verdict} 지적${s.lastReview.findings} 회차${s.lastReview.round}@${s.lastReview.at}`;
    let ovr = "";
    if (s.override) ovr = ` override="${s.override.reason}"`;
    let pk = "";
    if (s.args) pk = ` packages=${s.args.packages}`;

    console.log(`[loop] phase=${s.phase}${pk} ${test} ${review}${ovr}. 실제 진척은 git status 와 ./scripts/gate.sh --test-only 로 확인할 것.`);
' "$STATE_PATH"
exit 0
