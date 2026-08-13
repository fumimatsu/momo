[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$InputPath,
    [ValidateRange(1, 1000000)][int]$FrameCount = 1500,
    [ValidateRange(0.1, 1.0)][double]$RecognitionQuality = 0.6,
    [string]$ExpectedMarkerIds = "1,2,3",
    [ValidateRange(0.001, 1.0)][double]$MinimumExpectedAgreement = 0.99,
    [ValidateRange(1, 1000)][int]$MinimumGroupDetections = 3,
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
    throw "Python NVDEC capacity environment was not found. Run Initialize-ArucoCapacity.ps1 -IncludeNvCodec."
}
if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    $OutputPath = Join-Path $PSScriptRoot (".artifacts\aruco-parity\" + (Get-Date -Format "yyyyMMdd-HHmmss") + "\report.json")
}
& $PythonExecutable (Join-Path $PSScriptRoot "Compare-ArucoBackends.py") `
    --input $InputPath `
    --frame-count $FrameCount `
    --quality $RecognitionQuality `
    --expected-marker-ids $ExpectedMarkerIds `
    --minimum-expected-agreement $MinimumExpectedAgreement `
    --minimum-group-detections $MinimumGroupDetections `
    --output $OutputPath
exit $LASTEXITCODE
