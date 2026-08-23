[CmdletBinding()]
param(
    [string]$InputPath = '',
    [string]$FFmpegPath = '',
    [string]$GoExecutable = '',
    [string]$PythonExecutable = '',
    [string]$MomoExecutable = '',
    [string]$RaceControlRepository = '',
    [string]$TimingRepository = '',
    [ValidateRange(1, 32)]
    [int]$CarCount = 5,
    [ValidateSet(25, 33, 40, 50)]
    [int]$DetectionHz = 50,
    [ValidateRange(1, 100)]
    [int]$SpreadStartMaxPercent = 80,
    [ValidateRange(0, 600)]
    [double]$ReplayClipDurationSeconds = 30,
    [ValidateRange(30, 600)]
    [int]$EvidenceTimeoutSeconds = 210,
    [string]$ListenHost = '127.0.0.1',
    [int]$RaceControlPort = 18187,
    [int]$CoordinatorPort = 18189,
    [int]$RelayPort = 18190,
    [int]$PilotLoadPort = 18191,
    [int]$VirtualSourcePort = 18880,
    [switch]$ForceTranscode,
    [switch]$NoOpen
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Write-JsonAtomic {
    param(
        [Parameter(Mandatory)] [string]$Path,
        [Parameter(Mandatory)] [object]$Value
    )
    $directory = Split-Path -Parent $Path
    New-Item -ItemType Directory -Force -Path $directory | Out-Null
    $temporary = Join-Path $directory ([IO.Path]::GetRandomFileName())
    try {
        $json = $Value | ConvertTo-Json -Depth 30
        [IO.File]::WriteAllText($temporary, $json + [Environment]::NewLine, [Text.UTF8Encoding]::new($false))
        Move-Item -LiteralPath $temporary -Destination $Path -Force
    }
    finally {
        Remove-Item -LiteralPath $temporary -Force -ErrorAction SilentlyContinue
    }
}

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

function New-RandomToken {
    $bytes = [byte[]]::new(32)
    [Security.Cryptography.RandomNumberGenerator]::Fill($bytes)
    return [Convert]::ToBase64String($bytes).TrimEnd('=').Replace('+', '-').Replace('/', '_')
}

function Wait-HttpReady {
    param(
        [Parameter(Mandatory)] [string]$Url,
        [Parameter(Mandatory)] [string]$Name,
        [int]$TimeoutSeconds = 45
    )
    $deadline = [DateTimeOffset]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTimeOffset]::UtcNow -lt $deadline) {
        try {
            $response = Invoke-WebRequest -UseBasicParsing -Uri $Url -TimeoutSec 2 -SkipHttpErrorCheck
            if ([int]$response.StatusCode -ge 200 -and [int]$response.StatusCode -lt 400) { return }
        }
        catch {}
        Start-Sleep -Milliseconds 300
    }
    throw "$Name did not become ready: $Url"
}

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

function Get-MarkerEvidence {
    param([string]$Path)
    $events = @()
    if (Test-Path -LiteralPath $Path -PathType Leaf) {
        foreach ($line in Get-Content -LiteralPath $Path) {
            if ([string]::IsNullOrWhiteSpace($line)) { continue }
            try {
                $record = $line | ConvertFrom-Json
                if ($record.operation -eq 'enqueue' -and $null -ne $record.envelope.event) {
                    $events += $record.envelope.event
                }
            }
            catch {}
        }
    }
    $markerIds = @($events | ForEach-Object { [int]$_.markerId } | Sort-Object -Unique)
    $bySource = [ordered]@{}
    foreach ($source in $sourceIDs) {
        $bySource[$source] = @($events | Where-Object { $_.sourceId -eq $source }).Count
    }
    return [pscustomobject]@{
        events = $events
        markerIds = $markerIds
        eventCountBySource = $bySource
    }
}

$repoRoot = Split-Path -Parent $PSScriptRoot
$artifactRoot = Join-Path $PSScriptRoot '.artifacts\virtual-fleet-map'
$runtimePath = Join-Path $artifactRoot 'runtime.json'
$runStamp = [DateTimeOffset]::UtcNow.ToString('yyyyMMddTHHmmssfffZ')
$runRoot = Join-Path $artifactRoot "run-$runStamp"
$binRoot = Join-Path $artifactRoot 'bin'

