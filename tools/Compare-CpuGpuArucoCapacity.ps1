[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$InputPath,
    [int[]]$SourceCounts = @(1, 4, 8, 12, 16),
    [ValidateRange(5, 3600)][int]$DurationSeconds = 30,
    [ValidateRange(1, 60)][double]$DetectionHz = 50,
    [ValidateRange(0.1, 1.0)][double]$RecognitionQuality = 0.6,
    [ValidateRange(1, 100)][double]$MaxCpuPercent = 60,
    [ValidateSet('nvcodec-gpu', 'nvcodec-gpu-batch')][string]$GpuDecoder = 'nvcodec-gpu-batch',
    [string]$ExpectedMarkerIds = "1,2,3",
    [string]$PythonExecutable = "",
    [string]$OutputDirectory = ""
)

$ErrorActionPreference = "Stop"
$InputPath = [System.IO.Path]::GetFullPath($InputPath)
if (-not (Test-Path -LiteralPath $InputPath -PathType Leaf)) { throw "Input video not found: $InputPath" }
if ([string]::IsNullOrWhiteSpace($PythonExecutable)) {
    $candidates = @(@(
        $env:MOMO_ARUCO_PYTHON,
        (Join-Path $PSScriptRoot ".artifacts\aruco-venv\Scripts\python.exe")
    ) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) -and (Test-Path -LiteralPath $_) })
    if ($candidates.Count -gt 0) { $PythonExecutable = $candidates[0] }
}
if ([string]::IsNullOrWhiteSpace($PythonExecutable) -or -not (Test-Path -LiteralPath $PythonExecutable)) {
    throw "GPU ArUco environment was not found. Run Initialize-ArucoCapacity.ps1 -IncludeNvCodec."
}
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
    $OutputDirectory = Join-Path $PSScriptRoot (".artifacts\cpu-gpu-aruco-capacity\" + (Get-Date -Format "yyyyMMdd-HHmmss"))
}
$OutputDirectory = [System.IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
$cpuReport = Join-Path $OutputDirectory "cpu-report.json"
$gpuReport = Join-Path $OutputDirectory "gpu-report.json"
$comparisonReport = Join-Path $OutputDirectory "comparison-report.json"

& (Join-Path $PSScriptRoot "Measure-ArucoCapacity.ps1") `
    -InputPath $InputPath -SourceCounts $SourceCounts -DurationSeconds $DurationSeconds `
    -DetectionHz $DetectionHz -RecognitionQuality $RecognitionQuality `
    -Decoder nvcodec -MaxCpuPercent $MaxCpuPercent -AllowCapacityFailure `
    -PythonExecutable $PythonExecutable -OutputPath $cpuReport

& (Join-Path $PSScriptRoot "Measure-ArucoCapacity.ps1") `
    -InputPath $InputPath -SourceCounts $SourceCounts -DurationSeconds $DurationSeconds `
    -DetectionHz $DetectionHz -RecognitionQuality $RecognitionQuality `
    -Decoder $GpuDecoder -MaxCpuPercent $MaxCpuPercent -AllowCapacityFailure `
    -PythonExecutable $PythonExecutable -OutputPath $gpuReport

& $PythonExecutable (Join-Path $PSScriptRoot "Compare-CpuGpuArucoCapacity.py") `
    --cpu-report $cpuReport --gpu-report $gpuReport `
    --expected-marker-ids $ExpectedMarkerIds --output $comparisonReport
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
