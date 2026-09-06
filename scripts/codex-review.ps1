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
& node @args
exit $LASTEXITCODE