if ([string]::IsNullOrWhiteSpace($InputPath)) {
    $InputPath = Join-Path $PSScriptRoot '.artifacts\aruco-input\cpu-shadow-20260731T122739445Z-732b1f8f-upright-h264.mp4'
}
if ([string]::IsNullOrWhiteSpace($RaceControlRepository)) {
    $RaceControlRepository = [IO.Path]::GetFullPath((Join-Path $repoRoot '..\momo-race-control'))
}
if ([string]::IsNullOrWhiteSpace($TimingRepository)) {
    $TimingRepository = [IO.Path]::GetFullPath((Join-Path $repoRoot '..\momo-race-timing'))
}
if ([string]::IsNullOrWhiteSpace($MomoExecutable)) {
    $MomoExecutable = Join-Path $repoRoot '_build\windows_x86_64\release\momo\Release\momo.exe'
}
if ([string]::IsNullOrWhiteSpace($PythonExecutable)) {
    $bundledPython = Join-Path $PSScriptRoot '.artifacts\aruco-venv\Scripts\python.exe'
    if (Test-Path -LiteralPath $bundledPython -PathType Leaf) {
        $PythonExecutable = $bundledPython
    }
    else {
        $PythonExecutable = (Get-Command python.exe -ErrorAction Stop).Source
    }
}

foreach ($requiredFile in @(
    $InputPath,
    $MomoExecutable,
    $PythonExecutable,
    (Join-Path $RaceControlRepository 'package.json'),
    (Join-Path $RaceControlRepository '.dev.vars'),
    (Join-Path $TimingRepository 'go.mod')
)) {
    if (-not (Test-Path -LiteralPath $requiredFile -PathType Leaf)) {
        throw "Required file was not found: $requiredFile"
    }
}
if (Test-Path -LiteralPath $runtimePath -PathType Leaf) {
    & (Join-Path $PSScriptRoot 'Stop-VirtualFleetMapDemo.ps1')
}
foreach ($port in @($RaceControlPort, $CoordinatorPort, $RelayPort, $PilotLoadPort, $VirtualSourcePort)) {
    if (Get-NetTCPConnection -State Listen -LocalPort $port -ErrorAction SilentlyContinue) {
        throw "TCP port $port is already in use. Stop the existing service or pass another port."
    }
}

New-Item -ItemType Directory -Force -Path $runRoot, $binRoot | Out-Null
$devVars = Read-DotEnv -Path (Join-Path $RaceControlRepository '.dev.vars')
foreach ($requiredToken in @('VIEWER_TOKEN', 'TIMING_INGEST_TOKEN', 'RACE_CONTROL_TOKEN', 'TIMING_AUTHORITY_TOKEN')) {
    if (-not $devVars.ContainsKey($requiredToken) -or [string]::IsNullOrWhiteSpace([string]$devVars[$requiredToken])) {
        throw "$requiredToken is required in $RaceControlRepository\.dev.vars"
    }
}
if ([string]$devVars['TIMING_AUTHORITY_LEASE_REQUIRED'] -ne '1') {
    throw 'TIMING_AUTHORITY_LEASE_REQUIRED=1 is required for this authoritative E2E test.'
}

$go = & (Join-Path $PSScriptRoot 'Resolve-GoExecutable.ps1') -RequestedPath $GoExecutable -RequiredVersionPattern 'go1\.26(?:\.|\s)'
$pilotLoadExe = Join-Path $binRoot 'momo-relay-load.exe'
$coordinatorExe = Join-Path $binRoot 'momo-race-coordinator.exe'
Push-Location (Join-Path $PSScriptRoot 'momo-relay-load')
try {
    & $go build -trimpath -o $pilotLoadExe .
    if ($LASTEXITCODE -ne 0) { throw 'momo-relay-load build failed' }
}
finally { Pop-Location }
Push-Location $TimingRepository
try {
    & $go build -trimpath -o $coordinatorExe .\cmd\momo-race-coordinator
    if ($LASTEXITCODE -ne 0) { throw 'momo-race-coordinator build failed' }
}
finally { Pop-Location }

