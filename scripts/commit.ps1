<#
.SYNOPSIS
  Gated commit for one PLAN.md Phase. Refuses unless .loop/state.json proves the gate passed on this exact tree.

.DESCRIPTION
  Conditions (all must hold unless -Override is given for the review condition):
    1. .loop/state.json exists and state.phase == -Phase
    2. state.lastTest.result == "pass"
    3. current working-tree hash == state.lastTest.treeHash   (nothing changed since the tests ran)
    4. state.lastReview.verdict == "PASS"                       (or -Override "<reason>")
  Phase 1..3: print the git commands and exit 0 without committing (a human reviews the diff and commits).
  Phase 4+  : update the Phase's 상태 cell in PLAN.md (warn-only if the row is not found), then git add -A, git commit.
  -Override records {reason, at} in state.json and adds an "Override: <reason>" line to the commit body.

  The PreToolUse hook (scripts/hooks/deny-direct-commit.ps1) blocks `git commit` issued directly from the
  agent's shell tool. This script's own `git commit` is a child process of powershell.exe, not a tool call,
  so the hook never sees it; additionally GH_ARS_LOOP_COMMIT=1 is set in the child environment as a marker
  for any future git-side hook.

.EXAMPLE
  .\scripts\commit.ps1 -Phase 3 -Message "feat(plan): capacity, desired, spread, reconcile (Phase 3, SPEC §8)"
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [int] $Phase,
    [Parameter(Mandatory)] [string] $Message,
    [string] $Override = ""
)

$ErrorActionPreference = "Stop"
. (Join-Path $PSScriptRoot "loop-common.ps1")
$repo = Get-RepoRoot
Assert-GitRepo
Set-Location $repo

$state = Read-State
$problems = @()
if ($null -eq $state) {
    $problems += "state.json 없음: 먼저 .\scripts\gate.ps1 -Phase $Phase ... 를 실행"
} else {
    if ($state.phase -ne $Phase) { $problems += "state.phase=$($state.phase) 이지만 요청은 -Phase $Phase" }
    if ($null -eq $state.lastTest -or $state.lastTest.result -ne "pass") { $problems += "lastTest 가 pass 가 아님 (gate.ps1 을 다시 실행)" }
    else {
        $now = Get-TreeHash
        if ($now -ne $state.lastTest.treeHash) {
            $problems += "작업 트리가 마지막 테스트 이후 바뀜 (treeHash $($state.lastTest.treeHash.Substring(0,12)) → $($now.Substring(0,12))). gate.ps1 을 다시 실행"
        }
    }
    if ($Override) {
        if ($null -eq $state.lastReview) { $problems += "-Override 는 리뷰가 한 번 이상 돈 뒤에만 허용" }
    } else {
        if ($null -eq $state.lastReview -or $state.lastReview.verdict -ne "PASS") {
            $v = "(없음)"; if ($null -ne $state.lastReview) { $v = $state.lastReview.verdict }
            $problems += "lastReview.verdict=$v (PASS 필요. 지적을 해결하고 gate.ps1 재실행, 또는 -Override '<이유>')"
        }
    }
}
if ($problems.Count -gt 0) {
    Write-Host "commit: 거부"
    $problems | ForEach-Object { Write-Host "  - $_" }
    exit 1
}

$body = $Message
if ($Override) {
    $state.override = [pscustomobject]@{ reason = $Override; at = Get-NowIso }
    Write-State $state
    $body = $Message + "`n`nOverride: " + $Override
}
$body = $body + "`n`nCo-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"

if ($Phase -le 3) {
    Write-Host "commit: Phase $Phase 은 사람이 diff 확인 후 직접 커밋한다. 게이트 조건은 충족됨. 실행할 명령:"
    Write-Host "  git add -A"
    Write-Host "  git commit -F <메시지 파일>   # 메시지:"
    ($body -split "`n") | ForEach-Object { Write-Host "    | $_" }
    exit 0
}

# --- PLAN.md: update the 상태 cell of row "| N | ..." in the Phase table BEFORE staging, so the
#     status change rides in the same commit. Warn-only if the row is not found.
$planPath = Join-Path $repo "PLAN.md"
$planText = [System.IO.File]::ReadAllText($planPath, (Get-Utf8NoBom))
$planLines = $planText -split "`r?`n"
$updated = $false
for ($i = 0; $i -lt $planLines.Length; $i++) {
    if ($planLines[$i] -match "^\|\s*$Phase\s*\|") {
        $cells = $planLines[$i] -split '\|'
        # cells[0] is empty (leading '|'); the cell before the trailing '|' is 상태
        if ($cells.Length -ge 7) {
            $cells[$cells.Length - 2] = " 완료 " + (Get-Date).ToString("yyyy-MM-dd") + " "
            $planLines[$i] = ($cells -join '|')
            $updated = $true
        }
        break
    }
}
if ($updated) {
    [System.IO.File]::WriteAllText($planPath, ($planLines -join "`n"), (Get-Utf8NoBom))
    Write-Host "commit: PLAN.md Phase $Phase 상태 갱신"
} else {
    Write-Host "commit: 경고, PLAN.md 에서 Phase $Phase 행을 찾지 못해 상태를 갱신하지 않음"
}

$msgFile = Join-Path $repo ".loop\commit-msg.txt"
[System.IO.File]::WriteAllText($msgFile, $body, (Get-Utf8NoBom))
$env:GH_ARS_LOOP_COMMIT = "1"
try {
    & git add -A
    if ($LASTEXITCODE -ne 0) { throw "git add failed ($LASTEXITCODE)" }
    & git commit -F $msgFile
    if ($LASTEXITCODE -ne 0) { throw "git commit failed ($LASTEXITCODE)" }
} finally {
    Remove-Item -Force $msgFile -ErrorAction SilentlyContinue
    Remove-Item Env:GH_ARS_LOOP_COMMIT -ErrorAction SilentlyContinue
}
Write-Host "commit: 완료 ($(git rev-parse --short HEAD))"
exit 0
