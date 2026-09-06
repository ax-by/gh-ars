# Claude Code SessionStart hook: print a one-line summary of .loop/state.json. Silent if the file is absent.
$ErrorActionPreference = "Continue"
[Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)   # Claude Code reads hook stdout as UTF-8
$p = Join-Path (Split-Path -Parent (Split-Path -Parent $PSScriptRoot)) ".loop\state.json"
if (-not (Test-Path $p)) { exit 0 }
try {
    $s = [System.IO.File]::ReadAllText($p) | ConvertFrom-Json
} catch { exit 0 }

$test = "test=없음"
if ($null -ne $s.lastTest) { $test = "test=$($s.lastTest.result)@$($s.lastTest.at)" }
$review = "review=없음"
if ($null -ne $s.lastReview) { $review = "review=$($s.lastReview.verdict) 지적$($s.lastReview.findings) 회차$($s.lastReview.round)@$($s.lastReview.at)" }
$ovr = ""
if ($null -ne $s.override) { $ovr = " override='$($s.override.reason)'" }
$pk = ""
if ($null -ne $s.args) { $pk = " packages=$($s.args.packages)" }

Write-Output "[loop] phase=$($s.phase)$pk $test $review$ovr. 실제 진척은 git status 와 .\scripts\gate.ps1 -TestOnly 로 확인할 것."
exit 0
