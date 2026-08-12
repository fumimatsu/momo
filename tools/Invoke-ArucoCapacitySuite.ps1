[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$InputPath,
    [int[]]$SourceCounts = @(1, 2, 4, 6, 8, 10, 12, 16),
    [ValidateRange(5, 3600)][int]$DurationSeconds = 60,
    [ValidateRange(1, 50)][double]$DetectionHz = 25,
    [ValidateRange(0.1, 1.0)][double]$RecognitionQuality = 0.6,
    [ValidateRange(1, 100)][double]$MaxCpuPercent = 60,
    [ValidateSet('opencv', 'qsv', 'cuda')][string[]]$Decoders = @('opencv', 'qsv', 'cuda'),
    [string]$PythonExecutable = "",
    [string]$FfmpegExecutable = "",
    [string]$OutputDirectory = ""
)

$ErrorActionPreference = "Stop"
$InputPath = [System.IO.Path]::GetFullPath($InputPath)
if (-not (Test-Path -LiteralPath $InputPath -PathType Leaf)) { throw "Input video not found: $InputPath" }
if ([string]::IsNullOrWhiteSpace($PythonExecutable)) {
    $PythonExecutable = Join-Path $PSScriptRoot ".artifacts\aruco-venv\Scripts\python.exe"
}
if (-not (Test-Path -LiteralPath $PythonExecutable -PathType Leaf)) { throw "Run Initialize-ArucoCapacity.ps1 first" }
if ([string]::IsNullOrWhiteSpace($FfmpegExecutable)) {
    $FfmpegExecutable = & $PythonExecutable -c "import imageio_ffmpeg; print(imageio_ffmpeg.get_ffmpeg_exe())"
}
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
    $OutputDirectory = Join-Path $PSScriptRoot (".artifacts\aruco-capacity-suite\" + (Get-Date -Format 'yyyyMMdd-HHmmss'))
}
$OutputDirectory = [System.IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
& (Join-Path $PSScriptRoot 'Get-ScaleTestEnvironment.ps1') `
    -PythonExecutable $PythonExecutable -FfmpegExecutable $FfmpegExecutable `
    -OutputPath (Join-Path $OutputDirectory 'environment.json') | Out-Null

$reports = [System.Collections.Generic.List[object]]::new()
foreach ($decoder in $Decoders) {
    $reportPath = Join-Path $OutputDirectory "$decoder-report.json"
    $arguments = @{
        InputPath = $InputPath
        SourceCounts = $SourceCounts
        DurationSeconds = $DurationSeconds
        DetectionHz = $DetectionHz
        RecognitionQuality = $RecognitionQuality
        Decoder = $decoder
        MaxCpuPercent = $MaxCpuPercent
        AllowCapacityFailure = $true
        PythonExecutable = $PythonExecutable
        OutputPath = $reportPath
    }
    if ($decoder -ne 'opencv') { $arguments.FfmpegExecutable = $FfmpegExecutable }
    & (Join-Path $PSScriptRoot 'Measure-ArucoCapacity.ps1') @arguments
    $exitCode = $LASTEXITCODE
    if (-not (Test-Path -LiteralPath $reportPath)) {
        $reports.Add([pscustomobject]@{
            decoder = $decoder
            report = $reportPath
            error = "measurement did not produce a report"
            maximumPassingSources = 0
        })
        continue
    }
    $report = Get-Content -LiteralPath $reportPath -Raw | ConvertFrom-Json
    $passingCounts = @($report.cases | Where-Object passed | ForEach-Object { [int]$_.sourceCount })
    $firstFailure = @($report.cases | Where-Object { -not $_.passed } | Select-Object -First 1)
    $reports.Add([pscustomobject]@{
        decoder = $decoder
        report = $reportPath
        exitCode = $exitCode
        maximumPassingSources = if ($passingCounts.Count -gt 0) { ($passingCounts | Measure-Object -Maximum).Maximum } else { 0 }
        firstFailingSources = if ($firstFailure.Count -gt 0) { [int]$firstFailure[0].sourceCount } else { $null }
    })
}
$summary = [ordered]@{
    schemaVersion = 1
    measuredAt = (Get-Date).ToUniversalTime().ToString('o')
    host = $env:COMPUTERNAME
    input = $InputPath
    inputSha256 = (Get-FileHash -Algorithm SHA256 -LiteralPath $InputPath).Hash
    durationSeconds = $DurationSeconds
    detectionHz = $DetectionHz
    recognitionQuality = $RecognitionQuality
    maxCpuPercent = $MaxCpuPercent
    sourceCounts = @($SourceCounts)
    reports = @($reports)
}
$summaryPath = Join-Path $OutputDirectory 'suite-summary.json'
[System.IO.File]::WriteAllText($summaryPath, ($summary | ConvertTo-Json -Depth 8), [System.Text.UTF8Encoding]::new($false))
$reports | Format-Table decoder, maximumPassingSources, firstFailingSources, exitCode -AutoSize
Write-Host "Suite summary: $summaryPath"
