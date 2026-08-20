param(
    [string]$Device113 = '192.168.11.3',
    [string]$Device114 = '192.168.11.4',
    [string]$Device115 = '192.168.11.5',
    [string]$Device116 = '192.168.11.6',
    [string]$RelayConfigPath = '',
    [string]$RelaySourceRegistryPath = $env:MOMO_RELAY_SOURCE_REGISTRY,
    [string]$RaceControlUrl = $env:MOMO_RACE_CONTROL_WS_URL,
    [string]$RaceControlViewerToken = $env:MOMO_RACE_CONTROL_VIEWER_TOKEN,
    [string]$RaceAudioServiceUrl = $env:MOMO_RACE_AUDIO_SERVICE_URL,
    [ValidateSet('en-US', 'ja-JP')]
    [string]$RaceAudioDefaultLanguage = 'en-US',
    [string]$RaceAudioEnglishVoice = 'am_michael',
    [string]$RaceAudioJapaneseVoice = 'jf_alpha',
    [ValidateRange(0.5, 2.0)]
    [double]$RaceAudioSpeed = 1.04,
    [string]$AyameSignalingUrl = $env:MOMO_AYAME_SIGNALING_URL,
    [string]$AyamePilotRoom113 = $env:MOMO_AYAME_PILOT_ROOM_113,
    [string]$AyamePilotRoom116 = $env:MOMO_AYAME_PILOT_ROOM_116,
    [string]$AyameClientIdPrefix = 'momo-relay',
    [string]$AyameRoomPrefix = $env:MOMO_AYAME_ROOM_PREFIX,
    [string]$OperationsAllowCidr = '127.0.0.1/32',
    [string]$SourceAdminAllowCidr = '192.168.11.0/24',
    [string]$GarageAllowCidr = '192.168.11.0/24',
    [string]$TeamObserverDirectoryCache = $env:MOMO_TEAM_OBSERVER_DIRECTORY_CACHE,
    [string]$TeamObserverDirectoryOrganization = $env:MOMO_TEAM_OBSERVER_DIRECTORY_ORGANIZATION,
    [string]$TeamObserverDirectoryEvent = $env:MOMO_TEAM_OBSERVER_DIRECTORY_EVENT,
    [string]$TeamObserverDirectoryMaxAge = $(if ([string]::IsNullOrWhiteSpace($env:MOMO_TEAM_OBSERVER_DIRECTORY_MAX_AGE)) { '24h' } else { $env:MOMO_TEAM_OBSERVER_DIRECTORY_MAX_AGE }),
    [string]$RaceDirectoryRefreshConfig = $env:MOMO_RACE_DIRECTORY_REFRESH_CONFIG,
    [string]$RaceDirectoryRefreshScript = $env:MOMO_RACE_DIRECTORY_REFRESH_SCRIPT,
    [string[]]$GameplayAllowCidr = @('127.0.0.1/32'),
    [ValidateSet('legacy', 'pit-marker', 'hybrid', 'disabled')]
    [string]$HealthRecoveryMode = $(if ([string]::IsNullOrWhiteSpace($env:MOMO_RELAY_HEALTH_RECOVERY_MODE)) { 'hybrid' } else { $env:MOMO_RELAY_HEALTH_RECOVERY_MODE }),
    [ValidateRange(1, 86400)]
    [int]$FuelDriveDurationSeconds = 120,
    [string]$TelemetryLogDirectory = $(if ([string]::IsNullOrWhiteSpace($env:MOMO_RELAY_TELEMETRY_LOG_DIR)) { 'C:\fpv-telemetry-logs' } else { $env:MOMO_RELAY_TELEMETRY_LOG_DIR }),
    [ValidateRange(0, 8760)]
    [int]$TelemetryLogRetentionHours = 24,
    [string]$ObserverAudioSource = 'all',
    [ValidateRange(0.5, 3.0)]
    [double]$ObserverAudioGain = 1.5,
    [string]$ObserverFrameMappingName = 'Local\MomoObserverFrameV1',
    [string]$ObserverLumaMappingName = 'Local\MomoObserverLumaV1',
    [ValidateSet('legacy', 'off')]
    [string]$ObserverVisualOutput = 'legacy',
    [string]$ObserverRelayWebSocketUrl = 'ws://127.0.0.1:8090/ws',
    [ValidateRange(1, 60)]
    [int]$ObserverSharedOutputFps = 25,
    [string]$ObserverExecutable = '',
    [switch]$ObserverHeadless,
    [string]$ObserverCrashDumpDirectory = '',
    [string]$GoExecutable = $env:MOMO_GO_EXE,
    [switch]$SkipRelay,
    [switch]$RestartRelay,
    [switch]$RestartObserver,
    [switch]$RebuildRelay
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ($SkipRelay -and ($RestartRelay -or $RebuildRelay)) {
    throw 'SkipRelay cannot be combined with RestartRelay or RebuildRelay.'
}

if ([string]::IsNullOrWhiteSpace($ObserverRelayWebSocketUrl)) {
    throw 'ObserverRelayWebSocketUrl must not be empty.'
}
if ([string]::IsNullOrWhiteSpace($ObserverLumaMappingName)) {
    throw 'ObserverLumaMappingName must not be empty.'
}
if ($ObserverVisualOutput -eq 'legacy' -and [string]::IsNullOrWhiteSpace($ObserverFrameMappingName)) {
    throw 'ObserverFrameMappingName must not be empty when ObserverVisualOutput is legacy.'
}
$observerRelayEndpoint = $ObserverRelayWebSocketUrl.Trim()
$observerRelayUri = $null
if (-not [Uri]::TryCreate($observerRelayEndpoint, [UriKind]::Absolute, [ref]$observerRelayUri) `
    -or $observerRelayUri.Scheme -notin @('ws', 'wss')) {
    throw "ObserverRelayWebSocketUrl must be an absolute ws:// or wss:// URL: $ObserverRelayWebSocketUrl"
}
$observerRelayQuerySeparator = if ($observerRelayEndpoint.Contains('?')) { '&' } else { '?' }
$expectedObserverSource = "11.3=$observerRelayEndpoint$($observerRelayQuerySeparator)role=observer&device=11.3"
$observerAudioGainArgument = $ObserverAudioGain.ToString(
    '0.0', [System.Globalization.CultureInfo]::InvariantCulture)
$expectedObserverAudioGain = "--audio-gain $observerAudioGainArgument"
$expectedObserverAudioSource = if ([string]::IsNullOrWhiteSpace($ObserverAudioSource)) {
    ''
} else {
    "--audio-source $($ObserverAudioSource.Trim())"
}
$expectedObserverLumaOutput = "--shared-luma-name $($ObserverLumaMappingName.Trim())"
$expectedObserverFrameOutput = "--shared-frame-name $($ObserverFrameMappingName.Trim())"
$expectedObserverSharedOutputFps = "--shared-output-fps $ObserverSharedOutputFps"
$expectedObserverHeadless = $ObserverHeadless -or $ObserverVisualOutput -eq 'off'

if (-not $SkipRelay -and $HealthRecoveryMode -in @('pit-marker', 'hybrid')) {
    if ([string]::IsNullOrWhiteSpace($env:MOMO_RELAY_GAMEPLAY_TOKEN)) {
        throw "MOMO_RELAY_GAMEPLAY_TOKEN is required when HealthRecoveryMode is $HealthRecoveryMode."
    }
    if ([string]::IsNullOrWhiteSpace($RaceControlUrl)) {
        throw "RaceControlUrl is required when HealthRecoveryMode is $HealthRecoveryMode."
    }
}
if (-not $SkipRelay -and -not [string]::IsNullOrWhiteSpace($RaceAudioServiceUrl)) {
    $raceAudioServiceUri = $null
    if (-not [Uri]::TryCreate($RaceAudioServiceUrl.Trim(), [UriKind]::Absolute, [ref]$raceAudioServiceUri) `
        -or $raceAudioServiceUri.Scheme -notin @('http', 'https')) {
        throw "RaceAudioServiceUrl must be an absolute http:// or https:// URL: $RaceAudioServiceUrl"
    }
    if ([string]::IsNullOrWhiteSpace($env:MOMO_RACE_AUDIO_SERVICE_TOKEN)) {
        throw 'MOMO_RACE_AUDIO_SERVICE_TOKEN is required when RaceAudioServiceUrl is set.'
    }
}

