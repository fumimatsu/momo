[CmdletBinding()]
param(
    [string]$InputPath = "",
    [string[]]$Sources = @(),
    [ValidateRange(1, 240)][int]$DetectionHz = 50,
    [ValidateRange(0.1, 86400)][double]$DurationSeconds = 30,
    [string]$AllowedMarkerIds = "",
    [string]$MappingName = "Local\MomoMarkerObservationsV1",
    [string]$OutputPath = "",
    [string]$PythonExecutable = "",
    [switch]$NoLoop
)

$ErrorActionPreference = "Stop"
if ([string]::IsNullOrWhiteSpace($PythonExecutable)) {
    $PythonExecutable = Join-Path $PSScriptRoot ".artifacts\aruco-venv\Scripts\python.exe"
}
if (-not (Test-Path -LiteralPath $PythonExecutable -PathType Leaf)) {
    throw "AruCo Python environment was not found: $PythonExecutable. Run Initialize-ArucoCapacity.ps1 -IncludeNvCodec."
}
if ($Sources.Count -eq 0) {
    if ([string]::IsNullOrWhiteSpace($InputPath)) {
        $InputPath = Join-Path $PSScriptRoot ".artifacts\aruco-input\cpu-shadow-20260731T122739445Z-732b1f8f-upright-h264.mp4"
    }
    if (-not (Test-Path -LiteralPath $InputPath -PathType Leaf)) {
        throw "Replay input was not found: $InputPath"
    }
    $resolvedInput = (Resolve-Path -LiteralPath $InputPath).Path
    $Sources = 1..4 | ForEach-Object { "sim-{0:D2}={1}" -f $_, $resolvedInput }
}

$arguments = @(
    (Join-Path $PSScriptRoot "Run-GpuMarkerObserverReplay.py"),
    '--detection-hz', $DetectionHz,
    '--duration-seconds', $DurationSeconds,
    '--mapping-name', $MappingName
)
foreach ($source in $Sources) {
    $arguments += @('--source', $source)
}
if (-not [string]::IsNullOrWhiteSpace($AllowedMarkerIds)) {
    $arguments += @('--allowed-marker-ids', $AllowedMarkerIds)
}
if (-not [string]::IsNullOrWhiteSpace($OutputPath)) {
    $arguments += @('--output', $OutputPath)
}
if ($NoLoop) { $arguments += '--no-loop' }

& $PythonExecutable @arguments
if ($LASTEXITCODE -ne 0) {
    throw "GPU Marker Observer replay failed with exit code $LASTEXITCODE"
}
