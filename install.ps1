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

# Configure console encoding and TLS protocols
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch {}
try {
    [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.SecurityProtocolType]::Tls12 -bor [System.Net.SecurityProtocolType]::Tls13
} catch {
    try {
        [System.Net.ServicePointManager]::SecurityProtocol = [System.Net.SecurityProtocolType]::Tls12
    } catch {}
}

# Terminal capabilities & ANSI styling
$isInteractive = (-not [Console]::IsOutputRedirected) -and ($host.Name -notmatch "ServerRemoteHost")

$esc = [char]27
if ($isInteractive) {
    $cBold   = "$esc[1m"
    $cDim    = "$esc[2m"
    $cCyan   = "$esc[36m"
    $cGreen  = "$esc[32m"
    $cYellow = "$esc[33m"
    $cRed    = "$esc[31m"
    $cReset  = "$esc[0m"
} else {
    # Redirected or non-interactive host: emit plain text, no escape sequences.
    $cBold   = ""
    $cDim    = ""
    $cCyan   = ""
    $cGreen  = ""
    $cYellow = ""
    $cRed    = ""
    $cReset  = ""
}

# Unicode glyphs are built from character codes, and this file is kept pure
# ASCII -- including these comments. Windows PowerShell 5.1 decodes a BOM-less
# script using the system ANSI codepage, and on a multi-byte codepage such as
# 932/936/949/950 a literal glyph here can consume the following newline and
# swallow the next assignment. Name the glyphs, never paste them.
$gDot     = [char]0x00B7   # MIDDLE DOT
$gDiamond = [char]0x25C7   # WHITE DIAMOND
$gCheck   = [char]0x2714   # HEAVY CHECK MARK
$gWarn    = [char]0x25B2   # BLACK UP-POINTING TRIANGLE
$gCross   = [char]0x2716   # HEAVY MULTIPLICATION X
$gSparkle = [char]0x2728   # SPARKLES

$spinnerFrames = @(
    [char]0x280B, [char]0x2819, [char]0x2839, [char]0x2838,
    [char]0x283C, [char]0x2834, [char]0x2826, [char]0x2827,
    [char]0x2807, [char]0x280F
)

function Write-Info {
    param([string]$Message)
    Write-Host "  $($cCyan)$($gDiamond)$($cReset) $Message"
}

function Write-Success {
    param([string]$Message)
    Write-Host "  $($cGreen)$($gCheck)$($cReset) $Message"
}

function Write-WarningStep {
    param([string]$Message)
    Write-Host "  $($cYellow)$($gWarn)$($cReset) $Message"
}

function Write-ErrorStep {
    param([string]$Message)
    Write-Host "  $($cRed)$($gCross)$($cReset) $Message"
}

function Save-RemoteFile {
    param(
        [string]$Url,
        [string]$OutFile,
        [string]$ActiveMessage,
        [string]$DoneMessage
    )

    if (-not $isInteractive) {
        Write-Host "  ${ActiveMessage}"
        Invoke-WebRequest -Uri $Url -OutFile $OutFile -UseBasicParsing -TimeoutSec 300
        Write-Success $DoneMessage
        return
    }

    $wc = New-Object System.Net.WebClient
    $wc.Headers.Add("User-Agent", "qumo-installer")
    $global:dlDone = $false
    $global:dlErr = $null

    $sub = Register-ObjectEvent -InputObject $wc -EventName DownloadFileCompleted -Action {
        $global:dlDone = $true
        $global:dlErr = $EventArgs.Error
    }

    try {
        $wc.DownloadFileAsync((New-Object System.Uri($Url)), $OutFile)
        $i = 0
        while (-not $global:dlDone) {
            $f = $spinnerFrames[$i % $spinnerFrames.Length]
            Write-Host -NoNewline "`r  $($cCyan)$f$($cReset) ${ActiveMessage}"
            Start-Sleep -Milliseconds 70
            $i++
        }

        if ($global:dlErr) {
            throw $global:dlErr
        }

        Write-Host -NoNewline "`r$($esc)[2K"
        Write-Success $DoneMessage
    } catch {
        Invoke-WebRequest -Uri $Url -OutFile $OutFile -UseBasicParsing -TimeoutSec 300
        Write-Host -NoNewline "`r$($esc)[2K"
        Write-Success $DoneMessage
    } finally {
        Unregister-Event -SourceIdentifier $sub.Name -ErrorAction SilentlyContinue
        $wc.Dispose()
    }
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
            throw "Unsupported Windows architecture: '$rawArch'. qumo currently supports x64 and ARM64."
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
    } catch {}

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

