[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$InputPath,
    [ValidateRange(1, 1000000)][int]$FrameCount = 1800,
    [string]$ExpectedMarkerIds = "1",
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
    $OutputPath = Join-Path $PSScriptRoot (".artifacts\gpu-aruco-quadrants\" + (Get-Date -Format "yyyyMMdd-HHmmss") + "\report.json")
}
& $PythonExecutable (Join-Path $PSScriptRoot "Validate-GpuArucoQuadrants.py") `
    --input $InputPath --frame-count $FrameCount `
    --expected-marker-ids $ExpectedMarkerIds --output $OutputPath
exit $LASTEXITCODE
