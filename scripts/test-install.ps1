$ErrorActionPreference = 'Stop'
$testDir = Join-Path ([IO.Path]::GetTempPath()) ([Guid]::NewGuid().ToString())
New-Item -ItemType Directory -Path $testDir | Out-Null
try {
    $env:GROK_COMMIT_INSTALL_DIR = Join-Path $testDir 'install with spaces'
    $env:GROK_COMMIT_CONFIG_DIR = Join-Path $testDir 'config'
    $env:GROK_COMMIT_STATE_DIR = Join-Path $testDir 'state'
    $env:GROK_COMMIT_NO_SETUP = '1'
    $env:GROK_COMMIT_NO_PATH = '1'
    $env:GROK_COMMIT_VERSION = ''
    $release = Join-Path $testDir 'release'
    New-Item -ItemType Directory -Path $release | Out-Null
    $exe = Join-Path $release 'grok-commit.exe'
    go build -o $exe -ldflags '-X main.version=0.2.0' ./cmd/grok-commit
    if ($LASTEXITCODE -ne 0) { throw 'build failed' }
    $arch = go env GOARCH
    $archive = "grok-commit_0.2.0_windows_${arch}.zip"
    Compress-Archive -Path $exe -DestinationPath (Join-Path $release $archive)
    $hash = (Get-FileHash (Join-Path $release $archive) -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText((Join-Path $release 'checksums.txt'), "$hash  $archive`n")
    function Invoke-RestMethod { param($Uri); return @{tag_name='v0.2.0'} }
    function Invoke-WebRequest {
        param($Uri, $OutFile, [switch]$UseBasicParsing)
        $name = $Uri.Substring($Uri.LastIndexOf('/') + 1)
        Copy-Item (Join-Path $release $name) $OutFile
    }
    & ./install.ps1
    & ./install.ps1
    $target = Join-Path $env:GROK_COMMIT_INSTALL_DIR 'grok-commit.exe'
    if ((& $target version) -ne 'grok-commit 0.2.0') { throw 'wrong installed version' }
    if ((& $target update --status) -notcontains 'Check interval: 7 days') { throw 'updates not registered' }
    Set-Content (Join-Path $release $archive) 'broken package'
    $rejected = $false
    try { & ./install.ps1 } catch { $rejected = $true }
    if (-not $rejected) { throw 'accepted corrupted package' }
    if ((& $target version) -ne 'grok-commit 0.2.0') { throw 'corrupt package changed installation' }
    # Test the actual detached Windows helper: it must wait for the parent to
    # exit, replace the executable, and record the rolled-back version.
    $previous = "$target.previous.exe"
    go build -o $previous -ldflags '-X main.version=0.1.0' ./cmd/grok-commit
    if ($LASTEXITCODE -ne 0) { throw 'previous build failed' }
    $state = @(Get-ChildItem (Join-Path $env:GROK_COMMIT_STATE_DIR 'updates') -Filter state.json -Recurse)[0].FullName
    $hash = (Get-FileHash $previous -Algorithm SHA256).Hash.ToLowerInvariant()
    [IO.File]::WriteAllText($state, (@{installed='0.2.0';previous='0.1.0';previous_sha256=$hash} | ConvertTo-Json))
    & $target update --rollback
    if ($LASTEXITCODE -ne 0) { throw 'rollback launch failed' }
    $deadline = [DateTime]::UtcNow.AddSeconds(25)
    do {
        Start-Sleep -Milliseconds 300
        $result = Get-Content $state -Raw | ConvertFrom-Json
    } while ($result.installed -ne '0.1.0' -and [DateTime]::UtcNow -lt $deadline)
    if ((& $target version) -ne 'grok-commit 0.1.0') { throw "rollback failed: $($result.last_error)" }
    if ($result.skip_version -ne 'v0.2.0') { throw 'rollback did not suppress reverted release' }
    Write-Host 'Windows installer, integrity rejection, and detached rollback passed.'
} finally { Remove-Item $testDir -Recurse -Force }
