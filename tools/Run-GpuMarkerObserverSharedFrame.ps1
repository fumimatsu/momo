[CmdletBinding()]
param(
    [string[]]$Sources = @('11.3=0', '11.4=1', '11.5=2', '11.6=3'),
    [string]$InputMappingName = 'Local\MomoObserverFrameV1',
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
if ($Sources.Count -lt 1 -or $Sources.Count -gt 4) {
    throw 'One to four Native Observer slots are required.'
}

$arguments = @(
    (Join-Path $PSScriptRoot 'Run-GpuMarkerObserverSharedFrame.py'),
    '--input-mapping-name', $InputMappingName,
    '--output-mapping-name', $OutputMappingName,
    '--detection-hz', $DetectionHz,
    '--duration-seconds', $DurationSeconds,
    '--wait-for-mapping-seconds', $WaitForMappingSeconds
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

& $PythonExecutable @arguments
if ($LASTEXITCODE -ne 0) {
    throw "GPU Marker Observer shared-frame run failed with exit code $LASTEXITCODE"
}
