[CmdletBinding()]
param(
    [string]$Python,
    [string]$InputMappingName = 'Local\MomoMarkerLumaV2',
    [string]$OutputMappingName = 'Local\MomoMarkerObservationsV1',
    [ValidateRange(0, 32)]
    [int]$RequiredSourceCount = 0,
    [ValidateRange(0, 86400)]
    [double]$DurationSeconds = 0,
    [ValidateRange(0, 300)]
    [double]$WaitForMappingSeconds = 20,
    [ValidateSet(50, 40, 33, 25)]
    [int]$InitialDetectionHz = 50,
    [string]$Output,
    [switch]$NoAdaptive
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
if ([string]::IsNullOrWhiteSpace($Python)) {
    $preferred = Join-Path $PSScriptRoot '.artifacts\aruco-venv-gpu-313\Scripts\python.exe'
    $fallback = Join-Path $PSScriptRoot '.artifacts\aruco-venv\Scripts\python.exe'
    $candidate = if (Test-Path -LiteralPath $preferred -PathType Leaf) {
        $preferred
    } else {
        $fallback
    }
    if (-not (Test-Path -LiteralPath $candidate -PathType Leaf)) {
        throw "GPU ArUco Python was not found: $candidate"
    }
    $Python = $candidate
}

$arguments = @(
    (Join-Path $PSScriptRoot 'Run-GpuMarkerObserverLumaV2.py'),
    '--input-mapping-name', $InputMappingName,
    '--output-mapping-name', $OutputMappingName,
    '--required-source-count', $RequiredSourceCount,
    '--duration-seconds', $DurationSeconds,
    '--wait-for-mapping-seconds', $WaitForMappingSeconds.ToString([Globalization.CultureInfo]::InvariantCulture),
    '--initial-detection-hz', $InitialDetectionHz
)
if ($NoAdaptive) {
    $arguments += '--no-adaptive'
}
if (-not [string]::IsNullOrWhiteSpace($Output)) {
    $arguments += @('--output', $Output)
}

& $Python @arguments
exit $LASTEXITCODE
