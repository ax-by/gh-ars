<task>
You are reviewing Go code in this repository (module `gh-ars`), a GitHub Actions self-hosted runner autoscaler that uses `github.com/actions/scaleset` (v0.4.0) and its `listener` package to run ephemeral runners as docker/podman containers on static machines over SSH, without Kubernetes.

Review scope for this run:
- Phase: {{PHASE}}
- Packages / paths: {{PACKAGES}}
- Diff base: {{BASE}} (if "none", review the listed paths in full; otherwise review `git diff {{BASE}}..HEAD -- <paths>` plus any untracked files under those paths)

Normative documents (read them first, they are the source of truth):
- `docs/SPEC.md` — what must be true. Relevant sections for this run: {{SPEC_SECTIONS}}
- `docs/DESIGN.md` — how it must be structured. Relevant sections: {{DESIGN_SECTIONS}}
If SPEC.md and DESIGN.md conflict, SPEC.md wins.

Review on exactly two axes:
1. Document conformance. Does the code implement the referenced SPEC rules (R-numbers, section numbers) and the DESIGN interfaces, state transitions, message flows, and package boundaries exactly? Flag missing rules, extra behavior not in the documents, wrong defaults, wrong constants, wrong ordering (for example cleanup order, create→cp→start), and any place where a test name claims a rule it does not actually verify.
2. Fitness as an actions/scaleset-based autoscaler. Is the code correct for how the Runner Scale Set protocol and `listener` actually behave: `HandleDesiredRunnerCount` receives `TotalAssignedJobs` on every message, `SetMaxRunners` feeds the next `GetMessage` maxCapacity, `GetRunnerByName` returns `(nil, nil)` when absent, `RemoveRunner` wraps `RunnerNotFoundError` / `JobStillRunningError`, JIT config must never appear in argv or container env, ephemeral runners exit after one job. Also check concurrency (single controller goroutine, no shared mutable state across goroutines), context/timeout handling, error wrapping, idempotent cleanup, and resource leaks (SSH sessions, event streams, goroutines).
</task>

<grounding_rules>
Ground every finding in a specific file and line range you inspected, and in a specific SPEC/DESIGN reference or a specific actions/scaleset behavior. Quote the document sentence or rule id you are comparing against.
Do not present inferences as facts. If a point is a hypothesis, label it "hypothesis".
Do not invent SPEC rules. If the code is right and the document is wrong or silent, report it as category `doc-gap`, not as a code defect.
Read `go.mod` and the vendored or cached `github.com/actions/scaleset` sources if you need to confirm library behavior; do not guess signatures.
</grounding_rules>

<dig_deeper_nudge>
After the first plausible issue, check second-order failures: empty state (zero machines healthy, capacity 0), restarts and adoption, retries after partial cleanup, stale cached state versus machine truth, race between `die` events and statistics messages, and rollback paths in `startUnit`.
</dig_deeper_nudge>

<structured_output_contract>
Return exactly this shape and nothing else.

## Verdict
One line: `PASS` (no blocking findings) or `BLOCK` (at least one `blocker`).

## Findings
Ordered by severity: blocker, major, minor, doc-gap. For each:
- **[severity] [category] file:line-range** — one-sentence defect statement.
  - Reference: SPEC §x / Rn / DESIGN §y, or the actions/scaleset behavior.
  - Evidence: the code fact you observed (quote ≤ 3 lines).
  - Fix: the smallest concrete change.

If there are no findings in a severity class, omit that class. If there are none at all, write `No findings.` under this heading.

## Coverage
Bulleted list of the SPEC rules / DESIGN sections in scope that you verified as implemented, so the author can see what was checked rather than only what failed.

Do not restate the task. Do not include general praise. Do not propose refactors unrelated to the two axes. Do not modify files.
</structured_output_contract>
