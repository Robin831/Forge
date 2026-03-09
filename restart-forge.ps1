# Restart the Forge daemon with latest code from main.
# Usage: .\restart-forge.ps1

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Push-Location $PSScriptRoot
try {
    Write-Host "Stopping Forge..." -ForegroundColor Yellow
    forge down

    # Wait for the forge process to exit
    Write-Host "Waiting for Forge process to exit..." -ForegroundColor Yellow
    while (Get-Process -Name forge -ErrorAction SilentlyContinue) {
        Write-Host "." -NoNewline
        Start-Sleep -Seconds 1
    }
    Write-Host " done"

    Write-Host "Pulling latest..." -ForegroundColor Yellow
    git pull

    Write-Host "Building..." -ForegroundColor Yellow
    go install .\cmd\forge\

    Write-Host "Starting Forge..." -ForegroundColor Green
    forge up

    Start-Sleep -Seconds 2
    forge hearth
}
finally {
    Pop-Location
}
