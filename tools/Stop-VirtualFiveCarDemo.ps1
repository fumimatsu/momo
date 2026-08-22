[CmdletBinding()]
param()

$ErrorActionPreference = 'Stop'
$runtimePath = Join-Path $PSScriptRoot '.artifacts\virtual-five-car\runtime.json'
if (-not (Test-Path -LiteralPath $runtimePath -PathType Leaf)) {
    Write-Host 'Virtual five-car demo is not running.'
    exit 0
}
$runtime = Get-Content -LiteralPath $runtimePath -Raw | ConvertFrom-Json
foreach ($processID in @($runtime.relayPid, $runtime.virtualSourcePid)) {
    if ($processID -and (Get-Process -Id $processID -ErrorAction SilentlyContinue)) {
        Stop-Process -Id $processID -Force
    }
}
Remove-Item -LiteralPath $runtimePath -Force
Write-Host 'Virtual five-car demo stopped.'
