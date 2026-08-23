[CmdletBinding()]
param(
    [ValidateRange(5, 3600)] [int]$DurationSeconds = 60,
    [ValidateRange(100, 5000)] [int]$SampleIntervalMS = 1000,
    [string]$RaceControlRepository = '',
    [string]$OutputPath = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Read-DotEnv {
    param([Parameter(Mandatory)] [string]$Path)
    $values = @{}
    foreach ($line in Get-Content -LiteralPath $Path) {
        $trimmed = $line.Trim()
        if ($trimmed.Length -eq 0 -or $trimmed.StartsWith('#') -or -not $trimmed.Contains('=')) { continue }
        $name, $value = $trimmed.Split('=', 2)
        $values[$name.Trim()] = $value.Trim().Trim('"').Trim("'")
    }
    return $values
}

function Get-TransportSnapshot {
    param($Runtime)
    $virtualBaseUrl = if ($null -ne $Runtime.PSObject.Properties['virtualSourceUrl']) {
        [string]$Runtime.virtualSourceUrl
    }
    else { 'http://127.0.0.1:18880' }
    $pilotLoadBaseUrl = if ($null -ne $Runtime.PSObject.Properties['pilotLoadUrl']) {
        [string]$Runtime.pilotLoadUrl
    }
    else { 'http://127.0.0.1:18191' }
    $virtual = Invoke-RestMethod -Uri "$virtualBaseUrl/healthz" -TimeoutSec 5
    $load = Invoke-RestMethod -Uri "$pilotLoadBaseUrl/api/v1/status" -TimeoutSec 5
    return [ordered]@{
        virtualActive = @($virtual.active).Count
        serialOpen = @($virtual.playback | Where-Object serialOpen).Count
        telemetrySent = [uint64](($virtual.playback | Measure-Object telemetrySent -Sum).Sum)
        telemetryDropped = [uint64](($virtual.playback | Measure-Object telemetryDropped -Sum).Sum)
        telemetrySendErrors = [uint64](($virtual.playback | Measure-Object telemetrySendErrors -Sum).Sum)
        commandsReceived = [uint64](($virtual.playback | Measure-Object commandsReceived -Sum).Sum)
        loadConnected = [int]$load.connectedCount
        commandsSent = [uint64](($load.clients | Measure-Object commandsSent -Sum).Sum)
        commandErrors = [uint64](($load.clients | Measure-Object commandErrors -Sum).Sum)
        telemetryReceived = [uint64]$load.telemetry
    }
}

$runtimePath = Join-Path $PSScriptRoot '.artifacts\virtual-fleet-map\runtime.json'
if (-not (Test-Path -LiteralPath $runtimePath -PathType Leaf)) {
    throw 'Virtual Fleet Map demo is not running.'
}
$runtime = Get-Content -LiteralPath $runtimePath -Raw | ConvertFrom-Json
if ([string]::IsNullOrWhiteSpace($RaceControlRepository)) {
    $RaceControlRepository = [IO.Path]::GetFullPath((Join-Path $PSScriptRoot '..\..\momo-race-control'))
}
$devVars = Read-DotEnv -Path (Join-Path $RaceControlRepository '.dev.vars')
if (-not $devVars.ContainsKey('VIEWER_TOKEN')) { throw 'VIEWER_TOKEN was not found.' }
$headers = @{ Authorization = "Bearer $([string]$devVars['VIEWER_TOKEN'])" }
$stateUrl = "$($runtime.raceControlUrl)/api/races/$($runtime.raceId)/state"
if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    $OutputPath = Join-Path $runtime.runRoot 'replay-measurement.json'
}
$OutputPath = [IO.Path]::GetFullPath($OutputPath)

