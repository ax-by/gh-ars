# Shared helpers for the implementation-loop gate (gate.ps1, commit.ps1, hooks).
# Dot-source this file. Requires PowerShell 5.1+ and git. No other runtime dependencies.

Set-StrictMode -Version 2.0

$script:LoopSchemaVersion = 1

function Get-RepoRoot {
    # scripts/ lives directly under the repo root.
    return (Split-Path -Parent $PSScriptRoot)
}

function Get-StatePath {
    return (Join-Path (Get-RepoRoot) ".loop\state.json")
}

function Get-Utf8NoBom {
    return (New-Object System.Text.UTF8Encoding($false))
}

function Read-State {
    $p = Get-StatePath
    if (-not (Test-Path $p)) { return $null }
    $raw = [System.IO.File]::ReadAllText($p, (Get-Utf8NoBom))
    return ($raw | ConvertFrom-Json)
}

function New-State {
    # Note: the parameter is not named $Args because $args is a PowerShell automatic variable.
    param([int] $Phase, [hashtable] $PhaseArgs)
    return [pscustomobject]@{
        schemaVersion = $script:LoopSchemaVersion
        phase         = $Phase
        args          = [pscustomobject]$PhaseArgs
        lastTest      = $null
        lastReview    = $null
        override      = $null
    }
}

function Write-State {
    param([Parameter(Mandatory)] $State)
    $p = Get-StatePath
    $dir = Split-Path -Parent $p
    if (-not (Test-Path $dir)) { New-Item -ItemType Directory -Force $dir | Out-Null }
    $json = $State | ConvertTo-Json -Depth 6
    [System.IO.File]::WriteAllText($p, $json, (Get-Utf8NoBom))
}

function Get-Sha256Hex {
    param([byte[]] $Bytes)
    $sha = [System.Security.Cryptography.SHA256]::Create()
    try {
        $hash = $sha.ComputeHash($Bytes)
        return ([System.BitConverter]::ToString($hash)).Replace("-", "").ToLowerInvariant()
    } finally { $sha.Dispose() }
}

function Get-TreeHash {
    # Working-tree hash, as defined in gate.ps1's header comment:
    #   tracked changes : SHA-256 of `git diff HEAD` (binary-safe), or of "" when HEAD does not exist yet
    #   untracked files : per file, SHA-256 of "<relative path>\n<content bytes>" (gitignored files excluded)
    #   result          : SHA-256 of the sorted list of all component hashes joined by "\n"
    # Anything under .loop/ is excluded so that writing state.json never changes the hash.
    $root = Get-RepoRoot
    Push-Location $root
    # Native git stderr must not become a terminating error under $ErrorActionPreference = "Stop" (PS 5.1).
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        $parts = New-Object System.Collections.Generic.List[string]

        & git rev-parse --verify HEAD 2>&1 | Out-Null
        if ($LASTEXITCODE -eq 0) {
            $diff = & git diff HEAD --binary -- . ":(exclude).loop"
            $diffText = ($diff -join "`n")
        } else {
            $diffText = ""
        }
        $parts.Add("tracked:" + (Get-Sha256Hex ([System.Text.Encoding]::UTF8.GetBytes($diffText))))

        $untracked = & git ls-files --others --exclude-standard
        foreach ($rel in @($untracked)) {
            if (-not $rel) { continue }
            if ($rel -like ".loop/*") { continue }
            $full = Join-Path $root $rel
            if (-not (Test-Path $full -PathType Leaf)) { continue }
            $content = [System.IO.File]::ReadAllBytes($full)
            $prefix = [System.Text.Encoding]::UTF8.GetBytes($rel + "`n")
            $buf = New-Object byte[] ($prefix.Length + $content.Length)
            [Array]::Copy($prefix, 0, $buf, 0, $prefix.Length)
            [Array]::Copy($content, 0, $buf, $prefix.Length, $content.Length)
            $parts.Add("untracked:" + (Get-Sha256Hex $buf))
        }

        $sorted = $parts.ToArray() | Sort-Object
        return (Get-Sha256Hex ([System.Text.Encoding]::UTF8.GetBytes(($sorted -join "`n"))))
    } finally {
        $ErrorActionPreference = $prevEap
        Pop-Location
    }
}

function Get-NowIso {
    return (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
}

function Assert-GitRepo {
    Push-Location (Get-RepoRoot)
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = "Continue"
    try {
        & git rev-parse --is-inside-work-tree 2>&1 | Out-Null
        if ($LASTEXITCODE -ne 0) { throw "not a git repository: run 'git init' in the repo root first" }
    } finally {
        $ErrorActionPreference = $prevEap
        Pop-Location
    }
}
