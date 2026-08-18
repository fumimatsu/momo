[CmdletBinding()]
param(
    [switch]$SkipVerification
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$gitRoot = (& git -C $repoRoot rev-parse --show-toplevel).Trim()
if ($LASTEXITCODE -ne 0 -or [System.IO.Path]::GetFullPath($gitRoot) -ne [System.IO.Path]::GetFullPath($repoRoot)) {
    throw "Repository root could not be resolved: $repoRoot"
}

& git -C $repoRoot config --local core.hooksPath .githooks
if ($LASTEXITCODE -ne 0) {
    throw 'Could not configure core.hooksPath.'
}

if (-not $SkipVerification) {
    & (Join-Path $PSScriptRoot 'sync-relay-viewer.ps1') -CheckOnly
}

Write-Host "Repository hooks enabled: $repoRoot\.githooks"
