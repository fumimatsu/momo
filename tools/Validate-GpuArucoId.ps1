[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$InputPath,
    [ValidateRange(1, 1000000)][int]$FrameCount = 1500,
    [ValidateRange(0.1, 1.0)][double]$RecognitionQuality = 0.6,
    [ValidateRange(0, 16)][int]$MaxHamming = 0,
    [string]$ExpectedMarkerIds = "1,2,3",
    [string]$PythonExecutable = "",
    [string]$OutputPath = ""
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
if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    $OutputPath = Join-Path $PSScriptRoot (".artifacts\gpu-aruco-id\" + (Get-Date -Format "yyyyMMdd-HHmmss") + "\report.json")
}
& $PythonExecutable (Join-Path $PSScriptRoot "Validate-GpuArucoId.py") `
    --input $InputPath `
    --frame-count $FrameCount `
    --quality $RecognitionQuality `
    --max-hamming $MaxHamming `
    --expected-marker-ids $ExpectedMarkerIds `
    --output $OutputPath
exit $LASTEXITCODE
