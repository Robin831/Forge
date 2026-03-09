# Install The Forge — download the latest release from GitHub.
# Usage: irm https://raw.githubusercontent.com/Robin831/Forge/main/install.ps1 | iex
#
# Or run locally: .\install.ps1 [-InstallDir <path>] [-Version <tag>]

[CmdletBinding()]
param(
    [Parameter()]
    [string]$InstallDir,

    [Parameter()]
    [string]$Version
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$Repo = "Robin831/Forge"
$BinaryName = "forge"

# --- Helpers ----------------------------------------------------------------

function Write-Step($msg) { Write-Host "=> $msg" -ForegroundColor Cyan }
function Write-Ok($msg)   { Write-Host "   $msg" -ForegroundColor Green }
function Write-Err($msg)  { Write-Host "ERROR: $msg" -ForegroundColor Red; exit 1 }

# --- Detect OS/Arch ---------------------------------------------------------

Write-Step "Detecting platform..."

if ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([System.Runtime.InteropServices.OSPlatform]::Linux)) {
    $OS = "linux"
} elseif ([System.Runtime.InteropServices.RuntimeInformation]::IsOSPlatform([System.Runtime.InteropServices.OSPlatform]::OSX)) {
    $OS = "darwin"
} else {
    $OS = "windows"
}

$RawArch = if ($env:PROCESSOR_ARCHITECTURE) {
    $env:PROCESSOR_ARCHITECTURE
} else {
    [System.Runtime.InteropServices.RuntimeInformation]::OSArchitecture.ToString()
}

switch -Regex ($RawArch) {
    'AMD64|X64|x86_64' { $Arch = "amd64" }
    'ARM64|Arm64|aarch64' { $Arch = "arm64" }
    default { Write-Err "Unsupported architecture: $RawArch" }
}

# Windows arm64 is not built — fail early.
if ($OS -eq "windows" -and $Arch -eq "arm64") {
    Write-Err "Windows ARM64 builds are not available. Use Windows x64 instead."
}

Write-Ok "$OS/$Arch"

# --- Resolve install directory -----------------------------------------------

if (-not $InstallDir) {
    if ($OS -eq "windows") {
        $InstallDir = Join-Path $env:LOCALAPPDATA "Forge"
    } else {
        $InstallDir = Join-Path $HOME "bin"
    }
}

$BinaryPath = if ($OS -eq "windows") {
    Join-Path $InstallDir "$BinaryName.exe"
} else {
    Join-Path $InstallDir $BinaryName
}

# --- Resolve version ---------------------------------------------------------

Write-Step "Fetching latest release..."

if ($Version) {
    # Normalise: accept both "v1.2.3" and "1.2.3"
    if ($Version -notmatch '^v') { $Version = "v$Version" }
    $Tag = $Version
    $ApiUrl = "https://api.github.com/repos/$Repo/releases/tags/$Tag"
} else {
    $ApiUrl = "https://api.github.com/repos/$Repo/releases/latest"
}

try {
    $Release = Invoke-RestMethod -Uri $ApiUrl -Headers @{ Accept = "application/vnd.github+json" }
} catch {
    Write-Err "Failed to fetch release from $ApiUrl — $($_.Exception.Message)"
}

$Tag = $Release.tag_name
$ReleaseVersion = $Tag -replace '^v', ''

Write-Ok "$Tag"

# --- Check if already installed at this version ------------------------------

if (Test-Path $BinaryPath) {
    Write-Step "Checking installed version..."
    try {
        $CurrentVersion = & $BinaryPath version 2>&1 | Select-Object -First 1
        if ($CurrentVersion -match [regex]::Escape($ReleaseVersion)) {
            Write-Ok "Already on $Tag — nothing to do."
            exit 0
        }
        Write-Ok "Installed: $CurrentVersion -> upgrading to $Tag"
    } catch {
        Write-Ok "Could not determine current version — will overwrite."
    }
}

# --- Build asset name --------------------------------------------------------

$AssetName = "${BinaryName}_${ReleaseVersion}_${OS}_${Arch}.zip"
$ChecksumAssetName = "checksums.txt"

$AssetUrl = $Release.assets | Where-Object { $_.name -eq $AssetName } | Select-Object -ExpandProperty browser_download_url
$ChecksumUrl = $Release.assets | Where-Object { $_.name -eq $ChecksumAssetName } | Select-Object -ExpandProperty browser_download_url

if (-not $AssetUrl) {
    $Available = ($Release.assets | ForEach-Object { $_.name }) -join ', '
    Write-Err "Asset '$AssetName' not found in release $Tag. Available: $Available"
}

# --- Download ----------------------------------------------------------------

$TempDir = Join-Path ([System.IO.Path]::GetTempPath()) "forge-install-$([System.Guid]::NewGuid().ToString('N').Substring(0,8))"
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null

