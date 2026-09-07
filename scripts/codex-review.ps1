<#
.SYNOPSIS
  Run a Codex (gpt-6-astra, medium) read-only code review against docs/SPEC.md and docs/DESIGN.md.

.EXAMPLE
  .\scripts\codex-review.ps1 -Phase "1 pure core" -Packages "internal/resource,internal/domain" `
      -Spec "§4, §8.1, §9.3, R25" -Design "§3.1, §5" -Base HEAD~1

  Omit -Base (or pass "none") to review the listed paths in full instead of a diff.
  Add -Background to detach; then use /codex:status and /codex:result.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $Phase,
    [Parameter(Mandatory)] [string] $Packages,
    [Parameter(Mandatory)] [string] $Spec,
    [Parameter(Mandatory)] [string] $Design,
    [string] $Base = "none",
    [string] $Model = "gpt-6-astra",
    [ValidateSet("none","minimal","low","medium","high","xhigh")] [string] $Effort = "medium",
    [switch] $Background
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
$plugin = Join-Path $env:USERPROFILE ".claude\plugins\cache\openai-codex\codex\1.0.6"
$companion = Join-Path $plugin "scripts\codex-companion.mjs"
if (-not (Test-Path $companion)) { throw "codex companion not found: $companion" }

# Section references may be passed in ASCII as "S8.1" (for "§8.1") because "§" does not survive the
# PowerShell 5.1 console codepage when arguments come from a tool call. Transliterate here.
$Spec = $Spec -replace 'S(?=\d)', '§'
$Design = $Design -replace 'S(?=\d)', '§'

$template = Get-Content -Raw -Encoding utf8 (Join-Path $repo "docs\review\PROMPT.md")
$rendered = $template.
    Replace("{{PHASE}}", $Phase).
    Replace("{{PACKAGES}}", $Packages).
    Replace("{{SPEC_SECTIONS}}", $Spec).
    Replace("{{DESIGN_SECTIONS}}", $Design).
    Replace("{{BASE}}", $Base)

$outDir = Join-Path $repo ".codex-review"
New-Item -ItemType Directory -Force $outDir | Out-Null
$promptPath = Join-Path $outDir "prompt.md"
[System.IO.File]::WriteAllText($promptPath, $rendered, (New-Object System.Text.UTF8Encoding($false)))

# The full contract lives in a file so that XML tags and quotes never pass through a shell.
$instruction = "Read the file .codex-review/prompt.md in this repository and carry out the review it describes exactly, including its output contract. Do not modify any files."

$args = @("`"$companion`"", "task", "--model", $Model, "--effort", $Effort)
if ($Background) { $args += "--background" }
$args += "`"$instruction`""

Write-Host "codex-review: phase=$Phase packages=$Packages base=$Base model=$Model effort=$Effort"
Set-Location $repo
# The companion prints the rendered result only after `codex app-server` exits. When the review used
# tools, codex leaves `codex-code-mode-host` / `node_repl` grandchildren holding the app-server pipes,
# so the app-server never exits and the companion waits forever (its own teardown only fires on an
# explicit close). Run node detached, watch its stderr for the companion's completion line, and if it
# is still alive after a grace period, kill the descendants below node (deepest first). The app-server's
# exit then unblocks the companion, which flushes the result and exits by itself.
$stdoutPath = Join-Path $outDir "companion.stdout.txt"
$stderrPath = Join-Path $outDir "companion.stderr.txt"
Remove-Item $stdoutPath, $stderrPath -ErrorAction SilentlyContinue
$proc = Start-Process -FilePath "node" -ArgumentList $args -WorkingDirectory $repo -NoNewWindow -PassThru `
    -RedirectStandardOutput $stdoutPath -RedirectStandardError $stderrPath

$completionMarker = "Turn completion inferred"
$graceAfterDone = 30      # seconds to let the companion exit on its own after the marker
$hardLimit = 45 * 60      # seconds for the whole review
$doneAt = $null
$started = Get-Date
$killedTree = $false

function Get-DescendantPids([int] $parentPid) {
    $out = @()
    $children = Get-CimInstance Win32_Process -Filter "ParentProcessId = $parentPid" -ErrorAction SilentlyContinue
    foreach ($c in $children) { $out += Get-DescendantPids ([int]$c.ProcessId); $out += [int]$c.ProcessId }
    return $out
}

while (-not $proc.HasExited) {
    Start-Sleep -Seconds 2
    $now = Get-Date
    if ($null -eq $doneAt -and (Test-Path $stderrPath)) {
        if ((Get-Content $stderrPath -Raw -ErrorAction SilentlyContinue) -match [regex]::Escape($completionMarker)) { $doneAt = $now }
    }
    $overGrace = ($null -ne $doneAt) -and (($now - $doneAt).TotalSeconds -ge $graceAfterDone)
    $overLimit = ($now - $started).TotalSeconds -ge $hardLimit
    if (($overGrace -or $overLimit) -and -not $killedTree) {
        $killedTree = $true
        $desc = Get-DescendantPids $proc.Id   # deepest first
        Write-Host "codex-review: companion did not exit ($(if ($overLimit) { 'hard limit' } else { 'after completion' })); terminating $($desc.Count) descendant process(es)"
        foreach ($d in $desc) { Stop-Process -Id $d -Force -ErrorAction SilentlyContinue }
        $proc.WaitForExit(30000) | Out-Null
        if (-not $proc.HasExited) { Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue; $proc.WaitForExit() }
    }
}
if (Test-Path $stdoutPath) { Get-Content $stdoutPath -Raw -Encoding utf8 | Write-Output }
if (Test-Path $stderrPath) { Get-Content $stderrPath -Raw -Encoding utf8 | Write-Output }
exit $proc.ExitCode
exit $LASTEXITCODE
