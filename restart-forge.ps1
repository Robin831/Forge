# Restart the Forge daemon with latest code from main.
# Usage: .\restart-forge.ps1

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

Push-Location $PSScriptRoot
try {
    Write-Host "Stopping Forge..." -ForegroundColor Yellow
    forge down

    Write-Host "Pulling latest..." -ForegroundColor Yellow
    git pull

    Write-Host "Building..." -ForegroundColor Yellow
    go install .\cmd\forge\

    Write-Host "Waiting 30s for cleanup..." -ForegroundColor Yellow
    foreach ($i in 30..1) {
        Write-Host "`r$i seconds remaining..." -NoNewline
        Start-Sleep -Seconds 1
    }
    Write-Host ""

    Write-Host "Starting Forge..." -ForegroundColor Green
    forge up

    Start-Sleep -Seconds 2
    forge hearth
}
finally {
    Pop-Location
}