$repoRoot = Split-Path -Parent $PSScriptRoot
$relayDirectory = Join-Path $repoRoot 'tools\momo-relay'
$relayExe = Join-Path $relayDirectory 'momo-local-relay-device-input-v15.exe'
$observerExe = if ([string]::IsNullOrWhiteSpace($ObserverExecutable)) {
    Join-Path $repoRoot '_build\windows_x86_64\release\momo\Release\momo.exe'
} else {
    [System.IO.Path]::GetFullPath($ObserverExecutable.Trim())
}
$relayLogDirectory = $relayDirectory
$resolvedRelayConfigPath = if ([string]::IsNullOrWhiteSpace($RelayConfigPath)) {
    ''
} else {
    [System.IO.Path]::GetFullPath($RelayConfigPath.Trim())
}
if (-not [string]::IsNullOrWhiteSpace($resolvedRelayConfigPath) -and -not (Test-Path -LiteralPath $resolvedRelayConfigPath -PathType Leaf)) {
    throw "Relay config was not found: $resolvedRelayConfigPath"
}
$resolvedRelaySourceRegistryPath = if ([string]::IsNullOrWhiteSpace($RelaySourceRegistryPath)) {
    ''
} else {
    [System.IO.Path]::GetFullPath($RelaySourceRegistryPath.Trim())
}
if (-not [string]::IsNullOrWhiteSpace($resolvedRelaySourceRegistryPath) -and [string]::IsNullOrWhiteSpace($env:MOMO_RELAY_ADMIN_TOKEN)) {
    throw 'MOMO_RELAY_ADMIN_TOKEN is required when RelaySourceRegistryPath is set.'
}
$raceDirectoryRefreshValues = @($RaceDirectoryRefreshConfig, $RaceDirectoryRefreshScript)
$raceDirectoryRefreshConfigured = @($raceDirectoryRefreshValues | Where-Object {
    -not [string]::IsNullOrWhiteSpace($_)
}).Count
if ($raceDirectoryRefreshConfigured -ne 0 -and $raceDirectoryRefreshConfigured -ne 2) {
    throw 'RaceDirectoryRefreshConfig and RaceDirectoryRefreshScript must be configured together.'
}
$resolvedRaceDirectoryRefreshConfig = if ($raceDirectoryRefreshConfigured -eq 0) {
    ''
} else {
    [System.IO.Path]::GetFullPath($RaceDirectoryRefreshConfig.Trim())
}
$resolvedRaceDirectoryRefreshScript = if ($raceDirectoryRefreshConfigured -eq 0) {
    ''
} else {
    [System.IO.Path]::GetFullPath($RaceDirectoryRefreshScript.Trim())
}
$teamObserverDirectoryValues = @(
    $TeamObserverDirectoryCache,
    $TeamObserverDirectoryOrganization,
    $TeamObserverDirectoryEvent
)
$teamObserverDirectoryConfigured = @($teamObserverDirectoryValues | Where-Object {
    -not [string]::IsNullOrWhiteSpace($_)
}).Count
if ($teamObserverDirectoryConfigured -ne 0 -and $teamObserverDirectoryConfigured -ne 3) {
    throw 'TeamObserverDirectoryCache, TeamObserverDirectoryOrganization, and TeamObserverDirectoryEvent must be configured together.'
}
if ($raceDirectoryRefreshConfigured -ne 0 -and $teamObserverDirectoryConfigured -ne 3) {
    throw 'Race Directory startup refresh requires the Team Observer directory cache configuration.'
}
$resolvedTeamObserverDirectoryCache = if ($teamObserverDirectoryConfigured -eq 0) {
    ''
} else {
    [System.IO.Path]::GetFullPath($TeamObserverDirectoryCache.Trim())
}
if (-not [string]::IsNullOrWhiteSpace($resolvedTeamObserverDirectoryCache)) {
    if (-not [string]::IsNullOrWhiteSpace($resolvedRaceDirectoryRefreshConfig)) {
        if (-not (Test-Path -LiteralPath $resolvedRaceDirectoryRefreshConfig -PathType Leaf)) {
            throw "Race Directory refresh config was not found: $resolvedRaceDirectoryRefreshConfig"
        }
        if (-not (Test-Path -LiteralPath $resolvedRaceDirectoryRefreshScript -PathType Leaf)) {
            throw "Race Directory refresh script was not found: $resolvedRaceDirectoryRefreshScript"
        }
        try {
            $refresh = & $resolvedRaceDirectoryRefreshScript -ConfigPath $resolvedRaceDirectoryRefreshConfig
            Write-Host "Race Directory refreshed: $($refresh.status) revision=$($refresh.directoryRevision)"
        }
        catch {
            if (-not (Test-Path -LiteralPath $resolvedTeamObserverDirectoryCache -PathType Leaf)) {
                throw "Race Directory startup refresh failed and no local cache exists: $($_.Exception.Message)"
            }
            Write-Warning "Race Directory startup refresh failed; keeping the last-known-good cache: $($_.Exception.Message)"
        }
    }
    if (-not (Test-Path -LiteralPath $resolvedTeamObserverDirectoryCache -PathType Leaf)) {
        throw "Team Observer directory cache was not found: $resolvedTeamObserverDirectoryCache"
    }
    if ($TeamObserverDirectoryMaxAge.Trim() -notmatch '^[1-9][0-9]*(?:ms|s|m|h)$') {
        throw 'TeamObserverDirectoryMaxAge must be a positive Go duration such as 30m or 1h.'
    }
}
$resolvedObserverCrashDumpDirectory = if ([string]::IsNullOrWhiteSpace($ObserverCrashDumpDirectory)) {
    Join-Path $relayDirectory 'crash_dumps'
} else {
    [System.IO.Path]::GetFullPath($ObserverCrashDumpDirectory.Trim())
}