try {
    $ZipPath = Join-Path $TempDir $AssetName
    $ChecksumPath = Join-Path $TempDir $ChecksumAssetName

    Write-Step "Downloading $AssetName..."
    Invoke-WebRequest -Uri $AssetUrl -OutFile $ZipPath -UseBasicParsing
    Write-Ok "Downloaded."

    # --- Verify checksum -----------------------------------------------------

    if ($ChecksumUrl) {
        Write-Step "Verifying checksum..."
        Invoke-WebRequest -Uri $ChecksumUrl -OutFile $ChecksumPath -UseBasicParsing

        $ActualHash = (Get-FileHash -Path $ZipPath -Algorithm SHA256).Hash.ToLower()
        $escapedName = [regex]::Escape($AssetName)
        $pattern = "$escapedName`$"
        $matchingLines = Get-Content $ChecksumPath | Where-Object { $_ -match $pattern }

        if ($matchingLines -is [array]) {
            $lineCount = $matchingLines.Count
        } elseif ($matchingLines) {
            $lineCount = 1
        } else {
            $lineCount = 0
        }

        if ($lineCount -eq 1) {
            $ExpectedLine = if ($matchingLines -is [array]) { $matchingLines[0] } else { $matchingLines }
            $ExpectedHash = ($ExpectedLine -split '\s+')[0].ToLower()
            if ($ActualHash -ne $ExpectedHash) {
                Write-Err "Checksum mismatch!`n  Expected: $ExpectedHash`n  Actual:   $ActualHash"
            }
            Write-Ok "SHA256 verified."
        } elseif ($lineCount -eq 0) {
            Write-Host "   Warning: no checksum entry for $AssetName in checksums.txt — skipping verification." -ForegroundColor Yellow
        } else {
            Write-Err "Multiple checksum entries found for $AssetName in checksums.txt — cannot verify uniquely."
        }
    } else {
        Write-Host "   Warning: checksums.txt not found in release — skipping verification." -ForegroundColor Yellow
    }

    # --- Extract --------------------------------------------------------------

    Write-Step "Extracting to $InstallDir..."
    if (-not (Test-Path $InstallDir)) {
        New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    }

    $ExtractDir = Join-Path $TempDir "extracted"
    Expand-Archive -Path $ZipPath -DestinationPath $ExtractDir -Force

    # GoReleaser puts the binary at the root of the zip.
    $ExtractedBinary = Get-ChildItem -Path $ExtractDir -Recurse -Filter "$BinaryName*" |
        Where-Object { -not $_.PSIsContainer } |
        Select-Object -First 1

    if (-not $ExtractedBinary) {
        Write-Err "Could not find '$BinaryName' binary inside the archive."
    }

    Copy-Item -Path $ExtractedBinary.FullName -Destination $BinaryPath -Force

    if ($OS -ne "windows") {
        # Ensure the forge binary is executable on Unix-like systems.
        & chmod +x -- $BinaryPath
    }

    Write-Ok "Installed to $BinaryPath"

    # --- Add to PATH (Windows user-level) ------------------------------------

    if ($OS -eq "windows") {
        Write-Step "Checking PATH..."
        $UserPath = [Environment]::GetEnvironmentVariable("Path", "User")
        if ($UserPath -split ';' | Where-Object { $_ -eq $InstallDir }) {
            Write-Ok "Already in PATH."
        } else {
            [Environment]::SetEnvironmentVariable("Path", "$UserPath;$InstallDir", "User")
            $env:Path = "$env:Path;$InstallDir"
            Write-Ok "Added $InstallDir to user PATH. Restart your terminal for it to take effect."
        }
    } else {
        # On Linux/macOS, ~/bin is often already in PATH. Just advise if not.
        $InPath = $env:PATH -split ':' | Where-Object { $_ -eq $InstallDir }
        if (-not $InPath) {
            Write-Host "   Note: add '$InstallDir' to your PATH:" -ForegroundColor Yellow
            Write-Host "     export PATH=`"$InstallDir`:`$PATH`"" -ForegroundColor Yellow
        }
    }

    # --- Print version -------------------------------------------------------

    Write-Step "Verifying installation..."
    try {
        $InstalledVersion = & $BinaryPath version 2>&1 | Select-Object -First 1
        Write-Ok $InstalledVersion
    } catch {
        Write-Ok "Installed $Tag (could not run 'forge version' — you may need to restart your terminal)."
    }

    Write-Host ""
    Write-Host "Forge $Tag installed successfully!" -ForegroundColor Green
    Write-Host "Run 'forge doctor' to check your setup." -ForegroundColor Yellow

} finally {
    # Clean up temp files.
    Remove-Item -Path $TempDir -Recurse -Force -ErrorAction SilentlyContinue
}
