# One-line installer for agent-session-query on Windows
# https://github.com/itswl/agent-session-query

$ErrorActionPreference = 'Stop'

$Repo = "itswl/agent-session-query"
$BinaryName = "agent-session-query.exe"

# 1. Detect Architecture
$Arch = "amd64"
if ($env:PROCESSOR_ARCHITECTURE -eq "ARM64" -or $env:PROCESSOR_ARCHITEW6432 -eq "ARM64") {
    $Arch = "arm64"
}

# 2. Resolve Version (default to latest release)
$Version = $env:VERSION
if (-not $Version) {
    try {
        $Release = Invoke-RestMethod -Uri "https://api.github.com/repos/$Repo/releases/latest" -Headers @{ "User-Agent" = "PowerShell" } -UseBasicParsing
        $Version = $Release.tag_name
    } catch {
        Write-Error "Failed to determine the latest release version: $_"
        exit 1
    }
}

$ZipName = "agent-session-query-windows-$Arch.zip"
$DownloadUrl = "https://github.com/$Repo/releases/download/$Version/$ZipName"

$InstallDir = if ($env:INSTALL_DIR) { $env:INSTALL_DIR } else { "$env:LOCALAPPDATA\Programs\agent-session-query" }
if (-not (Test-Path $InstallDir)) {
    New-Item -ItemType Directory -Path $InstallDir -Force | Out-Null
}

$TempDir = [System.IO.Path]::Combine([System.IO.Path]::GetTempPath(), [System.Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $TempDir -Force | Out-Null
$ZipPath = Join-Path $TempDir $ZipName

try {
    Write-Host "Downloading agent-session-query $Version for windows-$Arch..."
    Invoke-WebRequest -Uri $DownloadUrl -OutFile $ZipPath -UseBasicParsing

    Expand-Archive -Path $ZipPath -DestinationPath $TempDir -Force
    $SourceExe = Join-Path $TempDir $BinaryName
    if (-not (Test-Path $SourceExe)) {
        throw "Archive did not contain $BinaryName"
    }

    $TargetExe = Join-Path $InstallDir $BinaryName
    Copy-Item -Path $SourceExe -Destination $TargetExe -Force

    # Crucial: Unblock the file to remove Zone.Identifier and bypass Windows SmartScreen
    Unblock-File -Path $TargetExe -ErrorAction SilentlyContinue

    Write-Host "Successfully installed $BinaryName ($Version) to $InstallDir" -ForegroundColor Green

    # 3. Add to User PATH if not already present
    $UserPath = [System.Environment]::GetEnvironmentVariable("Path", [System.EnvironmentVariableTarget]::User)
    $PathEntries = $UserPath -split ';' | Where-Object { $_ -ne "" }
    if ($PathEntries -notcontains $InstallDir) {
        $NewUserPath = "$UserPath;$InstallDir".Trim(';')
        [System.Environment]::SetEnvironmentVariable("Path", $NewUserPath, [System.EnvironmentVariableTarget]::User)
        $env:Path = "$env:Path;$InstallDir"
        Write-Host "[INFO] Added $InstallDir to user PATH." -ForegroundColor Cyan
    }

    Write-Host ""
    Write-Host "To get started, open a new terminal and run:"
    Write-Host "  agent-session-query --port 8080" -ForegroundColor Yellow
} finally {
    if (Test-Path $TempDir) {
        Remove-Item -Recurse -Force $TempDir -ErrorAction SilentlyContinue
    }
}