$sourceIDs = @(1..$CarCount | ForEach-Object { 'virtual-{0:d2}' -f $_ })
$pilots = @()
$vehicles = @()
$entries = @()
$candidates = @()
$assignments = @()
$participants = @()
$colors = @(
    '#75E36A', '#F0C54A', '#3FD4E8', '#ED6B74',
    '#C37AE5', '#FF9F43', '#5B8FF9', '#F08DB8',
    '#B8C4CE', '#C99A6B', '#34CFA1', '#CBEA55',
    '#8C8FF0', '#F57C55', '#72D6B1', '#D56BD7'
)
for ($index = 1; $index -le $CarCount; $index++) {
    $sourceID = $sourceIDs[$index - 1]
    $pilotID = "pilot-virtual-$index"
    $vehicleID = "vehicle-virtual-$index"
    $entryID = "entry-virtual-$index"
    $carID = "CP-$index"
    $color = $colors[($index - 1) % $colors.Count]
    $pilots += [ordered]@{
        pilotId = $pilotID; pilotNo = "$index"; callsign = "PILOT $index"
        displayName = "Pilot $index"; teamName = 'Virtual Fleet'; color = $color
    }
    $vehicles += [ordered]@{
        vehicleId = $vehicleID; vehicleName = "Virtual Car $index"; displayNumber = "$index"
        status = 'active'; sourceBindings = @([ordered]@{ sourceId = $sourceID; active = $true })
    }
    $entries += [ordered]@{
        entryId = $entryID; pilotId = $pilotID; classCode = 'E2E'; entryStatus = 'confirmed'
    }
    $candidates += [ordered]@{ entryId = $entryID; pilotId = $pilotID; classCode = 'E2E' }
    $assignments += [ordered]@{
        entryId = $entryID; vehicleId = $vehicleID; carId = $carID; detectorId = 'marker-node-a'
    }
    $participants += [ordered]@{
        vehicleId = $vehicleID; sourceId = $sourceID; carId = $carID; carName = "Virtual Car $index"
        displayNumber = "$index"; pilotId = $pilotID; pilotName = "Pilot $index"
    }
}

$generatedAt = [DateTimeOffset]::UtcNow.ToString('o')
$directoryRevision = 'rd_' + [Guid]::NewGuid().ToString('N')
$directory = [ordered]@{
    schemaVersion = 1
    generatedAt = $generatedAt
    organization = [ordered]@{ organizationId = 'org-virtual-fleet'; slug = 'virtual-fleet'; name = 'Virtual Fleet E2E' }
    event = [ordered]@{ eventId = 'event-virtual-fleet'; slug = 'virtual-fleet'; name = 'Virtual Marker E2E'; status = 'open' }
    pilots = $pilots
    vehicles = $vehicles
    entries = $entries
    rosterCandidates = $candidates
    directoryRevision = $directoryRevision
}
$directoryCachePath = Join-Path $runRoot 'race-directory-cache.json'
$assignmentsPath = Join-Path $runRoot 'coordinator-assignments.json'
$coursePath = Join-Path $runRoot 'course.json'
$coordinatorConfigPath = Join-Path $runRoot 'coordinator-process.json'
Write-JsonAtomic -Path $directoryCachePath -Value ([ordered]@{
    schemaVersion = 1; fetchedAt = $generatedAt; etag = $directoryRevision; directory = $directory
})
Write-JsonAtomic -Path $assignmentsPath -Value ([ordered]@{
    schemaVersion = 1; revision = 1; assignments = $assignments
})
Write-JsonAtomic -Path $coursePath -Value ([ordered]@{
    schemaVersion = 2
    limits = [ordered]@{ maxActiveCars = 64 }
    roster = [ordered]@{
        raceId = 'virtual-template'; raceRunId = 'virtual-template-run'; revision = 1
        locked = $false; participants = $participants
    }
    markerAssignment = [ordered]@{
        revision = 1
        inputs = @([ordered]@{
            detectorId = 'marker-node-a'; mappingName = 'Local\MomoMarkerObservationsV1'; sourceIds = $sourceIDs
        })
    }
    qualification = [ordered]@{
        mode = 'elapsed_time'; minDetections = 1; exitFrames = 4
        minimumPresenceMs = 40; exitDurationMs = 80; maximumObservationGapMs = 120; candidateCapacity = 10
        maxDetectionsPerFrame = 5; resetOnDifferentMarker = $false; resetOnDroppedBatches = $false
    }
    excludedMarkerIds = @(17, 34, 37)
    gates = @(
        [ordered]@{ gateId = 'checkpoint-1'; kind = 'checkpoint'; markerIds = @(2) }
        [ordered]@{ gateId = 'checkpoint-2'; kind = 'checkpoint'; markerIds = @(3) }
        [ordered]@{ gateId = 'lap-gate'; kind = 'lap'; markerIds = @(1) }
    )
})

