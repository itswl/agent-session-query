# Installs asq on Windows: downloads the release for this machine, checks it against the
# release's published checksums, and puts the binary on the user's PATH.
#
#   irm https://raw.githubusercontent.com/itswl/agent-session-query/main/install.ps1 | iex
#
#   $env:VERSION = 'v1.2.0'      a particular release instead of the latest
#   $env:INSTALL_DIR = 'D:\bin'  somewhere else than %LOCALAPPDATA%\Programs\asq
#   $env:ASQ_REPO = 'you/fork'   releases of a fork
$ErrorActionPreference = 'Stop'

$repo = if ($env:ASQ_REPO) { $env:ASQ_REPO } else { 'itswl/agent-session-query' }
$installDir = if ($env:INSTALL_DIR) { $env:INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\asq' }

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
    Move-Item -Path (Join-Path $tmp 'asq.exe') -Destination (Join-Path $installDir 'asq.exe') -Force
}
finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (($userPath -split ';') -notcontains $installDir) {
    [Environment]::SetEnvironmentVariable('Path', ($userPath.TrimEnd(';') + ';' + $installDir), 'User')
    Write-Host "added $installDir to your PATH (open a new terminal for it to take effect)"
}

Write-Host "installed $(Join-Path $installDir 'asq.exe')"
Write-Host ''
Write-Host '  asq ls          recent sessions'
Write-Host '  asq serve       the page, at http://127.0.0.1:7717'
Write-Host '  asq             everything else'
