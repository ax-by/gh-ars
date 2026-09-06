# Wrapper for the Go toolchain, which is not on PATH on this machine.
# Usage: .\scripts\go.ps1 test ./...
$go = "C:\Users\user\sdk\go1.27.0\bin\go.exe"
if (-not (Test-Path $go)) { throw "go.exe not found: $go" }
& $go @args
exit $LASTEXITCODE