$raceID = "virtual-marker-$($runStamp.ToLowerInvariant())"
$raceControlBaseUrl = "http://${ListenHost}:$RaceControlPort"
$relayBaseUrl = "http://${ListenHost}:$RelayPort"
$coordinatorBaseUrl = "http://${ListenHost}:$CoordinatorPort"
Write-JsonAtomic -Path $coordinatorConfigPath -Value ([ordered]@{
    schemaVersion = 1
    listenAddress = "${ListenHost}:$CoordinatorPort"
    directoryCache = $directoryCachePath
    organization = 'virtual-fleet'
    event = 'virtual-fleet'
    assignments = $assignmentsPath
    relayStatusUrl = "$relayBaseUrl/api/v1/status"
    courseTemplate = $coursePath
    stateRoot = (Join-Path $runRoot 'coordinator-state')
    raceControlBaseUrl = $raceControlBaseUrl
    raceId = $raceID
    ownerId = 'virtual-marker-e2e'
    heatId = 'virtual-heat-1'
    sessionType = 'practice'
    rosterRevision = 1
    totalLaps = 999
    sectorCount = 3
    countdownMs = 1000
    formationHoldMs = 500
    leaseTtlMs = 30000
    renewIntervalMs = 10000
    minimumGatePasses = 3
    minimumLapDurationMs = 1000
    markerPollIntervalMs = 20
    timingPublishIntervalMs = 200
    authorityCheckIntervalMs = 1000
    operationTimeoutMs = 5000
    requestOutboxCapacity = 4096
    eventOutboxCapacity = 4096
    directoryMaxAgeMs = 86400000
    relayMaxAgeMs = 5000
    driveDebounceMs = 1
    markerRetryDelayMs = 1000
    allTimeMode = 'elapsed'
})

