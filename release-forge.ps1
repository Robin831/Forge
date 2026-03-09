# Release The Forge — tags a version and pushes to trigger the GitHub Actions release workflow.
# Usage: .\release-forge.ps1 <version>
# Example: .\release-forge.ps1 0.1.0

[CmdletBinding()]
param(
    [Parameter(Mandatory = $true, Position = 0)]
    [string]$Version,

    [Parameter()]
    [switch]$Open
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

# --- Helpers ---

function Write-Step($msg) { Write-Host "=> $msg" -ForegroundColor Cyan }
function Write-Ok($msg)   { Write-Host "   $msg" -ForegroundColor Green }
function Write-Err($msg)  { Write-Host "ERROR: $msg" -ForegroundColor Red; exit 1 }

# --- Validate semver ---

if ($Version -notmatch '^\d+\.\d+\.\d+(-[\w.]+)?(\+[\w.]+)?$') {
    Write-Err "Invalid version '$Version'. Expected semver format (e.g. 1.2.3, 0.1.0-beta.1)."
}

$Tag = "v$Version"

# --- Ensure we are in the repo root ---

Push-Location $PSScriptRoot
try {

    # --- Clean working tree ---

    Write-Step "Checking working tree..."
    $status = git status --porcelain
    if ($status) {
        Write-Err "Working tree is not clean. Commit or stash your changes first."
    }
    Write-Ok "Working tree is clean."

    # --- Ensure main branch ---

    Write-Step "Checking branch..."
    $branch = git rev-parse --abbrev-ref HEAD
    if ($branch -ne 'main') {
        Write-Err "Must be on 'main' branch (currently on '$branch')."
    }
    Write-Ok "On main branch."

    # --- Check tag doesn't already exist ---

    Write-Step "Checking tag $Tag..."
    $existingTag = git tag -l $Tag
    if ($existingTag) {
        Write-Err "Tag $Tag already exists."
    }
    Write-Ok "Tag $Tag is available."

    # --- Pull latest ---

    Write-Step "Pulling latest from origin..."
    git pull --rebase
    Write-Ok "Up to date."

    # --- Assemble changelog ---

    Write-Step "Assembling changelog..."
    forge changelog assemble
    if ($LASTEXITCODE -ne 0) {
        Write-Err "'forge changelog assemble' failed."
    }
    Write-Ok "Changelog assembled."

    # --- Remove consumed fragments ---

    Write-Step "Removing changelog.d fragments..."
    $fragments = Get-ChildItem -Path "changelog.d" -Filter "*.md" -ErrorAction SilentlyContinue
    if ($fragments) {
        $fragments | Remove-Item -Force
        Write-Ok "Removed $($fragments.Count) fragment(s)."
    } else {
        Write-Ok "No fragments to remove."
    }

    # --- Commit changelog changes ---

    Write-Step "Committing changelog..."
    git add CHANGELOG.md changelog.d/
    $changeStatus = git status --porcelain
    if ($changeStatus) {
        git commit -m "docs: assemble changelog for $Tag"
    } else {
        Write-Ok "No changelog changes to commit (fragments may have already been assembled)."
    }

    # --- Tag ---

    Write-Step "Creating tag $Tag..."
    git tag -a $Tag -m "Release $Tag"
    Write-Ok "Tag $Tag created."

    # --- Push ---

    Write-Step "Pushing commit and tag to origin..."
    git push origin main
    git push origin $Tag
    Write-Ok "Pushed."

    # --- Done ---

    Write-Host ""
    Write-Host "Release $Tag is on its way!" -ForegroundColor Green
    Write-Host "The GitHub Actions release workflow will build and publish the release." -ForegroundColor Yellow
    Write-Host "Watch progress: https://github.com/Robin831/Forge/actions" -ForegroundColor Yellow

    if ($Open) {
        Write-Step "Opening GitHub Actions..."
        Start-Process "https://github.com/Robin831/Forge/actions"
    }

}
finally {
    Pop-Location
}
