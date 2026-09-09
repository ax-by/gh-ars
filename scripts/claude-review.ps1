<#
.SYNOPSIS
  Run a Claude Code (opus 5, medium effort) read-only code review against docs/SPEC.md and docs/DESIGN.md.

.DESCRIPTION
  Same contract as codex-review.ps1: same prompt template (docs/review/PROMPT.md), same parameters,
  same output shape ("## Verdict" + findings), so gate.ps1 can consume either reviewer.

  The review runs in a NEW headless session (`claude -p`) that shares no context with the session
  that wrote the code -- the reviewer starts from the repo and the rendered prompt only.

.EXAMPLE
  .\scripts\claude-review.ps1 -Phase "Phase 10" -Packages "internal/machine" `
      -Spec "S7.1-3, S10.2" -Design "S7" -Base HEAD~1

  Omit -Base (or pass "none") to review the listed paths in full instead of a diff.
#>
[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string] $Phase,
    [Parameter(Mandatory)] [string] $Packages,
    [Parameter(Mandatory)] [string] $Spec,
    [Parameter(Mandatory)] [string] $Design,
    [string] $Base = "none",
    [string] $Model = "claude-opus-5",
    # high 는 패키지 하나에 12~15분이라 루프가 느려진다. 필요하면 -Effort high.
    [ValidateSet("low","medium","high","xhigh","max")] [string] $Effort = "medium",
    [int] $TimeoutSeconds = (45 * 60)
)

$ErrorActionPreference = "Stop"
$repo = Split-Path -Parent $PSScriptRoot
if (-not (Get-Command claude -ErrorAction SilentlyContinue)) { throw "claude CLI not found in PATH" }

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

$outDir = Join-Path $repo ".codex-review"   # shared review artifact dir (gitignored); both reviewers write here
New-Item -ItemType Directory -Force $outDir | Out-Null
$promptPath = Join-Path $outDir "prompt.md"
[System.IO.File]::WriteAllText($promptPath, $rendered, (New-Object System.Text.UTF8Encoding($false)))

# The full contract lives in a file so that XML tags and quotes never pass through a shell.
$instruction = "Read the file .codex-review/prompt.md in this repository and carry out the review it describes exactly, including its output contract. Do not modify any files. Print only the review."

# Read-only tool set: the reviewer inspects the tree and the diff, and must not write. Anything not
# listed is denied by the headless session instead of prompting.
# 쉼표로 구분한다: 공백 구분은 인자 분리로 `Bash(git log:*)` 같은 규칙이 깨진다.
$allowed = "Read,Grep,Glob,Bash(git diff:*),Bash(git show:*),Bash(git log:*),Bash(git status:*),Bash(git ls-files:*),Bash(rg:*),Bash(cat:*),Bash(sed:*),Bash(head:*),Bash(tail:*),Bash(nl:*),Bash(wc:*),Bash(ls:*),Bash(find:*),Bash(go doc:*)"
$denied = "Write,Edit,NotebookEdit,Task,Agent,WebFetch,WebSearch"

Write-Host "claude-review: phase=$Phase packages=$Packages base=$Base model=$Model effort=$Effort"
Set-Location $repo
$stdoutPath = Join-Path $outDir "claude.stdout.txt"
$stderrPath = Join-Path $outDir "claude.stderr.txt"
Remove-Item $stdoutPath, $stderrPath -ErrorAction SilentlyContinue

# --strict-mcp-config keeps the reviewer hermetic (no user MCP servers). No -Resume/-Continue:
# the session starts empty, so the reviewer shares no context with the implementer.
$claudeArgs = @(
    "-p", "`"$instruction`"",
    "--model", $Model,
    "--effort", $Effort,
    "--output-format", "text",
    "--strict-mcp-config",
    "--allowedTools", "`"$allowed`"",
    "--disallowedTools", "`"$denied`""
)
# stdin 은 NUL 로 막는다: 열려 있으면 `claude -p` 가 파이프 입력을 3초 기다린다.
$proc = Start-Process -FilePath "claude" -ArgumentList $claudeArgs -WorkingDirectory $repo -NoNewWindow -PassThru `
    -RedirectStandardInput "NUL" -RedirectStandardOutput $stdoutPath -RedirectStandardError $stderrPath

if (-not $proc.WaitForExit($TimeoutSeconds * 1000)) {
    Write-Host "claude-review: timeout after $TimeoutSeconds s; terminating"
    Stop-Process -Id $proc.Id -Force -ErrorAction SilentlyContinue
    $proc.WaitForExit()
}
if (Test-Path $stdoutPath) { Get-Content $stdoutPath -Raw -Encoding utf8 | Write-Output }
# stderr only matters when the run failed; a successful review prints nothing there.
if ($proc.ExitCode -ne 0 -and (Test-Path $stderrPath)) { Get-Content $stderrPath -Raw -Encoding utf8 | Write-Error }
exit $proc.ExitCode
