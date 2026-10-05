# Usage: irm https://raw.githubusercontent.com/ddhjy/grok-commit/main/install.ps1 | iex
# Optional: GROK_COMMIT_VERSION, GROK_COMMIT_INSTALL_DIR, GROK_COMMIT_NO_SETUP=1,
# GROK_COMMIT_NO_PATH=1. No administrator privileges required.
& {
    $ErrorActionPreference = 'Stop'
    [Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12
    $repo = 'https://github.com/ddhjy/grok-commit'
    $nativeArch = if ($env:PROCESSOR_ARCHITEW6432) { $env:PROCESSOR_ARCHITEW6432 } else { $env:PROCESSOR_ARCHITECTURE }
    $arch = switch ($nativeArch) { 'AMD64' { 'amd64' } 'ARM64' { 'arm64' } default { throw 'Supported architectures: arm64 and amd64.' } }
    $tag = $env:GROK_COMMIT_VERSION
    if (-not $tag) { $tag = (Invoke-RestMethod 'https://api.github.com/repos/ddhjy/grok-commit/releases/latest').tag_name }
    $tag = 'v' + $tag.TrimStart('v')
    if ($tag -notmatch '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$') { throw 'Could not find a stable release.' }
    $version = $tag.Substring(1)
    $archive = "grok-commit_${version}_windows_${arch}.zip"
    $work = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString())
    $candidate = $null
    New-Item -ItemType Directory -Path $work | Out-Null
    try {
        Write-Host "Installing grok-commit $tag (windows/$arch)..."
        Invoke-WebRequest "$repo/releases/download/$tag/$archive" -OutFile (Join-Path $work $archive) -UseBasicParsing
        Invoke-WebRequest "$repo/releases/download/$tag/checksums.txt" -OutFile (Join-Path $work 'checksums.txt') -UseBasicParsing
        $matches = @(Get-Content (Join-Path $work 'checksums.txt') | Where-Object { $_ -match "^[0-9a-f]{64}\s+$([regex]::Escape($archive))$" })
        if ($matches.Count -ne 1) { throw 'Invalid or missing archive checksum.' }
        $expected = ($matches[0] -split '\s+')[0]
        $actual = (Get-FileHash (Join-Path $work $archive) -Algorithm SHA256).Hash
        if ($actual -ne $expected) { throw 'Checksum mismatch; installation unchanged.' }
        $installDir = $env:GROK_COMMIT_INSTALL_DIR
        if (-not $installDir) { $installDir = Join-Path $env:LOCALAPPDATA 'Programs\grok-commit' }
        New-Item -ItemType Directory -Path $installDir -Force | Out-Null
        $installDir = (Resolve-Path $installDir).Path
        $candidate = Join-Path $installDir ('.grok-commit-' + [Guid]::NewGuid().ToString() + '.exe')
        Add-Type -AssemblyName System.IO.Compression.FileSystem
        $zip = [IO.Compression.ZipFile]::OpenRead((Join-Path $work $archive))
        try {
            $entries = @($zip.Entries | Where-Object { $_.FullName -eq 'grok-commit.exe' })
            if ($entries.Count -ne 1 -or $entries[0].Length -le 0 -or $entries[0].Length -gt 64MB) { throw 'Invalid release executable.' }
            [IO.Compression.ZipFileExtensions]::ExtractToFile($entries[0], $candidate)
        } finally { $zip.Dispose() }
        $reported = & $candidate version
        if ($LASTEXITCODE -ne 0 -or $reported -ne "grok-commit $version") { throw 'Executable version check failed.' }
        $target = Join-Path $installDir 'grok-commit.exe'
        # Windows needs other foreground invocations closed before replacing.
        if (Test-Path $target) { [IO.File]::Replace($candidate, $target, $null) }
        else { [IO.File]::Move($candidate, $target) }
        $candidate = $null
        & $target __install
        if ($LASTEXITCODE -ne 0) { throw 'Could not register installation.' }
        if ($env:GROK_COMMIT_NO_PATH -ne '1') {
            $userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
            if (($userPath -split ';') -notcontains $installDir) {
                [Environment]::SetEnvironmentVariable('Path', "$installDir;$userPath", 'User')
            }
            if (($env:PATH -split ';') -notcontains $installDir) { $env:PATH = "$installDir;$env:PATH" }
        }
        Write-Host "Installed: $target"
        if ($env:GROK_COMMIT_NO_SETUP -eq '1') { Write-Host 'Next: grok-commit setup' }
        elseif ([Console]::IsInputRedirected) { & $target setup --yes }
        else { & $target setup }
        if ($env:GROK_COMMIT_NO_SETUP -ne '1' -and $LASTEXITCODE -ne 0) { Write-Host 'Installed successfully. Finish later with: grok-commit setup' }
    } finally {
        if ($candidate -and (Test-Path $candidate)) { Remove-Item $candidate -Force }
        Remove-Item $work -Recurse -Force
    }
}
