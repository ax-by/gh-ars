# Claude Code PostToolUse hook (Edit|Write): gofmt -w the edited .go file, then go vet its package.
# Non-.go files: exit 0 silently. vet failure: message on stderr + exit 2 so Claude sees it.
# Invoked in exec form from .claude/settings.json (no shell); cwd is the project root.
$ErrorActionPreference = "Continue"
try {
    $raw = [Console]::In.ReadToEnd().TrimStart([char]0xFEFF)   # a PowerShell parent may prepend a BOM
    $f = ($raw | ConvertFrom-Json).tool_input.file_path
} catch { exit 0 }
if (-not $f -or -not $f.EndsWith(".go")) { exit 0 }

$bin = "C:\Users\user\sdk\go1.27.0\bin"
& "$bin\gofmt.exe" -l -w $f
Push-Location (Split-Path -Parent $f)
& "$bin\go.exe" vet .
$code = $LASTEXITCODE
Pop-Location
if ($code -ne 0) { exit 2 }
exit 0
