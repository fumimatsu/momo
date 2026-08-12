[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$InputPath,
    [int[]]$SourceCounts = @(1, 2, 4, 6, 8, 12, 16, 24, 32),
    [ValidateRange(5, 3600)][int]$DurationSeconds = 30,
    [ValidateRange(1, 50)][double]$DetectionHz = 15,
    [ValidateRange(0.1, 1.0)][double]$RecognitionQuality = 0.6,
    [ValidateSet('opencv', 'qsv', 'cuda')][string]$Decoder = 'opencv',
    [ValidateRange(1, 100)][double]$MaxCpuPercent = 60,
    [string]$PythonExecutable = "",
    [string]$FfmpegExecutable = "",
    [string]$OutputPath = ""
)

$ErrorActionPreference = "Stop"
$InputPath = [System.IO.Path]::GetFullPath($InputPath)
if (-not (Test-Path -LiteralPath $InputPath -PathType Leaf)) { throw "Input video not found: $InputPath" }
if ($SourceCounts.Count -eq 0 -or @($SourceCounts | Where-Object { $_ -lt 1 -or $_ -gt 32 }).Count -gt 0) {
    throw "SourceCounts must contain values in 1..32"
}
if ([string]::IsNullOrWhiteSpace($PythonExecutable)) {
    $candidates = @(@(
        $env:MOMO_ARUCO_PYTHON,
        (Join-Path $PSScriptRoot ".artifacts\aruco-venv\Scripts\python.exe")
    ) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) -and (Test-Path -LiteralPath $_) })
    if ($candidates.Count -gt 0) { $PythonExecutable = $candidates[0] }
}
if ([string]::IsNullOrWhiteSpace($PythonExecutable) -or -not (Test-Path -LiteralPath $PythonExecutable)) {
    throw "Python with opencv-contrib-python-headless and psutil was not found. Set -PythonExecutable or MOMO_ARUCO_PYTHON."
}
if ($Decoder -in @('qsv', 'cuda')) {
    if ([string]::IsNullOrWhiteSpace($FfmpegExecutable)) { $FfmpegExecutable = $env:MOMO_FFMPEG_EXE }
    if ([string]::IsNullOrWhiteSpace($FfmpegExecutable) -or -not (Test-Path -LiteralPath $FfmpegExecutable)) {
        throw "Hardware decoder test requires -FfmpegExecutable or MOMO_FFMPEG_EXE."
    }
}
& $PythonExecutable -c "import cv2, psutil; assert hasattr(cv2, 'aruco')"
if ($LASTEXITCODE -ne 0) { throw "Python requires cv2.aruco and psutil" }

if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    $OutputPath = Join-Path $PSScriptRoot (".artifacts\aruco-capacity\" + (Get-Date -Format "yyyyMMdd-HHmmss") + "\report.json")
}
$counts = ($SourceCounts | ForEach-Object { [string]$_ }) -join ','
$arguments = @(
    (Join-Path $PSScriptRoot "Measure-ArucoCapacity.py"),
    '--input', $InputPath,
    '--source-counts', $counts,
    '--duration', $DurationSeconds,
    '--detection-hz', $DetectionHz,
    '--quality', $RecognitionQuality,
    '--decoder', $Decoder,
    '--max-cpu-percent', $MaxCpuPercent,
    '--output', $OutputPath
)
if ($Decoder -in @('qsv', 'cuda')) { $arguments += @('--ffmpeg', $FfmpegExecutable) }
& $PythonExecutable @arguments
if ($LASTEXITCODE -ne 0) { exit $LASTEXITCODE }