foreach ($path in @($observerExe)) {
    if (-not (Test-Path -LiteralPath $path)) {
        throw "Required executable was not found: $path"
    }
}

# Native Observer のアクセス違反を Windows Error Reporting の LocalDumps で保存する。
# 同一ユーザーの momo.exe に適用されるが、現行運用では Observer と Native Viewer を
# 同じ実行ファイルで起動するため、原因調査には両方を残す方が有用である。
New-Item -ItemType Directory -Path $resolvedObserverCrashDumpDirectory -Force | Out-Null
$localDumpsKey = 'HKCU:\Software\Microsoft\Windows\Windows Error Reporting\LocalDumps\momo.exe'
New-Item -Path $localDumpsKey -Force | Out-Null
New-ItemProperty -Path $localDumpsKey -Name 'DumpFolder' -PropertyType ExpandString -Value $resolvedObserverCrashDumpDirectory -Force | Out-Null
New-ItemProperty -Path $localDumpsKey -Name 'DumpCount' -PropertyType DWord -Value 10 -Force | Out-Null
New-ItemProperty -Path $localDumpsKey -Name 'DumpType' -PropertyType DWord -Value 1 -Force | Out-Null

$relayRunning = @(Get-CimInstance Win32_Process | Where-Object {
    $_.Name -match '^momo-local-relay-device-input(?:-v\d+)?\.exe$'
})
$relaySourceFiles = @(
    Get-ChildItem -LiteralPath $relayDirectory -File -Filter '*.go'
    Get-Item -LiteralPath (Join-Path $relayDirectory 'go.mod')
    Get-Item -LiteralPath (Join-Path $relayDirectory 'go.sum')
    Get-ChildItem -LiteralPath (Join-Path $relayDirectory 'web') -Recurse -File
)
$relayNeedsBuild = -not $SkipRelay -and (
    -not (Test-Path -LiteralPath $relayExe) -or
    (($relaySourceFiles | Measure-Object -Property LastWriteTime -Maximum).Maximum -gt (Get-Item -LiteralPath $relayExe).LastWriteTime)
)

