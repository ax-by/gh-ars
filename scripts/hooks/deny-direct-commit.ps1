# Claude Code PreToolUse hook (Bash|PowerShell): refuse `git commit` issued directly from the agent's shell tool.
#
# Distinguishing mechanism: the hook only sees the *tool call* text on stdin (tool_input.command). A commit
# made by scripts/commit.ps1 runs `git commit` as a child process of powershell.exe, which is never a tool
# call, so it is invisible to this hook and is not blocked. The only tool-call form that is allowed through
# is one that invokes scripts/commit.ps1 itself. commit.ps1 additionally sets GH_ARS_LOOP_COMMIT=1 in its
# child environment as a marker for any future git-side (pre-commit) hook; tool-level hooks cannot observe
# the environment of a command that has not run yet, which is why the command text is the discriminator here.
$ErrorActionPreference = "Continue"
# Claude Code reads hook stdout as UTF-8; the default console codepage would garble Korean text.
try { [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false) } catch {}
try { [Console]::InputEncoding = New-Object System.Text.UTF8Encoding($false) } catch {}   # may throw when stdin is a pipe
try {
    $raw = [Console]::In.ReadToEnd().TrimStart([char]0xFEFF)   # a PowerShell parent may prepend a BOM
    $cmd = [string]($raw | ConvertFrom-Json).tool_input.command
} catch { exit 0 }
if (-not $cmd) { exit 0 }

$isGitCommit = $cmd -match '(^|[\s;&|])git\s+(-C\s+\S+\s+)?commit(\s|$)'
$viaScript = $cmd -match 'commit\.ps1'
if ($isGitCommit -and -not $viaScript) {
    $out = @{
        hookSpecificOutput = @{
            hookEventName            = "PreToolUse"
            permissionDecision       = "deny"
            permissionDecisionReason = "직접 git commit 은 루프 게이트를 우회한다. .\scripts\gate.ps1 -Phase N ... 로 검증한 뒤 .\scripts\commit.ps1 -Phase N -Message '...' 를 사용할 것."
        }
    } | ConvertTo-Json -Compress -Depth 4
    [Console]::Out.Write($out)
}
exit 0