$before = Get-TransportSnapshot -Runtime $runtime
$samples = [Collections.Generic.List[object]]::new()
$deadline = [DateTimeOffset]::UtcNow.AddSeconds($DurationSeconds)
while ([DateTimeOffset]::UtcNow -lt $deadline) {
    $response = Invoke-RestMethod -Uri $stateUrl -Headers $headers -TimeoutSec 5
    $state = if ($null -ne $response.PSObject.Properties['state']) { $response.state } else { $response }
    $ordered = @($state.standings | Sort-Object position)
    $samples.Add([ordered]@{
        at = [DateTimeOffset]::UtcNow.ToString('o')
        order = @($ordered | ForEach-Object { [string]$_.carId })
        standings = @($ordered | ForEach-Object {
            [ordered]@{
                carId = [string]$_.carId
                position = [int]$_.position
                lap = [int]$_.lap
                sector = [int]$_.currentSector
            }
        })
    })
    Start-Sleep -Milliseconds $SampleIntervalMS
}
$after = Get-TransportSnapshot -Runtime $runtime

$orderTransitions = 0
$uniqueOrders = [Collections.Generic.HashSet[string]]::new()
$positionChangesByCar = @{}
$previous = $null
foreach ($sample in $samples) {
    $orderKey = $sample.order -join ','
    [void]$uniqueOrders.Add($orderKey)
    if ($null -ne $previous -and $orderKey -ne ($previous.order -join ',')) { $orderTransitions++ }
    if ($null -ne $previous) {
        $previousPosition = @{}
        foreach ($standing in $previous.standings) { $previousPosition[$standing.carId] = $standing.position }
        foreach ($standing in $sample.standings) {
            if ($previousPosition.ContainsKey($standing.carId) -and $previousPosition[$standing.carId] -ne $standing.position) {
                if (-not $positionChangesByCar.ContainsKey($standing.carId)) { $positionChangesByCar[$standing.carId] = 0 }
                $positionChangesByCar[$standing.carId]++
            }
        }
    }
    $previous = $sample
}

$transportDelta = [ordered]@{}
foreach ($name in @('telemetrySent', 'telemetryDropped', 'telemetrySendErrors', 'commandsReceived', 'commandsSent', 'commandErrors', 'telemetryReceived')) {
    $transportDelta[$name] = [int64]$after[$name] - [int64]$before[$name]
}
$passed = $samples.Count -gt 1 -and $uniqueOrders.Count -gt 1 -and $orderTransitions -gt 0 -and `
    $after.virtualActive -eq [int]$runtime.carCount -and $after.serialOpen -eq [int]$runtime.carCount -and `
    $after.loadConnected -eq [int]$runtime.carCount -and $transportDelta.telemetrySent -gt 0 -and `
    $transportDelta.telemetryReceived -gt 0 -and `
    $transportDelta.commandsSent -gt 0 -and $transportDelta.commandsReceived -gt 0 -and `
    $transportDelta.telemetryDropped -eq 0 -and $transportDelta.telemetrySendErrors -eq 0 -and `
    $transportDelta.commandErrors -eq 0

$result = [ordered]@{
    schemaVersion = 1
    measuredAt = [DateTimeOffset]::UtcNow.ToString('o')
    passed = $passed
    raceId = [string]$runtime.raceId
    carCount = [int]$runtime.carCount
    durationSeconds = $DurationSeconds
    sampleCount = $samples.Count
    uniqueOrders = $uniqueOrders.Count
    orderTransitions = $orderTransitions
    carsWithPositionChanges = @($positionChangesByCar.Keys).Count
    positionChangesByCar = $positionChangesByCar
    transportBefore = $before
    transportAfter = $after
    transportDelta = $transportDelta
    samples = $samples
}
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $OutputPath) | Out-Null
[IO.File]::WriteAllText($OutputPath, ($result | ConvertTo-Json -Depth 12) + [Environment]::NewLine, [Text.UTF8Encoding]::new($false))
Write-Host "Replay measurement: $OutputPath"
Write-Host "Orders: $($uniqueOrders.Count) unique, $orderTransitions transitions, $(@($positionChangesByCar.Keys).Count) cars changed position"
Write-Host "Transport: telemetry=$($transportDelta.telemetrySent) dropped=$($transportDelta.telemetryDropped) commands=$($transportDelta.commandsSent)/$($transportDelta.commandsReceived) errors=$($transportDelta.commandErrors)"
if (-not $passed) { throw 'Virtual Fleet replay measurement did not pass.' }
[pscustomobject]$result
