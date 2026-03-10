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

# --- Generate-ReleaseNotes ---

function Generate-ReleaseNotes {
    param([string]$Version)

    # Extract this version's section from CHANGELOG.md
    $changelog = Get-Content -Path "CHANGELOG.md" -Raw

    # Find the section for this version (## [x.y.z] ... until next ## [ or end)
    $versionEscaped = [regex]::Escape($Version)
    $pattern = "(?ms)^## \[$versionEscaped\][^\n]*\n(.*?)(?=^## \[|\z)"
    $match = [regex]::Match($changelog, $pattern)

    $features = @()
    $bugFixes = @()
    $other = @()

    if ($match.Success) {
        $section = $match.Groups[1].Value

        # Parse subsections (### Added, ### Changed, ### Fixed, etc.)
        $subPattern = "(?ms)^### (\w+)\s*\n(.*?)(?=^### |\z)"
        $subMatches = [regex]::Matches($section, $subPattern)

        foreach ($sub in $subMatches) {
            $category = $sub.Groups[1].Value
            $items = $sub.Groups[2].Value.Trim()
            if (-not $items) { continue }

            switch ($category) {
                'Added'   { $features += $items }
                'Fixed'   { $bugFixes += $items }
                default   { $other += $items }
            }
        }
    }

    # Build release notes
    $sb = [System.Text.StringBuilder]::new()

    [void]$sb.AppendLine("## Install")
    [void]$sb.AppendLine("")
    [void]$sb.AppendLine('```powershell')
    [void]$sb.AppendLine('irm https://raw.githubusercontent.com/Robin831/Forge/main/install.ps1 | iex')
    [void]$sb.AppendLine('```')
    [void]$sb.AppendLine("")
    [void]$sb.AppendLine("Or download the archive for your platform below.")
    [void]$sb.AppendLine("")

    if ($features) {
        [void]$sb.AppendLine("## Features")
        [void]$sb.AppendLine("")
        [void]$sb.AppendLine(($features -join "`n"))
        [void]$sb.AppendLine("")
    }

    if ($bugFixes) {
        [void]$sb.AppendLine("## Bug Fixes")
        [void]$sb.AppendLine("")
        [void]$sb.AppendLine(($bugFixes -join "`n"))
        [void]$sb.AppendLine("")
    }

    if ($other) {
        [void]$sb.AppendLine("## Other Changes")
        [void]$sb.AppendLine("")
        [void]$sb.AppendLine(($other -join "`n"))
        [void]$sb.AppendLine("")
    }

    return $sb.ToString().TrimEnd()
}

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
        Write-Err "Tag $Tag already exists locally."
    }
    $remoteTag = git ls-remote --tags origin $Tag
    if ($remoteTag) {
        Write-Err "Tag $Tag already exists on origin."
    }
    Write-Ok "Tag $Tag is available."

    # --- Pull latest ---

    Write-Step "Pulling latest from origin..."
    git pull --rebase
    Write-Ok "Up to date."

    # --- Assemble changelog ---

    Write-Step "Assembling changelog..."
    go run ./cmd/forge changelog assemble --version $Version
    if ($LASTEXITCODE -ne 0) {
        Write-Err "'go run ./cmd/forge changelog assemble' failed."
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

    # --- Generate release notes ---

    Write-Step "Generating release notes..."
    $releaseNotes = Generate-ReleaseNotes -Version $Version
    $utf8NoBom = [System.Text.UTF8Encoding]::new($false)
    [System.IO.File]::WriteAllText((Join-Path $PWD ".release-notes.md"), $releaseNotes, $utf8NoBom)
    Write-Ok "Release notes written to .release-notes.md"

    # --- Commit changelog changes ---

    Write-Step "Committing changelog..."
    git add CHANGELOG.md changelog.d/ .release-notes.md
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
