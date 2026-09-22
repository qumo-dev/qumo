<#
.SYNOPSIS
    Installs the qumo Media over QUIC (MoQ) relay server binary.

.DESCRIPTION
    Downloads the appropriate qumo prebuilt binary for the current platform
    from GitHub Releases, verifies its SHA-256 checksum, extracts it, installs
    it to ~/.qumo/bin (or a custom directory), and configures the user PATH.

.PARAMETER Version
    Specific version to install (e.g. "0.6.260906" or "v0.6.260906").
    Defaults to "latest" (or $env:QUMO_VERSION).

.PARAMETER InstallDir
    Target directory to install the binary into.
    Defaults to "$HOME\.qumo\bin" (or $env:QUMO_INSTALL_DIR).

.PARAMETER NoModifyPath
    Do not add the installation directory to the User PATH environment variable.

.PARAMETER Force
    Force reinstall even if the specified version is already installed.

.EXAMPLE
    powershell -ExecutionPolicy ByPass -c "irm https://raw.githubusercontent.com/qumo-dev/qumo/main/install.ps1 | iex"

.EXAMPLE
    .\install.ps1 -Version 0.6.260906
#>
[CmdletBinding()]
param(
    [string]$Version = $env:QUMO_VERSION,
    [string]$InstallDir = $env:QUMO_INSTALL_DIR,
    [switch]$NoModifyPath,
    [switch]$Force
)

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"

# Enable TLS 1.2 and TLS 1.3 where available
try {
    [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.SecurityProtocolType]::Tls12 -bor [System.Net.SecurityProtocolType]::Tls13
} catch {
    try {
        [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.SecurityProtocolType]::Tls12
    } catch {}
}

function Write-Step {
    param([string]$Message)
    Write-Host "==> $Message"
}

function Write-WarningStep {
    param([string]$Message)
    Write-Warning "==> $Message"
}

function Get-PlatformArch {
    $rawArch = if ($env:PROCESSOR_ARCHITEW6432) {
        $env:PROCESSOR_ARCHITEW6432
    } else {
        $env:PROCESSOR_ARCHITECTURE
    }

    switch -Regex ($rawArch) {
        "^(AMD64|x86_64)$" {
            return @{
                Arch = "amd64"
                Label = "Windows (x64)"
            }
        }
        "^(ARM64|aarch64)$" {
            return @{
                Arch = "arm64"
                Label = "Windows (ARM64)"
            }
        }
        default {
            throw "Unsupported Windows architecture: '$rawArch'. qumo currently supports x64 and ARM64 on Windows."
        }
    }
}

function Resolve-Version {
    param([string]$RequestedVersion)

    if (-not [string]::IsNullOrWhiteSpace($RequestedVersion) -and $RequestedVersion -ne "latest") {
        $tag = if ($RequestedVersion.StartsWith("v")) { $RequestedVersion } else { "v$RequestedVersion" }
        $ver = $tag.TrimStart("v")
        return @{
            Tag = $tag
            Version = $ver
        }
    }

    # First attempt: GitHub Releases API
    $apiUri = "https://api.github.com/repos/qumo-dev/qumo/releases/latest"
    $headers = @{
        "User-Agent" = "qumo-installer"
        "Accept" = "application/vnd.github.v3+json"
    }
    try {
        $rel = Invoke-RestMethod -Uri $apiUri -Headers $headers -TimeoutSec 15
        if ($rel -and $rel.tag_name) {
            $tag = [string]$rel.tag_name
            $ver = $tag.TrimStart("v")
            return @{
                Tag = $tag
                Version = $ver
            }
        }
    } catch {
        # Fallback to redirect resolution if rate-limited or network blocked
    }

    # Fallback: inspect 302 redirect of GitHub releases/latest page
    try {
        $req = [System.Net.WebRequest]::Create("https://github.com/qumo-dev/qumo/releases/latest")
        $req.AllowAutoRedirect = $false
        $req.Timeout = 15000
        $resp = $req.GetResponse()
        $loc = $resp.GetResponseHeader("Location")
        $resp.Close()
        if (-not [string]::IsNullOrWhiteSpace($loc)) {
            $parts = $loc.TrimEnd("/").Split("/")
            $tag = $parts[-1]
            if ($tag.StartsWith("v")) {
                $ver = $tag.TrimStart("v")
                return @{
                    Tag = $tag
                    Version = $ver
                }
            }
        }
    } catch {}

    throw "Failed to resolve the latest qumo release from GitHub. Please specify a version explicitly using -Version <version>."
}

function Find-Checksum {
    param(
        [string]$ChecksumFilePath,
        [string]$TargetFilename
    )

    $escaped = [regex]::Escape($TargetFilename)
    foreach ($line in Get-Content -LiteralPath $ChecksumFilePath) {
        if ($line -match "^\s*([a-fA-F0-9]{64})\s+\*?$escaped\s*$") {
            return $matches[1].ToLowerInvariant()
        }
    }
    throw "Checksum for '$TargetFilename' not found in checksums.txt."
}

# --- Main execution ---

Write-Step "Installing qumo CLI"

if ($env:OS -ne "Windows_NT") {
    throw "install.ps1 supports Windows only. Use install.sh on macOS or Linux."
}

$platform = Get-PlatformArch
Write-Step "Detected platform: $($platform.Label)"

