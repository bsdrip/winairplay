# Builds the winairplay programs into the bin directory.
# Usage:  powershell -ExecutionPolicy Bypass -File build.ps1
$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force -Path bin | Out-Null

# The tray application is a GUI program: it must not open a console window.
go build -trimpath -ldflags "-H windowsgui -s -w" -o bin\winairplay.exe .\cmd\winairplay
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

go build -trimpath -o bin\winairplay-cli.exe .\cmd\winairplay-cli
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

go build -trimpath -o bin\winairplay-test-receiver.exe .\cmd\winairplay-test-receiver
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "Build complete. Start bin\winairplay.exe"
