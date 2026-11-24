# Installs the latest release of plugyard on Windows, or updates it.
#
#   irm https://raw.githubusercontent.com/Amraa1/plugyard/main/install.ps1 | iex
#
# It goes into %LOCALAPPDATA%\Programs\plugyard unless PLUGYARD_INSTALL_DIR
# names another folder, and that folder is added to your PATH unless
# PLUGYARD_NO_MODIFY_PATH is set. PLUGYARD_RELEASES_URL names another copy of
# the releases, such as a mirror.
$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$repo = if ($env:PLUGYARD_RELEASES_URL) { $env:PLUGYARD_RELEASES_URL.TrimEnd('/') } else { 'https://github.com/Amraa1/plugyard' }
$dir = if ($env:PLUGYARD_INSTALL_DIR) { $env:PLUGYARD_INSTALL_DIR } else { Join-Path $env:LOCALAPPDATA 'Programs\plugyard' }
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$asset = "plugyard_windows_$arch.zip"

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("plugyard-install-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
    Write-Host "Downloading $asset"
    Invoke-WebRequest -UseBasicParsing "$repo/releases/latest/download/$asset" -OutFile (Join-Path $tmp $asset)
    Invoke-WebRequest -UseBasicParsing "$repo/releases/latest/download/checksums.txt" -OutFile (Join-Path $tmp 'checksums.txt')

    # The download must match the checksum the release lists for it.
    $want = $null
    foreach ($line in Get-Content (Join-Path $tmp 'checksums.txt')) {
        $fields = $line -split '\s+'
        if ($fields.Count -eq 2 -and $fields[1] -eq $asset) { $want = $fields[0] }
    }
    $got = (Get-FileHash -Algorithm SHA256 (Join-Path $tmp $asset)).Hash
    if (-not $want -or $want -ne $got) {
        throw "$asset does not match its checksum; nothing was installed."
    }

    Expand-Archive -Path (Join-Path $tmp $asset) -DestinationPath $tmp -Force
    New-Item -ItemType Directory -Force -Path $dir | Out-Null
    $exe = Join-Path $dir 'plugyard.exe'
    if (Test-Path $exe) {
        # A program that is running cannot be overwritten, only renamed.
        $old = "$exe.old"
        if (Test-Path $old) { Remove-Item $old -Force -ErrorAction SilentlyContinue }
        Rename-Item $exe $old
    }
    Copy-Item (Join-Path $tmp 'plugyard.exe') $exe

    $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
    $onPath = ($userPath -split ';') -contains $dir
    if ($env:PLUGYARD_NO_MODIFY_PATH) { $onPath = $true }
    if (-not $onPath) {
        $newPath = if ($userPath) { "$userPath;$dir" } else { $dir }
        [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
        $env:Path = "$env:Path;$dir"
    }

    $version = ((& $exe version) | Select-Object -First 1) -replace '^Version:\s*', ''
    Write-Host "Installed plugyard $version in $dir"
    if (-not $onPath) {
        Write-Host "$dir was added to your PATH. Terminals you open from now on will find plugyard."
    }
    Write-Host 'Try it: plugyard init my-site'
}
finally {
    Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
}