$resolved = Resolve-Version -RequestedVersion $Version
$resolvedVersion = $resolved.Version
$resolvedTag = $resolved.Tag
Write-Step "Resolved version: $resolvedVersion"

# Default install directory: $HOME\.qumo\bin
if ([string]::IsNullOrWhiteSpace($InstallDir)) {
    $userHome = if ($env:USERPROFILE) { $env:USERPROFILE } else { $HOME }
    $InstallDir = Join-Path $userHome ".qumo\bin"
}

$targetExe = Join-Path $InstallDir "qumo.exe"

if (-not $Force -and (Test-Path -LiteralPath $targetExe)) {
    try {
        $currentVerOutput = & $targetExe version 2>$null
        if ($currentVerOutput -match "qumo\s+v?([0-9A-Za-z.-]+)") {
            $currentVer = $matches[1]
            if ($currentVer -eq $resolvedVersion) {
                Write-Step "qumo $resolvedVersion is already installed at $targetExe"
                return
            }
        }
    } catch {}
}

$assetName = "qumo_${resolvedVersion}_windows_$($platform.Arch).zip"
$downloadUrl = "https://github.com/qumo-dev/qumo/releases/download/$resolvedTag/$assetName"
$checksumUrl = "https://github.com/qumo-dev/qumo/releases/download/$resolvedTag/checksums.txt"

$tempDir = Join-Path ([System.IO.Path]::GetTempPath()) ("qumo-install-" + [System.Guid]::NewGuid().ToString("N"))
New-Item -ItemType Directory -Path $tempDir -Force | Out-Null

try {
    $archiveFile = Join-Path $tempDir $assetName
    $checksumFile = Join-Path $tempDir "checksums.txt"

    Write-Step "Downloading qumo CLI"
    Invoke-WebRequest -Uri $downloadUrl -OutFile $archiveFile -UseBasicParsing -TimeoutSec 300
    Invoke-WebRequest -Uri $checksumUrl -OutFile $checksumFile -UseBasicParsing -TimeoutSec 60

    Write-Step "Verifying SHA-256 checksum"
    $expectedDigest = Find-Checksum -ChecksumFilePath $checksumFile -TargetFilename $assetName
    $actualDigest = (Get-FileHash -LiteralPath $archiveFile -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualDigest -ne $expectedDigest) {
        throw "Checksum verification failed for $assetName.`nExpected: $expectedDigest`nActual:   $actualDigest"
    }

    Write-Step "Extracting binary"
    $extractDir = Join-Path $tempDir "extracted"
    Expand-Archive -LiteralPath $archiveFile -DestinationPath $extractDir -Force

    $srcExe = Join-Path $extractDir "qumo.exe"
    if (-not (Test-Path -LiteralPath $srcExe)) {
        $found = Get-ChildItem -LiteralPath $extractDir -Filter "qumo.exe" -Recurse | Select-Object -First 1
        if ($null -ne $found) {
            $srcExe = $found.FullName
        } else {
            throw "qumo.exe not found inside the release archive."
        }
    }

    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
    Write-Step "Installing qumo to $targetExe"

    try {
        Copy-Item -LiteralPath $srcExe -Destination $targetExe -Force
    } catch {
        # If destination is locked (e.g. currently executing), rename and replace
        $oldExe = "$targetExe.old"
        if (Test-Path -LiteralPath $oldExe) {
            Remove-Item -LiteralPath $oldExe -Force -ErrorAction SilentlyContinue
        }
        Move-Item -LiteralPath $targetExe -Destination $oldExe -Force
        Copy-Item -LiteralPath $srcExe -Destination $targetExe -Force
    }

    if (-not $NoModifyPath) {
        $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
        $segments = if ($userPath) { $userPath.Split(";", [System.StringSplitOptions]::RemoveEmptyEntries) } else { @() }
        $alreadyInPath = $false
        foreach ($segment in $segments) {
            if ($segment.TrimEnd("\") -ieq $InstallDir.TrimEnd("\")) {
                $alreadyInPath = $true
                break
            }
        }

        if (-not $alreadyInPath) {
            Write-Step "Adding $InstallDir to PATH"
            $newUserPath = if ($userPath) { "$InstallDir;$userPath" } else { $InstallDir }
            [Environment]::SetEnvironmentVariable("Path", $newUserPath, "User")
            Write-Step "PATH updated for future sessions."
        } else {
            Write-Step "$InstallDir is already in User PATH."
        }
    }

    # Update current session PATH so qumo is immediately usable
    $currentSessionSegments = $env:Path.Split(";", [System.StringSplitOptions]::RemoveEmptyEntries)
    $inCurrentSession = $false
    foreach ($segment in $currentSessionSegments) {
        if ($segment.TrimEnd("\") -ieq $InstallDir.TrimEnd("\")) {
            $inCurrentSession = $true
            break
        }
    }
    if (-not $inCurrentSession) {
        $env:Path = "$InstallDir;$env:Path"
    }

    Write-Step "Successfully installed qumo CLI $resolvedVersion!"
    Write-Host ""
    Write-Host "To get started, restart your terminal or run:"
    Write-Host "  qumo playground      # launch relay + embedded web UI"
    Write-Host "  qumo --help          # list available commands"
} finally {
    Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
}
