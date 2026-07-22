param(
    [string]$ViewerRepository = (Join-Path (Split-Path -Parent $PSScriptRoot) '..\momo-fpv-viewer'),
    [switch]$AllowDirtySource
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$viewerRoot = (Resolve-Path -LiteralPath $ViewerRepository).Path
$destinationDirectory = Join-Path $repoRoot 'tools\momo-relay\web'
$sourceFiles = @(
    [ordered]@{ Source = 'variants\relay\pilot.html'; Destination = 'pilot.html' },
    [ordered]@{ Source = 'variants\relay\pilot.js'; Destination = 'pilot.js' },
    [ordered]@{ Source = 'variants\relay\ffb-bridge.js'; Destination = 'ffb-bridge.js' },
    [ordered]@{ Source = 'gamepad.html'; Destination = 'gamepad.html' },
    [ordered]@{ Source = 'gamepad.js'; Destination = 'gamepad.js' },
    [ordered]@{ Source = 'gamepad-profile.js'; Destination = 'gamepad-profile.js' }
)

foreach ($file in $sourceFiles) {
    $sourcePath = Join-Path $viewerRoot $file.Source
    if (-not (Test-Path -LiteralPath $sourcePath)) {
        throw "Relay Viewer source was not found: $sourcePath"
    }
}

$gitRoot = (& git -C $viewerRoot rev-parse --show-toplevel).Trim()
$gitRootPath = if ($LASTEXITCODE -eq 0) { (Resolve-Path -LiteralPath $gitRoot).Path } else { '' }
if ($LASTEXITCODE -ne 0 -or $gitRootPath -ne $viewerRoot) {
    throw "ViewerRepository is not the root of a Git repository: $viewerRoot"
}

$dirty = @(& git -C $viewerRoot status --porcelain)
if ($LASTEXITCODE -ne 0) {
    throw 'Could not inspect Viewer repository status.'
}
if ($dirty.Count -gt 0 -and -not $AllowDirtySource) {
    throw 'Viewer repository has uncommitted changes. Commit the Relay Variant before synchronizing, or use -AllowDirtySource only for investigation.'
}

$commit = (& git -C $viewerRoot rev-parse HEAD).Trim()
if ($LASTEXITCODE -ne 0) {
    throw 'Could not resolve Viewer source commit.'
}

foreach ($file in $sourceFiles) {
    Copy-Item -LiteralPath (Join-Path $viewerRoot $file.Source) -Destination (Join-Path $destinationDirectory $file.Destination) -Force
}

$metadata = [ordered]@{
    sourceRepository = 'https://github.com/fumimatsu/momo-fpv-viewer'
    sourceCommit = $commit
    sourceDirty = $dirty.Count -gt 0
    variant = 'relay-pilot'
    files = @($sourceFiles | ForEach-Object { $_.Destination })
}
$metadata | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $destinationDirectory 'viewer-source.json') -Encoding utf8
Write-Host "Synchronized Relay Viewer from $commit"
