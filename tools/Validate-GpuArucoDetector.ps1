[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$InputPath,
    [ValidateRange(1, 1000000)][int]$FrameCount = 1500,
    [ValidateRange(0.1, 1.0)][double]$RecognitionQuality = 0.6,
    [string]$ExpectedMarkerIds = "1,2,3",
    [ValidateRange(3, 255)][int]$AdaptiveWindowSize = 13,
    [ValidateRange(0.01, 1.0)][double]$MaximumComponentAreaRatio = 0.1,
    [string]$PythonExecutable = "",
    [string]$OutputPath = ""
)

$ErrorActionPreference = "Stop"
$InputPath = [System.IO.Path]::GetFullPath($InputPath)
if (-not (Test-Path -LiteralPath $InputPath -PathType Leaf)) { throw "Input video not found: $InputPath" }
if ($AdaptiveWindowSize % 2 -eq 0) { throw "AdaptiveWindowSize must be odd." }
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
if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    $OutputPath = Join-Path $PSScriptRoot (".artifacts\gpu-aruco-detector\" + (Get-Date -Format "yyyyMMdd-HHmmss") + "\report.json")
}
& $PythonExecutable (Join-Path $PSScriptRoot "Validate-GpuArucoDetector.py") `
    --input $InputPath `
    --frame-count $FrameCount `
    --quality $RecognitionQuality `
    --expected-marker-ids $ExpectedMarkerIds `
    --adaptive-window-size $AdaptiveWindowSize `
    --maximum-component-area-ratio $MaximumComponentAreaRatio `
    --output $OutputPath
exit $LASTEXITCODE
