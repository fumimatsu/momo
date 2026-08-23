[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$artifactRoot = Join-Path $PSScriptRoot '.artifacts\virtual-fleet-map'
$runtimePath = Join-Path $artifactRoot 'runtime.json'

function Stop-ProcessTree {
    param([long]$RootProcessId)
    if (-not $RootProcessId) { return }
    $children = @(Get-CimInstance Win32_Process -ErrorAction SilentlyContinue | Where-Object {
        $_.ParentProcessId -eq $RootProcessId
    })
    foreach ($child in $children) {
        Stop-ProcessTree -RootProcessId $child.ProcessId
    }
    Stop-Process -Id $RootProcessId -Force -ErrorAction SilentlyContinue
}

if (Test-Path -LiteralPath $runtimePath -PathType Leaf) {
    $runtime = Get-Content -LiteralPath $runtimePath -Raw | ConvertFrom-Json
    foreach ($property in @('coordinatorPid', 'pilotLoadPid', 'gpuObserverPid', 'markerReceiverPid', 'raceControlPid')) {
        if ($null -ne $runtime.PSObject.Properties[$property] -and $runtime.$property) {
            Stop-ProcessTree -RootProcessId ([long]$runtime.$property)
        }
    }
    Remove-Item -LiteralPath $runtimePath -Force
}
& (Join-Path $PSScriptRoot 'Stop-VirtualFiveCarDemo.ps1')
Write-Host 'Virtual Marker E2E demo stopped.'
