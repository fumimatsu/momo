param(
    [string]$RaceControlRepository = 'D:\src\momo-race-control',
    [string]$RaceId = 'race-test',
    [ValidateSet('legacy', 'pit-marker', 'hybrid', 'disabled')]
    [string]$HealthRecoveryMode = 'hybrid',
    [string[]]$GameplayAllowCidr = @('127.0.0.1/32'),
    [string]$AyameSignalingUrl = $env:MOMO_AYAME_SIGNALING_URL,
    [string]$AyamePilotRoom113 = $env:MOMO_AYAME_PILOT_ROOM_113,
    [string]$AyamePilotRoom116 = $env:MOMO_AYAME_PILOT_ROOM_116,
    [string]$TeamObserverDirectoryCache = $env:MOMO_TEAM_OBSERVER_DIRECTORY_CACHE,
    [string]$TeamObserverDirectoryOrganization = $env:MOMO_TEAM_OBSERVER_DIRECTORY_ORGANIZATION,
    [string]$TeamObserverDirectoryEvent = $env:MOMO_TEAM_OBSERVER_DIRECTORY_EVENT,
    [string]$TeamObserverDirectoryMaxAge = $(if ([string]::IsNullOrWhiteSpace($env:MOMO_TEAM_OBSERVER_DIRECTORY_MAX_AGE)) { '24h' } else { $env:MOMO_TEAM_OBSERVER_DIRECTORY_MAX_AGE }),
    [string]$RaceDirectoryRefreshConfig = $env:MOMO_RACE_DIRECTORY_REFRESH_CONFIG,
    [string]$RaceDirectoryRefreshScript = $env:MOMO_RACE_DIRECTORY_REFRESH_SCRIPT,
    [ValidateRange(1, 86400)]
    [int]$FuelDriveDurationSeconds = 120,
    [string]$TelemetryLogDirectory = 'C:\fpv-telemetry-logs',
    [ValidateRange(0, 8760)]
    [int]$TelemetryLogRetentionHours = 24,
    [ValidateSet('legacy', 'off')]
    [string]$ObserverVisualOutput = 'legacy',
    [switch]$OpenAdmin,
    [switch]$KeepOpenOnError
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Get-DotEnvValue {
    param(
        [Parameter(Mandatory)] [string]$Path,
        [Parameter(Mandatory)] [string]$Name
    )

    $prefix = "$Name="
    $line = Get-Content -LiteralPath $Path | Where-Object {
        $_.TrimStart().StartsWith($prefix, [StringComparison]::Ordinal)
    } | Select-Object -First 1
    if ($null -eq $line) {
        return ''
    }

    $value = $line.Trim().Substring($prefix.Length).Trim()
    if ($value.Length -ge 2 -and (($value.StartsWith('"') -and $value.EndsWith('"')) -or
        ($value.StartsWith("'") -and $value.EndsWith("'")))) {
        return $value.Substring(1, $value.Length - 2)
    }
    return $value
}

function Test-HttpEndpoint {
    param([Parameter(Mandatory)] [string]$Url)

    try {
        $response = Invoke-WebRequest -UseBasicParsing -Uri $Url -TimeoutSec 2
        return $response.StatusCode -eq 200
    }
    catch {
        return $false
    }
}

function Wait-HttpEndpoint {
    param(
        [Parameter(Mandatory)] [string]$Url,
        [Parameter(Mandatory)] [string]$ServiceName,
        [int]$TimeoutSeconds = 30
    )

    $deadline = [DateTimeOffset]::UtcNow.AddSeconds($TimeoutSeconds)
    while ([DateTimeOffset]::UtcNow -lt $deadline) {
        if (Test-HttpEndpoint -Url $Url) {
            return
        }
        Start-Sleep -Milliseconds 500
    }
    throw "$ServiceName did not become ready within $TimeoutSeconds seconds: $Url"
}

try {
    $repoRoot = Split-Path -Parent $PSScriptRoot
    $relayScript = Join-Path $PSScriptRoot 'start-mads-observer.ps1'
    $relayDirectory = Join-Path $repoRoot 'tools\momo-relay'
    $relayExe = Join-Path $relayDirectory 'momo-local-relay-device-input-v15.exe'
    $observerExe = Join-Path $repoRoot '_build\windows_x86_64\release\momo\Release\momo.exe'
    $raceVars = Join-Path $RaceControlRepository '.dev.vars'
    $raceControlHttpUrl = 'http://127.0.0.1:8787/'
    $raceControlAdminUrl = 'http://127.0.0.1:8787/admin'
    $raceControlWsUrl = "ws://127.0.0.1:8787/ws/races/$RaceId"

    foreach ($requiredPath in @($relayScript, $observerExe, $RaceControlRepository, $raceVars)) {
        if (-not (Test-Path -LiteralPath $requiredPath)) {
            throw "Required path was not found: $requiredPath"
        }
    }

    $viewerToken = Get-DotEnvValue -Path $raceVars -Name 'VIEWER_TOKEN'
    if ([string]::IsNullOrWhiteSpace($viewerToken)) {
        throw "VIEWER_TOKEN is missing from $raceVars"
    }

    if (-not (Test-HttpEndpoint -Url $raceControlHttpUrl)) {
        $npmExe = (Get-Command npm.cmd -ErrorAction SilentlyContinue).Source
        if ([string]::IsNullOrWhiteSpace($npmExe)) {
            throw 'npm.cmd was not found in PATH.'
        }
        $raceStdout = Join-Path $RaceControlRepository 'race-control.stdout.log'
        $raceStderr = Join-Path $RaceControlRepository 'race-control.stderr.log'
        Start-Process -FilePath $npmExe `
            -ArgumentList @('run', 'dev', '--', '--ip', '0.0.0.0', '--port', '8787', '--local', '--persist-to', '.wrangler\state') `
            -WorkingDirectory $RaceControlRepository `
            -RedirectStandardOutput $raceStdout `
            -RedirectStandardError $raceStderr `
            -WindowStyle Hidden | Out-Null
        Wait-HttpEndpoint -Url $raceControlHttpUrl -ServiceName 'Race Control'
        Write-Host 'Race Control started: http://127.0.0.1:8787/admin'
    }
    else {
        Write-Host 'Race Control is already running.'
    }

    $relaySourceFiles = @(
        Get-ChildItem -LiteralPath $relayDirectory -File -Filter '*.go'
        Get-Item -LiteralPath (Join-Path $relayDirectory 'go.mod')
        Get-Item -LiteralPath (Join-Path $relayDirectory 'go.sum')
        Get-ChildItem -LiteralPath (Join-Path $relayDirectory 'web') -Recurse -File
    )
    $rebuildRelay = -not (Test-Path -LiteralPath $relayExe) -or
        (($relaySourceFiles | Measure-Object -Property LastWriteTime -Maximum).Maximum -gt
            (Get-Item -LiteralPath $relayExe).LastWriteTime)

    $relayProcesses = @(Get-CimInstance Win32_Process | Where-Object {
        $_.Name -match '^momo-local-relay-device-input(?:-v\d+)?\.exe$'
    })
    $relayHasExpectedConfig = $relayProcesses.Count -gt 0 -and
        @($relayProcesses | Where-Object {
            $directoryMatches = if ([string]::IsNullOrWhiteSpace($TeamObserverDirectoryCache)) {
                $_.CommandLine -notlike '*-team-observer-directory-cache*'
            }
            else {
                $_.CommandLine.Contains('-team-observer-directory-cache') -and
                    $_.CommandLine.Contains([System.IO.Path]::GetFullPath($TeamObserverDirectoryCache.Trim())) -and
                    $_.CommandLine.Contains($TeamObserverDirectoryOrganization.Trim()) -and
                    $_.CommandLine.Contains($TeamObserverDirectoryEvent.Trim()) -and
                    $_.CommandLine.Contains($TeamObserverDirectoryMaxAge.Trim())
            }
            $_.CommandLine -like "*${raceControlWsUrl}*" -and
            $_.CommandLine -like "*-health-recovery-mode $HealthRecoveryMode*" -and
            $_.CommandLine -like "*-fuel-drive-duration $($FuelDriveDurationSeconds)s*" -and
            $directoryMatches
        }).Count -gt 0

    $launchParameters = @{
        RaceControlUrl = $raceControlWsUrl
        RaceControlViewerToken = $viewerToken
        HealthRecoveryMode = $HealthRecoveryMode
        GameplayAllowCidr = $GameplayAllowCidr
        AyameSignalingUrl = $AyameSignalingUrl
        AyamePilotRoom113 = $AyamePilotRoom113
        AyamePilotRoom116 = $AyamePilotRoom116
        TeamObserverDirectoryCache = $TeamObserverDirectoryCache
        TeamObserverDirectoryOrganization = $TeamObserverDirectoryOrganization
        TeamObserverDirectoryEvent = $TeamObserverDirectoryEvent
        TeamObserverDirectoryMaxAge = $TeamObserverDirectoryMaxAge
        RaceDirectoryRefreshConfig = $RaceDirectoryRefreshConfig
        RaceDirectoryRefreshScript = $RaceDirectoryRefreshScript
        FuelDriveDurationSeconds = $FuelDriveDurationSeconds
        TelemetryLogDirectory = $TelemetryLogDirectory
        TelemetryLogRetentionHours = $TelemetryLogRetentionHours
        ObserverVisualOutput = $ObserverVisualOutput
    }
    if ($rebuildRelay) {
        $launchParameters.RebuildRelay = $true
    }
    elseif ($relayProcesses.Count -gt 0 -and -not $relayHasExpectedConfig) {
        $launchParameters.RestartRelay = $true
    }
    & $relayScript @launchParameters

    Wait-HttpEndpoint -Url 'http://127.0.0.1:8090/pilot.html' -ServiceName 'Relay'
    $observer = Get-CimInstance Win32_Process | Where-Object {
        $_.Name -eq 'momo.exe' -and $_.CommandLine -like '*p2p-recv-multi*'
    } | Select-Object -First 1
    if ($null -eq $observer) {
        throw 'Observer process was not found after startup.'
    }

    $relay = Get-CimInstance Win32_Process | Where-Object {
        $_.Name -match '^momo-local-relay-device-input(?:-v\d+)?\.exe$'
    } | Select-Object -First 1
    if ($null -eq $relay) {
        throw 'Relay process was not found after startup.'
    }

    $raceConnectionDeadline = [DateTimeOffset]::UtcNow.AddSeconds(10)
    $raceConnected = $false
    while ([DateTimeOffset]::UtcNow -lt $raceConnectionDeadline) {
        $raceConnected = [bool](Get-NetTCPConnection -State Established -OwningProcess $relay.ProcessId -ErrorAction SilentlyContinue |
            Where-Object { $_.RemotePort -eq 8787 })
        if ($raceConnected) {
            break
        }
        Start-Sleep -Milliseconds 500
    }
    if (-not $raceConnected) {
        throw 'Relay started, but its Race Control WebSocket connection was not established.'
    }

    Write-Host ''
    Write-Host 'Momo race stack is ready.' -ForegroundColor Green
    Write-Host "Race Control: $raceControlAdminUrl"
    Write-Host 'Relay:        http://127.0.0.1:8090/operations.html'
    Write-Host "Observer PID: $($observer.ProcessId)"

    if ($OpenAdmin) {
        Start-Process $raceControlAdminUrl | Out-Null
    }
}
catch {
    Write-Host ''
    Write-Host "Momo race stack startup failed: $($_.Exception.Message)" -ForegroundColor Red
    if ($KeepOpenOnError) {
        [void](Read-Host 'Press Enter to close')
    }
    exit 1
}