$raceControl = $null
$markerReceiver = $null
$gpuObserver = $null
$pilotLoad = $null
$coordinator = $null
$virtualStackStarted = $false
$operatorToken = New-RandomToken
try {
    $npm = (Get-Command npm.cmd -ErrorAction Stop).Source
    $raceControl = Start-Process -FilePath $npm -WindowStyle Hidden -PassThru `
        -WorkingDirectory $RaceControlRepository `
        -RedirectStandardOutput (Join-Path $runRoot 'race-control.stdout.log') `
        -RedirectStandardError (Join-Path $runRoot 'race-control.stderr.log') `
        -ArgumentList @('run', 'dev', '--', '--ip', $ListenHost, '--port', "$RaceControlPort", '--local', '--persist-to', (Join-Path $runRoot 'race-control-state'))
    Wait-HttpReady -Url $raceControlBaseUrl -Name 'Race Control'

    $virtualArgs = @{
        InputPath = $InputPath
        FFmpegPath = $FFmpegPath
        GoExecutable = $go
        ListenHost = $ListenHost
        VirtualSourcePort = $VirtualSourcePort
        RelayPort = $RelayPort
        CarCount = $CarCount
        FrameRate = 50
        ClipDurationSeconds = $ReplayClipDurationSeconds
        InputAlreadyUpright = $true
        SpreadStartPositions = $true
        SpreadStartMaxPercent = $SpreadStartMaxPercent
        RaceControlWsUrl = "ws://${ListenHost}:$RaceControlPort/ws/races/$raceID"
        RaceControlViewerToken = [string]$devVars['VIEWER_TOKEN']
        NoOpen = $true
    }
    if ($ForceTranscode) { $virtualArgs.ForceTranscode = $true }
    & (Join-Path $PSScriptRoot 'Start-VirtualFiveCarDemo.ps1') @virtualArgs
    $virtualStackStarted = $true

    $manifestUrl = "$relayBaseUrl/api/v1/marker-sources?selection=all"
    $markerReceiver = Start-Process -FilePath $MomoExecutable -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput (Join-Path $runRoot 'marker-receiver.stdout.log') `
        -RedirectStandardError (Join-Path $runRoot 'marker-receiver.stderr.log') `
        -ArgumentList @('--no-google-stun', 'p2p-marker-recv', '--manifest-url', $manifestUrl, '--connect-parallelism', '4', '--connect-timeout-ms', '20000')
    & $PythonExecutable (Join-Path $PSScriptRoot 'Wait-MarkerLumaV2Ready.py') `
        --required-source-count $CarCount --timeout-seconds 90 --stable-seconds 2
    if ($LASTEXITCODE -ne 0) { throw 'Marker Receiver did not publish all requested MLY2 sources.' }

    $gpuObserver = Start-Process -FilePath $PythonExecutable -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput (Join-Path $runRoot 'gpu-marker-observer.stdout.log') `
        -RedirectStandardError (Join-Path $runRoot 'gpu-marker-observer.stderr.log') `
        -ArgumentList @((Join-Path $PSScriptRoot 'Run-GpuMarkerObserverLumaV2.py'), '--required-source-count', "$CarCount", '--duration-seconds', '0', '--initial-detection-hz', "$DetectionHz")

    $pilotLoad = Start-Process -FilePath $pilotLoadExe -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput (Join-Path $runRoot 'pilot-load.stdout.log') `
        -RedirectStandardError (Join-Path $runRoot 'pilot-load.stderr.log') `
        -ArgumentList @('-relay-url', $relayBaseUrl, '-source-count', "$CarCount", '-observers-per-source', '0', '-pilot-sources', ($sourceIDs -join ','), '-listen', "${ListenHost}:$PilotLoadPort")
    Wait-HttpReady -Url "http://${ListenHost}:$PilotLoadPort/healthz" -Name 'Pilot load clients'

    $driveDeadline = [DateTimeOffset]::UtcNow.AddSeconds(45)
    $relayStatus = $null
    do {
        try {
            $loadStatus = Invoke-RestMethod -Uri "http://${ListenHost}:$PilotLoadPort/api/v1/status" -TimeoutSec 2
            $relayStatus = Invoke-RestMethod -Uri "$relayBaseUrl/api/v1/status" -TimeoutSec 2
            $readyPilots = @($loadStatus.clients | Where-Object { $_.role -eq 'pilot' -and $_.connected -and $_.driveOpen }).Count
            $readySources = @($relayStatus.sources | Where-Object {
                $_.id -in $sourceIDs -and $_.state -eq 'STREAMING' -and $_.drive.enabled
            }).Count
        }
        catch {
            $readyPilots = 0
            $readySources = 0
        }
        if ($readyPilots -eq $CarCount -and $readySources -eq $CarCount) { break }
        Start-Sleep -Milliseconds 500
    } while ([DateTimeOffset]::UtcNow -lt $driveDeadline)
    if ($readyPilots -ne $CarCount -or $readySources -ne $CarCount) {
        throw "DRIVE setup did not become ready: pilots=$readyPilots/$CarCount sources=$readySources/$CarCount"
    }

    $environmentValues = [ordered]@{
        RACE_CONTROL_TOKEN = [string]$devVars['RACE_CONTROL_TOKEN']
        TIMING_INGEST_TOKEN = [string]$devVars['TIMING_INGEST_TOKEN']
        TIMING_AUTHORITY_TOKEN = [string]$devVars['TIMING_AUTHORITY_TOKEN']
        COORDINATOR_OPERATOR_TOKEN = $operatorToken
    }
    $savedEnvironment = @{}
    try {
        foreach ($name in $environmentValues.Keys) {
            $savedEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
            [Environment]::SetEnvironmentVariable($name, $environmentValues[$name], 'Process')
        }
        $coordinator = Start-Process -FilePath $coordinatorExe -WindowStyle Hidden -PassThru `
            -WorkingDirectory $TimingRepository `
            -RedirectStandardOutput (Join-Path $runRoot 'coordinator.stdout.log') `
            -RedirectStandardError (Join-Path $runRoot 'coordinator.stderr.log') `
            -ArgumentList @('-profile', 'coordinator', '-enable-coordinator', '-coordinator-config', $coordinatorConfigPath)
    }
    finally {
        foreach ($name in $environmentValues.Keys) {
            [Environment]::SetEnvironmentVariable($name, $savedEnvironment[$name], 'Process')
        }
    }
    Wait-HttpReady -Url "$coordinatorBaseUrl/healthz" -Name 'Coordinator'

    $virtualRuntime = Get-Content -LiteralPath (Join-Path $PSScriptRoot '.artifacts\virtual-five-car\runtime.json') -Raw | ConvertFrom-Json
    $runtime = [ordered]@{
        createdAt = [DateTimeOffset]::UtcNow.ToString('o')
        runRoot = $runRoot
        raceId = $raceID
        carCount = $CarCount
        sourceIds = $sourceIDs
        sourceVideoOffsetsEnabled = $true
        spreadStartMaxPercent = $SpreadStartMaxPercent
        replayClipDurationSeconds = $ReplayClipDurationSeconds
        raceControlPid = $raceControl.Id
        markerReceiverPid = $markerReceiver.Id
        gpuObserverPid = $gpuObserver.Id
        pilotLoadPid = $pilotLoad.Id
        coordinatorPid = $coordinator.Id
        virtualSourcePid = $virtualRuntime.virtualSourcePid
        relayPid = $virtualRuntime.relayPid
        raceControlUrl = $raceControlBaseUrl
        coordinatorUrl = $coordinatorBaseUrl
        relayUrl = $relayBaseUrl
        teamObserverUrl = "$relayBaseUrl/observer.html?relayHost=${ListenHost}:$RelayPort"
        validationPath = (Join-Path $runRoot 'validation.json')
    }
    Write-JsonAtomic -Path $runtimePath -Value $runtime

    $headers = @{ Authorization = "Bearer $operatorToken"; 'Content-Type' = 'application/json' }
    $prepareID = "virtual-prepare-$($runStamp.ToLowerInvariant())"
    $startID = "virtual-start-$($runStamp.ToLowerInvariant())"
    $prepare = Invoke-RestMethod -Method Post -Uri "$coordinatorBaseUrl/api/v1/prepare" -Headers $headers `
        -Body (@{ commandId = $prepareID; legacyPublisherStopped = $true } | ConvertTo-Json -Compress) -TimeoutSec 30
    if (-not $prepare.ok) { throw "Coordinator Prepare failed: $($prepare.error)" }
    $start = Invoke-RestMethod -Method Post -Uri "$coordinatorBaseUrl/api/v1/start-sequence" -Headers $headers `
        -Body (@{ commandId = $startID } | ConvertTo-Json -Compress) -TimeoutSec 30
    if (-not $start.ok) { throw "Coordinator Start failed: $($start.error)" }

    $coordinatorStatus = Invoke-RestMethod -Uri "$coordinatorBaseUrl/api/v1/status" -TimeoutSec 3
    $runDirectory = [string]$coordinatorStatus.operator.runDirectory
    if ([string]::IsNullOrWhiteSpace($runDirectory)) { throw 'Coordinator did not publish a run directory.' }
    $eventOutboxPath = Join-Path $runDirectory 'reliable-marker-events.jsonl'
    $raceStateUrl = "$raceControlBaseUrl/api/races/$raceID/state"
    $viewerHeaders = @{ Authorization = "Bearer $([string]$devVars['VIEWER_TOKEN'])" }
    $evidenceDeadline = [DateTimeOffset]::UtcNow.AddSeconds($EvidenceTimeoutSeconds)
    $raceState = $null
    $evidence = $null
    $distinctProgress = 0
    $startedCars = 0
    $bestDistinctProgress = 0
    $bestStartedCars = 0
    $allSourcesObserved = $false
    $allGateIDsObserved = $false
    $ready = $false
    do {
        try {
            $raceStateResponse = Invoke-RestMethod -Uri $raceStateUrl -Headers $viewerHeaders -TimeoutSec 3
            $raceState = if ($null -ne $raceStateResponse.PSObject.Properties['state']) {
                $raceStateResponse.state
            }
            else {
                $raceStateResponse
            }
            $evidence = Get-MarkerEvidence -Path $eventOutboxPath
            $startedCars = @($raceState.standings | Where-Object {
                $property = $_.PSObject.Properties['lastMarkerRaceMs']
                $null -ne $property -and $null -ne $property.Value
            }).Count
            $distinctProgress = @($raceState.standings | ForEach-Object {
                $property = $_.PSObject.Properties['lastMarkerRaceMs']
                $markerRaceMs = if ($null -ne $property -and $null -ne $property.Value) {
                    $property.Value
                }
                else {
                    'missing'
                }
                "$($_.currentSector):$markerRaceMs"
            } | Sort-Object -Unique).Count
            $bestStartedCars = [Math]::Max($bestStartedCars, $startedCars)
            $bestDistinctProgress = [Math]::Max($bestDistinctProgress, $distinctProgress)
            $allSourcesObserved = @($sourceIDs | Where-Object { [int]$evidence.eventCountBySource[$_] -gt 0 }).Count -eq $CarCount
            $allGateIDsObserved = @(1, 2, 3 | Where-Object { $_ -in $evidence.markerIds }).Count -eq 3
            $ready = $raceState.phase -eq 'green' -and @($raceState.standings).Count -eq $CarCount -and `
                $startedCars -eq $CarCount -and $distinctProgress -ge 2 -and $allSourcesObserved -and $allGateIDsObserved
        }
        catch {
            $ready = $false
        }
        if ($ready) { break }
        Start-Sleep -Milliseconds 500
    } while ([DateTimeOffset]::UtcNow -lt $evidenceDeadline)
    if (-not $ready) {
        $ids = if ($evidence) { $evidence.markerIds -join ',' } else { '' }
        throw "Actual Marker E2E evidence timed out: bestStarted=$bestStartedCars/$CarCount bestDistinct=$bestDistinctProgress markerIds=$ids"
    }

    $validation = [ordered]@{
        schemaVersion = 1
        passed = $true
        measuredAt = [DateTimeOffset]::UtcNow.ToString('o')
        path = 'video -> Relay -> Native Marker Receiver -> GPU Observer -> Coordinator -> Race Control -> Team Observer'
        raceId = $raceID
        raceRunId = $raceState.raceRunId
        phase = $raceState.phase
        carCount = $CarCount
        markerIdsObserved = $evidence.markerIds
        eventCountBySource = $evidence.eventCountBySource
        distinctMapProgress = $distinctProgress
        standings = @($raceState.standings | ForEach-Object {
            [ordered]@{
                carId = $_.carId; lap = $_.lap; currentSector = $_.currentSector
                lastMarkerIndex = $_.lastMarkerIndex; lastMarkerRaceMs = $_.lastMarkerRaceMs
            }
        })
        runDirectory = $runDirectory
        eventOutbox = $eventOutboxPath
    }
    Write-JsonAtomic -Path $runtime.validationPath -Value $validation
    $runtime.runDirectory = $runDirectory
    $runtime.eventOutbox = $eventOutboxPath
    $runtime.actualMarkerE2EValidated = $true
    Write-JsonAtomic -Path $runtimePath -Value $runtime

    Write-Host ''
    Write-Host 'Actual Marker E2E is running.' -ForegroundColor Green
    Write-Host "Sources:       $($sourceIDs -join ', ')"
    Write-Host "Marker IDs:    $($evidence.markerIds -join ', ')"
    Write-Host "Map progress:  $distinctProgress distinct positions"
    Write-Host "Team Observer: $($runtime.teamObserverUrl)"
    Write-Host "Validation:    $($runtime.validationPath)"
    Write-Host "Stop:          pwsh -ExecutionPolicy Bypass -File `"$PSScriptRoot\Stop-VirtualFleetMapDemo.ps1`""
    if (-not $NoOpen) {
        Start-Process $runtime.teamObserverUrl
    }
}
catch {
    Remove-Item -LiteralPath $runtimePath -Force -ErrorAction SilentlyContinue
    foreach ($process in @($coordinator, $pilotLoad, $gpuObserver, $markerReceiver, $raceControl)) {
        if ($null -ne $process) { Stop-ProcessTree -RootProcessId $process.Id }
    }
    if ($virtualStackStarted) {
        & (Join-Path $PSScriptRoot 'Stop-VirtualFiveCarDemo.ps1')
    }
    throw
}
finally {
    $operatorToken = $null
}
