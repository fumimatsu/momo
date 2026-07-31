param(
    [string]$ViewerRepository = (Join-Path (Split-Path -Parent $PSScriptRoot) '..\momo-fpv-viewer'),
    [switch]$AllowDirtySource,
    [switch]$AllowDistributionDrift
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$viewerRoot = (Resolve-Path -LiteralPath $ViewerRepository).Path
$destinationDirectory = Join-Path $repoRoot 'tools\momo-relay\web'
$sourceFiles = @(
    [ordered]@{ Source = 'variants\relay\pilot.html'; Destination = 'pilot.html' },
    [ordered]@{ Source = 'variants\relay\pilot.js'; Destination = 'pilot.js' },
    [ordered]@{ Source = 'variants\relay\garage.html'; Destination = 'garage.html' },
    [ordered]@{ Source = 'variants\relay\ffb-bridge.js'; Destination = 'ffb-bridge.js' },
    [ordered]@{ Source = 'telemetry.js'; Destination = 'telemetry.js' },
    [ordered]@{ Source = 'm5-audio.js'; Destination = 'm5-audio.js' },
    [ordered]@{ Source = 'cpu-shadow-capture.js'; Destination = 'cpu-shadow-capture.js' },
    [ordered]@{ Source = 'gamepad.html'; Destination = 'gamepad.html' },
    [ordered]@{ Source = 'gamepad.js'; Destination = 'gamepad.js' },
    [ordered]@{ Source = 'gamepad-profile.js'; Destination = 'gamepad-profile.js' }
)

function Get-RecordedDistributionDrift {
    param(
        [string]$ViewerRoot,
        [string]$RepositoryRoot,
        [string]$DistributionRoot,
        [array]$Files
    )

    $metadataPath = Join-Path $DistributionRoot 'viewer-source.json'
    if (-not (Test-Path -LiteralPath $metadataPath)) {
        return @()
    }

    try {
        $metadata = Get-Content -Raw -LiteralPath $metadataPath | ConvertFrom-Json
    }
    catch {
        return @('viewer-source.json cannot be parsed')
    }

    if ($metadata.sourceRepository -ne 'https://github.com/fumimatsu/momo-fpv-viewer') {
        return @('viewer-source.json has an unexpected source repository')
    }
    if ([bool]$metadata.sourceDirty -or [string]::IsNullOrWhiteSpace([string]$metadata.sourceCommit)) {
        return @('viewer-source.json does not identify a clean source commit')
    }

    $driftedFiles = [System.Collections.Generic.List[string]]::new()
    foreach ($file in $Files) {
        $sourcePath = $file.Source -replace '\\', '/'
        $recordedBlob = @(& git -C $ViewerRoot rev-parse "$($metadata.sourceCommit):$sourcePath" 2>$null)
        if ($LASTEXITCODE -ne 0 -or $recordedBlob.Count -ne 1) {
            $driftedFiles.Add("$($file.Destination) (not present in recorded source commit)")
            continue
        }

        $destinationPath = Join-Path $DistributionRoot $file.Destination
        if (-not (Test-Path -LiteralPath $destinationPath)) {
            $driftedFiles.Add("$($file.Destination) (missing from distribution)")
            continue
        }

        $destinationBlob = @(& git -C $RepositoryRoot hash-object $destinationPath 2>$null)
        if ($LASTEXITCODE -ne 0 -or $destinationBlob.Count -ne 1 -or $recordedBlob[0].Trim() -ne $destinationBlob[0].Trim()) {
            $driftedFiles.Add($file.Destination)
        }
    }
    return $driftedFiles.ToArray()
}

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

$distributionDrift = @(Get-RecordedDistributionDrift -ViewerRoot $viewerRoot -RepositoryRoot $repoRoot -DistributionRoot $destinationDirectory -Files $sourceFiles)
if ($distributionDrift.Count -gt 0) {
    $description = $distributionDrift -join ', '
    if (-not $AllowDistributionDrift) {
        throw "Relay distribution differs from the recorded Viewer source: $description. Port the changes to momo-fpv-viewer and commit them before synchronizing. Use -AllowDistributionDrift only to replace a known, already-migrated distribution copy."
    }
    Write-Warning "Replacing known Relay distribution drift: $description"
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
