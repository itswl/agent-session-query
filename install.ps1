# Installs asq on Windows: downloads the release for this machine, checks it against the
# release's published checksums, and puts the binary on the user's PATH. Run again, it
# upgrades the asq on the PATH where it is.
#
#   irm https://raw.githubusercontent.com/itswl/agent-session-query/main/install.ps1 | iex
#
#   $env:VERSION = 'v1.2.0'      a particular release instead of the latest
#   $env:INSTALL_DIR = 'D:\bin'  somewhere else than %LOCALAPPDATA%\Programs\asq, or than where
#                                the asq on the PATH is
#   $env:ASQ_REPO = 'you/fork'   releases of a fork
$ErrorActionPreference = 'Stop'

$repo = if ($env:ASQ_REPO) { $env:ASQ_REPO } else { 'itswl/agent-session-query' }
# An upgrade goes where the asq already installed is: a second one elsewhere would leave
# whichever comes first on the PATH running. A program of that name that is not asq is
# left alone.
$current = Get-Command asq.exe -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
$was = $null
if ($current) {
    try { $said = (& $current.Source --version 2>$null | Select-Object -First 1) } catch { $said = $null }
    if ($said -match '^asq (\S+)$') { $was = $Matches[1] }
}
$installDir = if ($env:INSTALL_DIR) { $env:INSTALL_DIR } elseif ($was) { Split-Path -Parent $current.Source } else { Join-Path $env:LOCALAPPDATA 'Programs\asq' }
$target = Join-Path $installDir 'asq.exe'

$arch = switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { 'amd64' }
    'ARM64' { 'arm64' }
    default { throw "no release for $($env:PROCESSOR_ARCHITECTURE) (there are amd64 and arm64)" }
}

$version = $env:VERSION
if (-not $version) {
    $latest = Invoke-RestMethod -Uri "https://api.github.com/repos/$repo/releases/latest" -Headers @{ 'User-Agent' = 'asq-install' }
    $version = $latest.tag_name
}
if (-not $version) { throw "could not find the latest release; name one: `$env:VERSION = 'v1.2.0'" }

$archive = "asq-windows-$arch.zip"
$base = "https://github.com/$repo/releases/download/$version"
$tmp = Join-Path ([IO.Path]::GetTempPath()) ("asq-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "asq $version for windows/$arch"
    if ($was -and $current.Source -eq $target) { Write-Host "replacing asq $was at $target" }
    try {
        Invoke-WebRequest -Uri "$base/$archive" -OutFile (Join-Path $tmp $archive) -UseBasicParsing
    } catch {
        throw "release $version of $repo has no ${archive}: name another with `$env:VERSION"
    }
    Invoke-WebRequest -Uri "$base/SHA256SUMS" -OutFile (Join-Path $tmp 'SHA256SUMS') -UseBasicParsing

    # What was downloaded is about to be run: it is checked first
    $expected = (Get-Content (Join-Path $tmp 'SHA256SUMS') | Where-Object { ($_ -split '\s+')[1] -eq $archive } | ForEach-Object { ($_ -split '\s+')[0] }) | Select-Object -First 1
    $actual = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $archive)).Hash.ToLower()
    if (-not $expected -or $expected.ToLower() -ne $actual) {
        throw "checksum mismatch for ${archive}: published '$expected', got '$actual'"
    }

    Expand-Archive -Path (Join-Path $tmp $archive) -DestinationPath $tmp -Force
    New-Item -ItemType Directory -Path $installDir -Force | Out-Null
    try {
        Move-Item -Path (Join-Path $tmp 'asq.exe') -Destination $target -Force
    } catch {
        # Windows does not replace a program that is running
        throw "$target is in use: an asq serve running from it? End it (schtasks /end /tn asq), run this again, then start it (schtasks /run /tn asq)"
    }
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

# Upgraded where it was found on the PATH, it is on the PATH already
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not ($was -and $current.Source -eq $target) -and ($userPath -split ';') -notcontains $installDir) {
    [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ';' + $installDir), 'User')
    Write-Host "added $installDir to your PATH (open a new terminal for it to take effect)"
}

Write-Host "installed $target"
if ($current -and $current.Source -ne $target) {
    Write-Host "another asq is on your PATH at $($current.Source): remove it, or it stays the one that runs"
}
Write-Host ''
Write-Host '  asq ls          recent sessions'
Write-Host '  asq serve       the page, at http://127.0.0.1:7717'
Write-Host '  asq             everything else'
