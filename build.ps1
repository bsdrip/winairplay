# Builds winairplay.exe into the bin directory.
# Usage:  powershell -ExecutionPolicy Bypass -File build.ps1
# The installer is built by GitHub Actions (.github/workflows/build.yml).
$ErrorActionPreference = "Stop"
New-Item -ItemType Directory -Force -Path bin | Out-Null

# The tray application is a GUI program: it must not open a console window.
go build -trimpath -ldflags "-H windowsgui -s -w" -o bin\winairplay.exe .\cmd\winairplay
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }

Write-Host "Build complete. Start bin\winairplay.exe (FFmpeg must be on PATH or next to it)."