Write-Host ""
Write-Host "  $($cBold)$($cCyan)qumo$($cReset) $($cDim)$($gDot) Media over QUIC Relay installer$($cReset)"
Write-Host ""

if ($env:OS -ne "Windows_NT") {
    # Throw rather than exit: this script is documented to run via `irm ... | iex`,
    # where `exit` would terminate the caller's whole PowerShell session.
    throw "install.ps1 supports Windows only. On Linux/macOS, run install.sh."
}

$platform = Get-PlatformArch
Write-Info "Detected platform: $($cBold)$($platform.Label)$($cReset)"

$resolved = Resolve-Version -RequestedVersion $Version
$resolvedVersion = $resolved.Version
$resolvedTag = $resolved.Tag
Write-Success "Resolved version: $($cBold)$resolvedVersion$($cReset)"

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
                Write-Info "qumo $($cBold)$resolvedVersion$($cReset) is already installed at $targetExe"
                Write-Host ""
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

    # Animated archive download
    Save-RemoteFile -Url $downloadUrl -OutFile $archiveFile `
        -ActiveMessage "Downloading qumo CLI..." `
        -DoneMessage "Downloaded release archive $($cDim)($assetName)$($cReset)"

    # Fetch checksum
    Invoke-WebRequest -Uri $checksumUrl -OutFile $checksumFile -UseBasicParsing -TimeoutSec 60

    # Verify SHA-256
    $expectedDigest = Find-Checksum -ChecksumFilePath $checksumFile -TargetFilename $assetName
    $actualDigest = (Get-FileHash -LiteralPath $archiveFile -Algorithm SHA256).Hash.ToLowerInvariant()
    if ($actualDigest -ne $expectedDigest) {
        throw "Checksum mismatch for $assetName.`nExpected: $expectedDigest`nActual:   $actualDigest"
    }
    Write-Success "Verified SHA-256 checksum"

    # Extract
    $extractDir = Join-Path $tempDir "extracted"
    Expand-Archive -LiteralPath $archiveFile -DestinationPath $extractDir -Force
    Write-Success "Extracted binary"

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

    try {
        Copy-Item -LiteralPath $srcExe -Destination $targetExe -Force
    } catch {
        $oldExe = "$targetExe.old"
        if (Test-Path -LiteralPath $oldExe) {
            Remove-Item -LiteralPath $oldExe -Force -ErrorAction SilentlyContinue
        }
        Move-Item -LiteralPath $targetExe -Destination $oldExe -Force
        Copy-Item -LiteralPath $srcExe -Destination $targetExe -Force
    }
    Write-Success "Installed qumo to $($cDim)$targetExe$($cReset)"

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
            $newUserPath = if ($userPath) { "$InstallDir;$userPath" } else { $InstallDir }
            [Environment]::SetEnvironmentVariable("Path", $newUserPath, "User")
            Write-Success "Added $($cDim)$InstallDir$($cReset) to User PATH"
        }
    }

    # Update current session PATH
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

    # Clean finish card
    Write-Host ""
    Write-Host "  $($cGreen)$($cBold)$($gSparkle) Successfully installed qumo v${resolvedVersion}!$($cReset)"
    Write-Host ""
    Write-Host "  $($cDim)$($cBold)Get started:$($cReset)"
    Write-Host "    $($cCyan)qumo playground$($cReset)      $($cDim)# launch relay + embedded web demo$($cReset)"
    Write-Host "    $($cCyan)qumo --help$($cReset)          $($cDim)# explore available commands$($cReset)"
    Write-Host ""
} finally {
    Remove-Item -LiteralPath $tempDir -Recurse -Force -ErrorAction SilentlyContinue
}
