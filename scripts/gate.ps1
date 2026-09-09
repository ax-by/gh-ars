<#
.SYNOPSIS
  Implementation-loop gate: go test -> go vet -> review (Codex or Claude) -> .loop/state.json.

.DESCRIPTION
  Runs the deterministic checks for one PLAN.md Phase and records the outcome in .loop/state.json
  so that commit.ps1 (and the next session) can verify the gate without trusting memory.

  Working-tree hash definition (shared with commit.ps1 via loop-common.ps1):
    tracked changes : SHA-256 of `git diff HEAD --binary` ("" if HEAD does not exist yet)
    untracked files : per file (gitignored excluded), SHA-256 of "<relative path>\n<content bytes>"
    result          : SHA-256 of the sorted component hashes joined by "\n"
    .loop/ itself is excluded, so writing state.json never changes the hash.

  Review round: increments each time a review runs for the same -Phase; resets on PASS or when -Phase changes.
  Verdict parsing: exactly one "## Verdict" heading followed by a line that is PASS or BLOCK. Missing,
  ambiguous, or duplicated verdicts are treated as BLOCK (fail closed).

.EXAMPLE
  .\scripts\gate.ps1 -Phase 3 -Packages "internal/plan" -Spec "S7.2-3, S8.1-8.3" -Design "S5" -Base HEAD
  .\scripts\gate.ps1 -Phase 3 -Packages "internal/plan" -Spec "S8" -Design "S5" -TestOnly
  .\scripts\gate.ps1 -Phase 3 ... -Reviewer claude   # Claude Code (opus 5, high) 로 리뷰
  .\scripts\gate.ps1 -Phase 3 ... -ReviewFromFile .codex-review\last.md   # re-parse a saved review (no review call)

  Reviewer: -Reviewer codex|claude (default codex, or $env:GH_ARS_REVIEWER). Both render the same
  prompt template (docs/review/PROMPT.md) and produce the same output contract, so the verdict parsing
  does not care which one ran. Use claude when Codex quota is out (or to get a second opinion).
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [int] $Phase,
    [Parameter(Mandatory)] [string] $Packages,
    [Parameter(Mandatory)] [string] $Spec,
    [Parameter(Mandatory)] [string] $Design,
    [string] $Base = "none",
    [switch] $TestOnly,
    [string] $ReviewFromFile = "",
    [ValidateSet("codex","claude")] [string] $Reviewer = $(if ($env:GH_ARS_REVIEWER) { $env:GH_ARS_REVIEWER } else { "codex" })
)

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "loop-common.ps1")
$repo = Get-RepoRoot
Assert-GitRepo
Set-Location $repo

$goPs1 = Join-Path $PSScriptRoot "go.ps1"
if ($Reviewer -eq "claude") {
    $reviewPs1 = Join-Path $PSScriptRoot "claude-review.ps1"
    $reviewLabel = "claude review (opus 5, high)"
} else {
    $reviewPs1 = Join-Path $PSScriptRoot "codex-review.ps1"
    $reviewLabel = "codex review (gpt-6-astra, medium)"
}

# --- state: load or create; reset review round when the phase changes
$state = Read-State
$argsNow = @{ packages = $Packages; spec = $Spec; design = $Design; base = $Base }
if ($null -eq $state -or $state.phase -ne $Phase) {
    $state = New-State -Phase $Phase -PhaseArgs $argsNow
} else {
    $state.args = [pscustomobject]$argsNow
}

# --- tests
Write-Host "gate: phase=$Phase packages=$Packages reviewer=$Reviewer"
Write-Host "gate: go test ./..."
$prevEap = $ErrorActionPreference
$ErrorActionPreference = "Continue"
$testOut = & $goPs1 test ./... 2>&1 | ForEach-Object { "$_" }
$testCode = $LASTEXITCODE
$ErrorActionPreference = $prevEap
$testOut | ForEach-Object { Write-Host $_ }
# Bootstrap case: a repo with no Go packages yet. `go test ./...` exits 1 with "no packages to test";
# treat that as pass so the gate is usable before the first package lands. Never triggers once code exists.
if ($testCode -ne 0 -and ($testOut -join "`n") -match 'no packages to test') {
    Write-Host "gate: no Go packages yet, treating tests as pass"
    $testCode = 0
}
if ($testCode -eq 0) {
    Write-Host "gate: go vet ./..."
    $ErrorActionPreference = "Continue"
    $vetOut = & $goPs1 vet ./... 2>&1 | ForEach-Object { "$_" }
    $testCode = $LASTEXITCODE
    $ErrorActionPreference = $prevEap
    $vetOut | ForEach-Object { Write-Host $_ }
    if ($testCode -ne 0 -and ($vetOut -join "`n") -match 'no packages to vet') {
        Write-Host "gate: no Go packages yet, treating vet as pass"
        $testCode = 0
    }
}
$treeHash = Get-TreeHash
$state.lastTest = [pscustomobject]@{
    result   = $(if ($testCode -eq 0) { "pass" } else { "fail" })
    treeHash = $treeHash
    at       = Get-NowIso
}
Write-State $state
if ($testCode -ne 0) {
    Write-Host "gate: BLOCK, tests or vet failed (exit $testCode). Review skipped."
    exit 1
}
Write-Host "gate: tests pass. treeHash=$($treeHash.Substring(0,12))"