if (-not $SkipRelay -and ($RebuildRelay -or $RestartRelay) -and $relayRunning.Count -gt 0) {
    foreach ($process in $relayRunning) {
        Stop-Process -Id $process.ProcessId -Force
    }
    $relayRunning = @()
}

if ($RebuildRelay) {
    $goExe = & (Join-Path $PSScriptRoot 'Resolve-GoExecutable.ps1') `
        -RequestedPath $GoExecutable `
        -RequiredVersionPattern 'go1\.26(?:\.|\s)'
    Push-Location $relayDirectory
    try {
        & $goExe build -trimpath -o $relayExe .
        if ($LASTEXITCODE -ne 0) {
            throw "Relay build failed with exit code $LASTEXITCODE"
        }
    }
    finally {
        Pop-Location
    }
    $relayNeedsBuild = $false
}

if (-not $SkipRelay -and $relayNeedsBuild) {
    throw "Relay source is newer than $relayExe. Run this script with -RebuildRelay."
}

if (-not $SkipRelay -and -not (Test-Path -LiteralPath $relayExe)) {
    throw "Required executable was not found: $relayExe"
}
if (-not $SkipRelay -and $relayRunning.Count -eq 0) {
    $relayArgs = @(
        '-listen', ':8090',
        '-operations-allow-cidr', $OperationsAllowCidr,
        '-source-admin-allow-cidr', $SourceAdminAllowCidr,
        '-health-recovery-mode', $HealthRecoveryMode,
        '-fuel-drive-duration', "$($FuelDriveDurationSeconds)s",
        '-garage-allow-cidr', '127.0.0.1/32',
        '-garage-allow-cidr', $GarageAllowCidr
    )
    foreach ($gameplayAllowCidrEntry in $GameplayAllowCidr) {
        $relayArgs += @('-gameplay-allow-cidr', $gameplayAllowCidrEntry)
    }
    if (-not [string]::IsNullOrWhiteSpace($resolvedRelaySourceRegistryPath)) {
        $relayArgs += @('-source-registry', $resolvedRelaySourceRegistryPath)
    }
    if (-not [string]::IsNullOrWhiteSpace($resolvedTeamObserverDirectoryCache)) {
        $relayArgs += @(
            '-team-observer-directory-cache', $resolvedTeamObserverDirectoryCache,
            '-team-observer-directory-organization', $TeamObserverDirectoryOrganization.Trim(),
            '-team-observer-directory-event', $TeamObserverDirectoryEvent.Trim(),
            '-team-observer-directory-max-age', $TeamObserverDirectoryMaxAge.Trim()
        )
    }
    if ([string]::IsNullOrWhiteSpace($resolvedRelayConfigPath)) {
        $relayArgs += @(
            '-source', "11.3=ws://$Device113`:8080/ws",
            '-source', "11.4=ws://$Device114`:8080/ws",
            '-source', "11.5=ws://$Device115`:8080/ws",
            '-source', "11.6=ws://$Device116`:8080/ws",
            '-race-car', '11.3=CP-1',
            '-race-car', '11.4=CP-2',
            '-race-car', '11.5=CP-3',
            '-race-car', '11.6=CP-4'
        )
    } else {
        $relayArgs += '-config', $resolvedRelayConfigPath
    }
    if (-not [string]::IsNullOrWhiteSpace($RaceControlUrl)) {
        $relayArgs += '-race-url', $RaceControlUrl.Trim()
        if (-not [string]::IsNullOrWhiteSpace($RaceControlViewerToken)) {
            $relayArgs += '-race-viewer-token', $RaceControlViewerToken.Trim()
        }
    }
    if (-not [string]::IsNullOrWhiteSpace($RaceAudioServiceUrl)) {
        $relayArgs += '-race-audio-service-url', $RaceAudioServiceUrl.Trim()
        $relayArgs += '-race-audio-default-language', $RaceAudioDefaultLanguage
        $relayArgs += '-race-audio-en-voice', $RaceAudioEnglishVoice.Trim()
        $relayArgs += '-race-audio-ja-voice', $RaceAudioJapaneseVoice.Trim()
        $relayArgs += '-race-audio-speed', $RaceAudioSpeed.ToString(
            '0.00', [System.Globalization.CultureInfo]::InvariantCulture)
    }
    if (-not [string]::IsNullOrWhiteSpace($TelemetryLogDirectory)) {
        $relayArgs += '-telemetry-log-dir', $TelemetryLogDirectory.Trim()
        $relayArgs += '-telemetry-log-retention', "$($TelemetryLogRetentionHours)h"
    }
    $ayamePilotRooms = @(if ([string]::IsNullOrWhiteSpace($resolvedRelayConfigPath)) {
        @(
            if (-not [string]::IsNullOrWhiteSpace($AyamePilotRoom113)) { "11.3=$($AyamePilotRoom113.Trim())" }
            if (-not [string]::IsNullOrWhiteSpace($AyamePilotRoom116)) { "11.6=$($AyamePilotRoom116.Trim())" }
        )
    })
    if (-not [string]::IsNullOrWhiteSpace($resolvedRelayConfigPath) -and -not [string]::IsNullOrWhiteSpace($AyameSignalingUrl)) {
        $relayArgs += '-ayame-signaling-url', $AyameSignalingUrl.Trim()
        $relayArgs += '-ayame-client-id-prefix', $AyameClientIdPrefix.Trim()
    }
    if (-not [string]::IsNullOrWhiteSpace($AyameRoomPrefix)) {
        if ([string]::IsNullOrWhiteSpace($AyameSignalingUrl)) {
            throw 'AyameRoomPrefix requires AyameSignalingUrl or MOMO_AYAME_SIGNALING_URL.'
        }
        if ([string]::IsNullOrWhiteSpace($resolvedRelayConfigPath) -and $ayamePilotRooms.Count -eq 0) {
            $relayArgs += '-ayame-signaling-url', $AyameSignalingUrl.Trim()
            $relayArgs += '-ayame-client-id-prefix', $AyameClientIdPrefix.Trim()
        }
        $relayArgs += '-ayame-room-prefix', $AyameRoomPrefix.Trim()
    }
    if ($ayamePilotRooms.Count -gt 0) {
        if ([string]::IsNullOrWhiteSpace($AyameSignalingUrl)) {
            throw 'An AyamePilotRoom requires AyameSignalingUrl or MOMO_AYAME_SIGNALING_URL.'
        }
        $relayArgs += '-ayame-signaling-url', $AyameSignalingUrl.Trim()
        $relayArgs += '-ayame-client-id-prefix', $AyameClientIdPrefix.Trim()
        foreach ($ayamePilotRoom in $ayamePilotRooms) {
            $relayArgs += '-ayame-pilot-room', $ayamePilotRoom
        }
    }
    Start-Process -FilePath $relayExe -ArgumentList $relayArgs `
        -RedirectStandardOutput (Join-Path $relayLogDirectory 'relay-unity.stdout.log') `
        -RedirectStandardError (Join-Path $relayLogDirectory 'relay-unity.stderr.log') `
        -WindowStyle Hidden | Out-Null
    Write-Host 'Relay started: http://127.0.0.1:8090/'
    if (-not [string]::IsNullOrWhiteSpace($TelemetryLogDirectory)) {
        Write-Host "Telemetry log directory: $($TelemetryLogDirectory.Trim()) (retention: $TelemetryLogRetentionHours h)"
    }
    if (-not [string]::IsNullOrWhiteSpace($resolvedTeamObserverDirectoryCache)) {
        Write-Host "Team Observer directory: $resolvedTeamObserverDirectoryCache ($($TeamObserverDirectoryOrganization.Trim())/$($TeamObserverDirectoryEvent.Trim()))"
    }
}
elseif (-not $SkipRelay) {
    Write-Host 'Relay is already running.'
}
else {
    Write-Host "Local Relay startup skipped. Observer upstream: $observerRelayEndpoint"
}

$observerRunning = @(Get-CimInstance Win32_Process | Where-Object {
    $_.Name -eq 'momo.exe' -and
    $_.CommandLine -like '*p2p-recv-multi*'
})
$observerMatching = @($observerRunning | Where-Object {
    if ([string]::IsNullOrWhiteSpace($_.CommandLine)) {
        return $false
    }
    $audioSourceMatches = if ([string]::IsNullOrWhiteSpace($expectedObserverAudioSource)) {
        -not $_.CommandLine.Contains('--audio-source')
    } else {
        $_.CommandLine.Contains($expectedObserverAudioSource)
    }
    $frameOutputMatches = if ($ObserverVisualOutput -eq 'legacy') {
        $_.CommandLine.Contains($expectedObserverFrameOutput)
    } else {
        -not $_.CommandLine.Contains('--shared-frame-name')
    }
    $headlessMatches = $_.CommandLine.Contains('--shared-output-headless') -eq $expectedObserverHeadless
    $_.CommandLine.Contains($expectedObserverSource) -and
        $_.CommandLine.Contains($expectedObserverAudioGain) -and
        $_.CommandLine.Contains($expectedObserverLumaOutput) -and
        $_.CommandLine.Contains($expectedObserverSharedOutputFps) -and
        $audioSourceMatches -and
        $frameOutputMatches -and
        $headlessMatches
})
if ($RestartObserver -and $observerRunning.Count -gt 0) {
    foreach ($process in $observerRunning) {
        Stop-Process -Id $process.ProcessId -Force
    }
    $observerRunning = @()
    $observerMatching = @()
}
elseif ($observerRunning.Count -gt 0 -and $observerMatching.Count -ne $observerRunning.Count) {
    throw "Observer is already running with different Relay or audio settings. Rerun with -RestartObserver to apply the requested configuration."
}
if ($observerRunning.Count -eq 0) {
    $observerArgs = @(
        '--use-sdl', '--window-width', '1280', '--window-height', '720',
        '--shared-luma-name', $ObserverLumaMappingName.Trim(),
        '--shared-output-fps', $ObserverSharedOutputFps,
        'p2p-recv-multi',
        '--source', "11.3=$observerRelayEndpoint$($observerRelayQuerySeparator)role=observer&device=11.3",
        '--source-flip', '11.3=HV',
        '--source', "11.4=$observerRelayEndpoint$($observerRelayQuerySeparator)role=observer&device=11.4",
        '--source-flip', '11.4=HV',
        '--source', "11.5=$observerRelayEndpoint$($observerRelayQuerySeparator)role=observer&device=11.5",
        '--source-flip', '11.5=HV',
        '--source', "11.6=$observerRelayEndpoint$($observerRelayQuerySeparator)role=observer&device=11.6",
        '--source-flip', '11.6=HV'
    )
    if ($ObserverVisualOutput -eq 'legacy') {
        $observerArgs = @('--shared-frame-name', $ObserverFrameMappingName.Trim()) + $observerArgs
    }
    if ($expectedObserverHeadless) {
        $observerArgs = @('--shared-output-headless') + $observerArgs
    }
    if (-not [string]::IsNullOrWhiteSpace($ObserverAudioSource)) {
        $observerArgs += '--audio-source', $ObserverAudioSource.Trim()
    }
    $observerArgs += '--audio-gain', $observerAudioGainArgument
    $observerHash = (Get-FileHash -LiteralPath $observerExe -Algorithm SHA256).Hash
    Add-Content -LiteralPath (Join-Path $relayLogDirectory 'observer-unity.launch.log') -Value "$(Get-Date -Format o) start sha256=$observerHash crash_dumps=$resolvedObserverCrashDumpDirectory visual_output=$ObserverVisualOutput"
    Start-Process -FilePath $observerExe -ArgumentList $observerArgs `
        -WorkingDirectory $relayDirectory `
        -RedirectStandardOutput (Join-Path $relayLogDirectory 'observer-unity.stdout.log') `
        -RedirectStandardError (Join-Path $relayLogDirectory 'observer-unity.stderr.log') | Out-Null
    if ($ObserverVisualOutput -eq 'legacy') {
        Write-Host "Observer visual output: $($ObserverFrameMappingName.Trim())"
    } else {
        Write-Host 'Observer visual output: off (luma-only)'
    }
    Write-Host "Observer marker luma: $($ObserverLumaMappingName.Trim())"
    Write-Host "Observer logs: $relayLogDirectory\webrtc_logs_*"
    Write-Host "Observer crash dumps: $resolvedObserverCrashDumpDirectory"
    if (-not [string]::IsNullOrWhiteSpace($ObserverAudioSource)) {
        Write-Host "Observer audio source: $($ObserverAudioSource.Trim())"
    }
    Write-Host "Observer audio gain: $observerAudioGainArgument"
}
else {
    Write-Host 'Observer is already running.'
}
