[CmdletBinding()]
param(
    [string[]]$SourceIds = @(),
    [string]$InputMappingName = 'Local\MomoObserverLumaV1',
    [string]$OutputMappingName = 'Local\MomoMarkerObservationsV1',
    [ValidateRange(1, 240)][int]$DetectionHz = 50,
    [ValidateRange(0, 86400)][double]$DurationSeconds = 0,
    [ValidateRange(0, 300)][double]$WaitForMappingSeconds = 20,
    [string]$AllowedMarkerIds = '',
    [string]$OutputPath = '',
    [string]$PythonExecutable = ''
)

$ErrorActionPreference = 'Stop'
if ([string]::IsNullOrWhiteSpace($PythonExecutable)) {
    $preferredPython = Join-Path $PSScriptRoot '.artifacts\aruco-venv-gpu-313\Scripts\python.exe'
    $fallbackPython = Join-Path $PSScriptRoot '.artifacts\aruco-venv\Scripts\python.exe'
    $PythonExecutable = if (Test-Path -LiteralPath $preferredPython -PathType Leaf) {
        $preferredPython
    } else {
        $fallbackPython
    }
}
if (-not (Test-Path -LiteralPath $PythonExecutable -PathType Leaf)) {
    throw "AruCo Python environment was not found: $PythonExecutable. Run Initialize-ArucoCapacity.ps1 -IncludeNvCodec."
}
if ($SourceIds.Count -gt 4) {
    throw 'At most four Native Observer luma sources can be selected.'
}

$arguments = @(
    (Join-Path $PSScriptRoot 'Run-GpuMarkerObserverLuma.py'),
    '--input-mapping-name', $InputMappingName,
    '--output-mapping-name', $OutputMappingName,
    '--detection-hz', $DetectionHz,
    '--duration-seconds', $DurationSeconds,
    '--wait-for-mapping-seconds', $WaitForMappingSeconds
)
foreach ($sourceId in $SourceIds) {
    $arguments += @('--source-id', $sourceId)
}
if (-not [string]::IsNullOrWhiteSpace($AllowedMarkerIds)) {
    $arguments += @('--allowed-marker-ids', $AllowedMarkerIds)
}
if (-not [string]::IsNullOrWhiteSpace($OutputPath)) {
    $arguments += @('--output', $OutputPath)
}

& $PythonExecutable @arguments
if ($LASTEXITCODE -ne 0) {
    throw "GPU Marker Observer luma run failed with exit code $LASTEXITCODE"
}