if ($TestOnly) {
    Write-Host "gate: -TestOnly, review skipped. state.phase=$Phase lastTest=pass"
    exit 0
}

# --- review
$reviewDir = Join-Path $repo ".codex-review"
New-Item -ItemType Directory -Force $reviewDir | Out-Null
$lastReviewPath = Join-Path $reviewDir "last.md"

if ($ReviewFromFile) {
    if (-not (Test-Path $ReviewFromFile)) { throw "review file not found: $ReviewFromFile" }
    $reviewText = [System.IO.File]::ReadAllText($ReviewFromFile, (Get-Utf8NoBom))
} else {
    Write-Host "gate: $reviewLabel ..."
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = "Continue"   # child stderr lines must not terminate the gate (PS 5.1)
    try {
        $out = & powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $reviewPs1 `
            -Phase "Phase $Phase" -Packages $Packages -Spec $Spec -Design $Design -Base $Base 2>&1 | ForEach-Object { "$_" }
        $reviewCode = $LASTEXITCODE
    } finally { $ErrorActionPreference = $prevEap }
    $reviewText = ($out -join "`n")
    [System.IO.File]::WriteAllText($lastReviewPath, $reviewText, (Get-Utf8NoBom))
    if ($reviewCode -ne 0) { Write-Host "gate: review command exited $reviewCode (output saved to .codex-review\last.md)" }
}

# --- verdict parsing (fail closed)
$lines = $reviewText -split "`r?`n"
$verdictHeadings = @()
for ($i = 0; $i -lt $lines.Length; $i++) { if ($lines[$i] -match '^\s*##\s+Verdict\s*$') { $verdictHeadings += $i } }
$verdict = "BLOCK"
$reason = ""
if ($verdictHeadings.Count -ne 1) {
    $reason = "verdict heading count = $($verdictHeadings.Count) (expected 1)"
} else {
    $v = ""
    for ($j = $verdictHeadings[0] + 1; $j -lt $lines.Length; $j++) {
        $t = $lines[$j].Trim()
        if ($t -eq "") { continue }
        $v = $t.Trim('`').Trim()
        break
    }
    if ($v -eq "PASS") { $verdict = "PASS" }
    elseif ($v -eq "BLOCK") { $verdict = "BLOCK" }
    else { $reason = "verdict line unrecognized: '$v'" }
}
$findings = @($lines | Where-Object { $_ -match '^\s*-\s+\*\*\[' }).Count

# --- round bookkeeping
$round = 1
if ($null -ne $state.lastReview -and $state.lastReview.verdict -ne "PASS") { $round = [int]$state.lastReview.round + 1 }
if ($verdict -eq "PASS") { $round = 0 }
$state.lastReview = [pscustomobject]@{
    verdict  = $verdict
    findings = $findings
    round    = $round
    at       = Get-NowIso
}
$state.override = $null
Write-State $state

if ($verdict -eq "PASS") {
    Write-Host "gate: PASS, 커밋 가능 (findings=$findings). next: .\scripts\commit.ps1 -Phase $Phase -Message '...'"
    exit 0
}
$roundText = "회차 ${round}/2"
if ($reason) { Write-Host "gate: BLOCK (fail closed: $reason), 지적 ${findings}개, $roundText" }
else { Write-Host "gate: BLOCK, 지적 ${findings}개, $roundText" }
if ($round -ge 2) { Write-Host "gate: 2회차 초과. 사람 결정 필요 (commit.ps1 -Override '<이유>' 또는 SPEC 개정)." }
Write-Host "gate: review saved to .codex-review\last.md"
exit 1
