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
$distributionManifestPath = Join-Path $viewerRoot 'tools\distribution-targets.json'
if (-not (Test-Path -LiteralPath $distributionManifestPath)) {
    throw "Viewer distribution manifest was not found: $distributionManifestPath"
}
$distributionManifest = Get-Content -Raw -LiteralPath $distributionManifestPath | ConvertFrom-Json
$relayTarget = $distributionManifest.targets.'relay-web'
if ($null -eq $relayTarget -or @($relayTarget.files).Count -eq 0) {
    throw 'Viewer distribution manifest does not define relay-web files.'
}
$sourceFiles = @($relayTarget.files | ForEach-Object {
    [ordered]@{
        Source = ([string]$_.source -replace '/', '\')
        Destination = ([string]$_.destination -replace '/', '\')
    }
})

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

    $sourceByDestination = @{}
    foreach ($file in $Files) {
        $sourceByDestination[[string]$file.Destination] = [string]$file.Source
    }

    $driftedFiles = [System.Collections.Generic.List[string]]::new()
    foreach ($recordedFile in @($metadata.files)) {
        $destination = [string]$recordedFile
        if ([string]::IsNullOrWhiteSpace($destination)) {
            continue
        }
        $source = if ($sourceByDestination.ContainsKey($destination)) {
            $sourceByDestination[$destination]
        }
        else {
            $destination
        }
        $sourcePath = $source -replace '\\', '/'
        $recordedBlob = @(& git -C $ViewerRoot rev-parse "$($metadata.sourceCommit):$sourcePath" 2>$null)
        if ($LASTEXITCODE -ne 0 -or $recordedBlob.Count -ne 1) {
            $driftedFiles.Add("$destination (not present in recorded source commit)")
            continue
        }

        $destinationPath = Join-Path $DistributionRoot $destination
        if (-not (Test-Path -LiteralPath $destinationPath)) {
            $driftedFiles.Add("$destination (missing from distribution)")
            continue
        }

        $destinationBlob = @(& git -C $RepositoryRoot hash-object $destinationPath 2>$null)
        if ($LASTEXITCODE -ne 0 -or $destinationBlob.Count -ne 1 -or $recordedBlob[0].Trim() -ne $destinationBlob[0].Trim()) {
            $driftedFiles.Add($destination)
        }
    }
    return $driftedFiles.ToArray()
}

function Remove-StaleDistributionFiles {
    param(
        [Parameter(Mandatory = $true)][string]$DistributionRoot,
        [Parameter(Mandatory = $true)][array]$Files
    )

    $metadataPath = Join-Path $DistributionRoot 'viewer-source.json'
    if (-not (Test-Path -LiteralPath $metadataPath)) {
        return
    }
    $metadata = Get-Content -Raw -LiteralPath $metadataPath | ConvertFrom-Json
    $desired = [System.Collections.Generic.HashSet[string]]::new([System.StringComparer]::OrdinalIgnoreCase)
    foreach ($file in $Files) {
        [void]$desired.Add([string]$file.Destination)
    }
    $rootPath = [System.IO.Path]::GetFullPath($DistributionRoot).TrimEnd('\') + '\'
    foreach ($previousFile in @($metadata.files)) {
        $relativePath = [string]$previousFile
        if ([string]::IsNullOrWhiteSpace($relativePath) -or $desired.Contains($relativePath)) {
            continue
        }
        if ([System.IO.Path]::IsPathRooted($relativePath)) {
            throw "Recorded Viewer file is rooted: $relativePath"
        }
        $resolvedPath = [System.IO.Path]::GetFullPath((Join-Path $DistributionRoot $relativePath))
        if (-not $resolvedPath.StartsWith($rootPath, [System.StringComparison]::OrdinalIgnoreCase)) {
            throw "Recorded Viewer file escapes the distribution directory: $relativePath"
        }
        if (Test-Path -LiteralPath $resolvedPath -PathType Leaf) {
            Remove-Item -LiteralPath $resolvedPath -Force
            Write-Host "Removed stale Relay Viewer file: $relativePath"
        }
    }
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

Remove-StaleDistributionFiles -DistributionRoot $destinationDirectory -Files $sourceFiles

foreach ($file in $sourceFiles) {
    Copy-Item -LiteralPath (Join-Path $viewerRoot $file.Source) -Destination (Join-Path $destinationDirectory $file.Destination) -Force
}

$metadata = [ordered]@{
    sourceRepository = 'https://github.com/fumimatsu/momo-fpv-viewer'
    sourceCommit = $commit
    sourceDirty = $dirty.Count -gt 0
    variant = 'relay-pilot'
    target = 'relay-web'
    files = @($sourceFiles | ForEach-Object { $_.Destination })
}
$metadata | ConvertTo-Json | Set-Content -LiteralPath (Join-Path $destinationDirectory 'viewer-source.json') -Encoding utf8
Write-Host "Synchronized Relay Viewer from $commit"
