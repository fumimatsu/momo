[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$InputPath,
    [ValidateNotNullOrEmpty()]
    [int[]]$SourceCounts = @(1, 5, 8, 12, 20, 32),
    [ValidateRange(1, 60)]
    [int]$FrameRate = 50,
    [ValidateRange(5, 3600)]
    [int]$MeasurementSeconds = 30,
    [ValidateRange(5, 600)]
    [int]$SourceReadyTimeoutSeconds = 120,
    [ValidateSet(50, 40, 33, 25)]
    [int]$InitialDetectionHz = 50,
    [ValidateRange(1, 8)]
    [int]$ConnectParallelism = 4,
    [string]$MomoExecutable = '',
    [string]$Python = '',
    [string]$OutputDirectory = '',
    [switch]$NoAdaptive
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$artifactRoot = Join-Path $PSScriptRoot '.artifacts\dynamic-marker'
if ([string]::IsNullOrWhiteSpace($MomoExecutable)) {
    $MomoExecutable = Join-Path $repoRoot '_build\windows_x86_64\release\momo\Release\momo.exe'
}
if ([string]::IsNullOrWhiteSpace($Python)) {
    $Python = Join-Path $PSScriptRoot '.artifacts\aruco-venv\Scripts\python.exe'
}
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
    $stamp = [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ')
    $OutputDirectory = Join-Path $artifactRoot "capacity-$stamp"
}

foreach ($path in @($InputPath, $MomoExecutable, $Python)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Required file was not found: $path"
    }
}
foreach ($count in $SourceCounts) {
    if ($count -lt 1 -or $count -gt 32) {
        throw "SourceCounts must contain only values in 1..32: $count"
    }
}

New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
$receiverOutput = Join-Path $OutputDirectory 'marker-receiver.stdout.log'
$receiverError = Join-Path $OutputDirectory 'marker-receiver.stderr.log'
$summaryPath = Join-Path $OutputDirectory 'summary.json'
$manifestUrl = 'http://127.0.0.1:18190/api/v1/marker-sources?selection=all'
$results = [System.Collections.Generic.List[object]]::new()
$receiver = $null
$failure = $null

try {
    $receiverArguments = @(
        '--no-google-stun',
        'p2p-marker-recv',
        '--manifest-url', $manifestUrl,
        '--connect-parallelism', "$ConnectParallelism",
        '--connect-timeout-ms', '20000'
    )
    $receiver = Start-Process -FilePath $MomoExecutable -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput $receiverOutput -RedirectStandardError $receiverError `
        -ArgumentList $receiverArguments

    foreach ($count in $SourceCounts) {
        & (Join-Path $PSScriptRoot 'Start-VirtualFiveCarDemo.ps1') `
            -InputPath $InputPath -CarCount $count -FrameRate $FrameRate -NoOpen
        if ($LASTEXITCODE -ne 0) {
            throw "Virtual source startup failed for $count sources"
        }

        & $Python (Join-Path $PSScriptRoot 'Wait-MarkerLumaV2Ready.py') `
            --required-source-count $count `
            --timeout-seconds $SourceReadyTimeoutSeconds
        if ($LASTEXITCODE -ne 0) {
            throw "MLY2 warm-up failed for $count sources"
        }

        $reportPath = Join-Path $OutputDirectory ("sources-{0:d2}.json" -f $count)
        $observerArguments = @(
            (Join-Path $PSScriptRoot 'Run-GpuMarkerObserverLumaV2.py'),
            '--required-source-count', "$count",
            '--duration-seconds', "$MeasurementSeconds",
            '--initial-detection-hz', "$InitialDetectionHz",
            '--output', $reportPath
        )
        if ($NoAdaptive) {
            $observerArguments += '--no-adaptive'
        }
        & $Python @observerArguments
        $observerExitCode = $LASTEXITCODE
        if (Test-Path -LiteralPath $reportPath -PathType Leaf) {
            $report = Get-Content -Raw -LiteralPath $reportPath | ConvertFrom-Json
            $results.Add([pscustomobject]@{
                sourceCount = $count
                passed = [bool]$report.passed
                publicationRateHz = $report.publicationRateHz
                effectiveDetectionHz = $report.effectiveDetectionHz
                cycleTimeMs = $report.cycleTimeMs
                profileHistory = $report.profileHistory
                reportPath = $reportPath
            })
        }
        if ($observerExitCode -ne 0) {
            throw "GPU marker validation failed for $count sources"
        }
    }
}
catch {
    $failure = $_
}
finally {
    if ($receiver -and -not $receiver.HasExited) {
        Stop-Process -Id $receiver.Id -Force -ErrorAction SilentlyContinue
        [void]$receiver.WaitForExit(5000)
    }
    & (Join-Path $PSScriptRoot 'Stop-VirtualFiveCarDemo.ps1')
    [pscustomobject]@{
        schemaVersion = 1
        measuredAt = [DateTime]::UtcNow.ToString('o')
        frameRate = $FrameRate
        measurementSeconds = $MeasurementSeconds
        initialDetectionHz = $InitialDetectionHz
        adaptive = -not $NoAdaptive
        connectParallelism = $ConnectParallelism
        results = $results
        error = if ($failure) { $failure.Exception.Message } else { $null }
    } | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $summaryPath -Encoding utf8
}

Write-Host "Capacity summary: $summaryPath"
if ($failure) {
    throw $failure
}
