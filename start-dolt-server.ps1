#!/usr/bin/env pwsh
# LOCAL DEVELOPMENT FALLBACK — Start a local Dolt SQL Server for beads issue tracking
#
# NOTE: The PRIMARY setup uses the shared Dolt server in AKS via kubectl port-forward.
# See .beads/README.md for the standard developer setup (no local Dolt server needed).
#
# Use this script ONLY if you need an isolated local Dolt instance (e.g. offline dev,
# CI environments without kubectl access, or testing migrations locally).
# It runs on port 3307 to avoid conflicting with the shared server port-forward (3306).
#
# Usage:
#   .\start-dolt-server.ps1
#
# Or run in background:
#   Start-Job -ScriptBlock { Set-Location $using:PSScriptRoot; .\start-dolt-server.ps1 }
#
# If using this script, update .beads/metadata.json dolt_server_port to 3307 locally
# (do not commit that change).
#
# NOTE: Requires the beads/ Dolt database to be initialized.
# If beads/ is missing, clone the repo and run bd hooks install, then bd dolt pull.

$ErrorActionPreference = 'Stop'

# Check if Dolt is installed
if (-not (Get-Command dolt -ErrorAction SilentlyContinue)) {
    Write-Error "Dolt not found. Install it first: https://github.com/dolthub/dolt/releases"
    exit 1
}

# Check if port 3307 is already in use (and verify it's owned by dolt, not another process)
$portInUse = Get-NetTCPConnection -LocalPort 3307 -State Listen -ErrorAction SilentlyContinue
if ($portInUse) {
    $owningProcess = Get-Process -Id $portInUse.OwningProcess -ErrorAction SilentlyContinue
    if ($owningProcess -and $owningProcess.ProcessName -match '^dolt(\.exe)?$') {
        Write-Host "✓ Dolt server already running on port 3307" -ForegroundColor Green
        exit 0
    }
    else {
        $procName = if ($owningProcess) { $owningProcess.ProcessName } else { "unknown" }
        Write-Error "Port 3307 is already in use by '$procName' (PID $($portInUse.OwningProcess)), not Dolt. Stop that process first."
        exit 1
    }
}

# Change to the beads database directory
$beadsDir = Join-Path $PSScriptRoot "beads"
if (-not (Test-Path $beadsDir)) {
    Write-Error "beads/ directory not found at $beadsDir. Run 'bd dolt pull' to initialize the local Dolt database."
    Write-Host "Hint: After cloning, run: bd hooks install && bd dolt pull" -ForegroundColor Yellow
    exit 1
}

Write-Host "Starting Dolt SQL server..." -ForegroundColor Cyan
Write-Host "  Database: $beadsDir" -ForegroundColor Gray
Write-Host "  Host: 127.0.0.1" -ForegroundColor Gray
Write-Host "  Port: 3307" -ForegroundColor Gray
Write-Host ""
Write-Host "Press Ctrl+C to stop the server" -ForegroundColor Yellow
Write-Host ""

Set-Location $beadsDir

# Start the server (runs in foreground)
dolt sql-server --port 3307 --host 127.0.0.1
