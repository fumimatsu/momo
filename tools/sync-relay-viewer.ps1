param(
    [string]$ViewerRepository = (Join-Path (Split-Path -Parent $PSScriptRoot) '..\momo-fpv-viewer'),
    [switch]$AllowDirtySource
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$viewerRoot = (Resolve-Path -LiteralPath $ViewerRepository).Path
$sourceDirectory = Join-Path $viewerRoot 'variants\relay'
$destinationDirectory = Join-Path $repoRoot 'tools\momo-relay\web'
$sourceFiles = @('pilot.html', 'pilot.js')

foreach ($name in $sourceFiles) {
    if (-not (Test-Path -LiteralPath (Join-Path $sourceDirectory $name))) {
        throw "Relay Viewer source was not found: $(Join-Path $sourceDirectory $name)"
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

foreach ($name in $sourceFiles) {
    Copy-Item -LiteralPath (Join-Path $sourceDirectory $name) -Destination (Join-Path $destinationDirectory $name) -Force
}

$metadata = [ordered]@{
    sourceRepository = 'https://github.com/fumimatsu/momo-fpv-viewer'
    sourceCommit = $commit
    sourceDirty = $dirty.Count -gt 0
    variant = 'relay-pilot'
    files = $sourceFiles
}
$metadata | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $destinationDirectory 'viewer-source.json') -Encoding utf8
Write-Host "Synchronized Relay Viewer from $commit"
