# Usage: irm https://raw.githubusercontent.com/ddhjy/grok-commit/main/install.ps1 | iex
# Optional: GROK_COMMIT_VERSION, GROK_COMMIT_INSTALL_DIR, GROK_COMMIT_NO_SETUP=1,
# GROK_COMMIT_NO_PATH=1. No administrator privileges required.
& {
    $ErrorActionPreference = 'Stop'
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $repo = 'https://github.com/ddhjy/grok-commit'
    $nativeArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    $arch = switch ($nativeArch) {
        'AMD64' { 'amd64' }
        'ARM64' { 'arm64' }
        default { throw "grok-commit is built for x64 and ARM64 processors, not $nativeArch. To build it from source instead, run: go install github.com/ddhjy/grok-commit/cmd/grok-commit@latest" }
    }
    $stable = '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$'
    if ($env:GROK_COMMIT_VERSION) {
        $tag = 'v' + $env:GROK_COMMIT_VERSION.TrimStart('v')
        if ($tag -notmatch $stable) { throw "GROK_COMMIT_VERSION=$($env:GROK_COMMIT_VERSION) isn't a release version. Use one such as v0.2.0, or leave it unset to install the latest release." }
    }
    else {
        try { $latest = (Invoke-RestMethod 'https://api.github.com/repos/ddhjy/grok-commit/releases/latest').tag_name }
        catch { throw "Couldn't look up the latest release on GitHub ($($_.Exception.Message)). Check your internet connection, then try again." }
        $tag = 'v' + "$latest".TrimStart('v')
        if ($tag -notmatch $stable) { throw "GitHub didn't report a stable release of grok-commit, so nothing was installed. Try again later." }
    }
    $version = $tag.Substring(1)
    $archive = "grok-commit_${version}_windows_${arch}.zip"
    $work = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString())
    $candidate = $null
    New-Item -ItemType Directory -Path $work | Out-Null
    try {
        Write-Host "Installing grok-commit $version for Windows ($arch)..."
        try { Invoke-WebRequest "$repo/releases/download/$tag/$archive" -OutFile (Join-Path $work $archive) -UseBasicParsing }
        catch { throw "Couldn't download grok-commit $version ($($_.Exception.Message)). Check your internet connection, and that this version exists at $repo/releases." }
        try { Invoke-WebRequest "$repo/releases/download/$tag/checksums.txt" -OutFile (Join-Path $work 'checksums.txt') -UseBasicParsing }
        catch { throw "Couldn't download the checksums for grok-commit $version, so nothing was installed. Try again." }
        $matches = @(Get-Content (Join-Path $work 'checksums.txt') | Where-Object { $_ -match "^[0-9a-f]{64}\s+$([regex]::Escape($archive))$" })
        if ($matches.Count -ne 1) { throw "The release's checksum file doesn't list $archive, so nothing was installed." }
        $expected = ($matches[0] -split '\s+')[0]
        $actual = (Get-FileHash (Join-Path $work $archive) -Algorithm SHA256).Hash
        if ($actual -ne $expected) { throw "The download didn't match its published checksum, so nothing was installed. Try again." }
        $installDir = $env:GROK_COMMIT_INSTALL_DIR
        if (-not $installDir) { $installDir = Join-Path $env:LOCALAPPDATA 'Programs\grok-commit' }
        try { New-Item -ItemType Directory -Path $installDir -Force | Out-Null }
        catch { throw "Couldn't create $installDir. Choose another folder with GROK_COMMIT_INSTALL_DIR, then run the installer again." }
        $installDir = (Resolve-Path $installDir).Path
        $candidate = Join-Path $installDir ('.grok-commit-' + [Guid]::NewGuid().ToString() + '.exe')
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $zip = [IO.Compression.ZipFile]::OpenRead((Join-Path $work $archive))
        try {
            $entries = @($zip.Entries | Where-Object { $_.FullName -eq 'grok-commit.exe' })
            if ($entries.Count -ne 1 -or $entries[0].Length -le 0 -or $entries[0].Length -gt 64MB) { throw "The release package doesn't contain a valid grok-commit program, so nothing was installed." }
            [IO.Compression.ZipFileExtensions]::ExtractToFile($entries[0], $candidate)
        } finally { $zip.Dispose() }
        $reported = & $candidate version
        if ($LASTEXITCODE -ne 0 -or $reported -ne "grok-commit $version") { throw "The downloaded grok-commit didn't pass its version check, so nothing was installed." }
        $target = Join-Path $installDir 'grok-commit.exe'
        # Windows needs other foreground invocations closed before replacing.
        $backup = "$candidate.backup"
        try {
            # PowerShell can coerce a null string argument into an empty path.
            # An explicit backup works with both Windows PowerShell and pwsh.
            if (Test-Path $target) { [IO.File]::Replace($candidate, $target, $backup) }
            else { [IO.File]::Move($candidate, $target) }
        }
        catch { throw "Couldn't replace $target ($($_.Exception.Message)). Close any running grok-commit, then run the installer again." }
        # A still-running old copy can't be deleted yet; it's harmless to leave.
        if (Test-Path $backup) { Remove-Item $backup -Force -ErrorAction SilentlyContinue }
        $candidate = $null
        & $target __install
        $updatesReady = $LASTEXITCODE -eq 0
        Write-Host "$([char]0x2713) Installed grok-commit $version at $target"
        if (-not $updatesReady) { Write-Host "Automatic updates couldn't be set up (see the message above). To try again, run: grok-commit update --enable" }
        if ($env:GROK_COMMIT_NO_PATH -ne '1') {
            $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
            if (($userPath -split ';') -notcontains $installDir) {
                [Environment]::SetEnvironmentVariable('Path', "$installDir;$userPath", 'User')
                Write-Host "Added $installDir to your PATH. If a terminal can't find grok-commit, restart it."
            }
            if (($env:PATH -split ';') -notcontains $installDir) { $env:PATH = "$installDir;$env:PATH" }
        }
        elseif (($env:PATH -split ';') -notcontains $installDir) { Write-Host "To use the grok-commit command, add $installDir to your PATH." }
        if ($env:GROK_COMMIT_NO_SETUP -eq '1') { Write-Host 'Next, connect to Grok: grok-commit setup' }
        elseif ([Console]::IsInputRedirected) { & $target setup --yes }
        else {
            Write-Host ''
            & $target setup
        }
    } finally {
        if ($candidate -and (Test-Path $candidate)) { Remove-Item $candidate -Force }
        Remove-Item $work -Recurse -Force
    }
}
