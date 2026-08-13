param(
    [string]$Device113 = '192.168.11.3',
    [string]$Device114 = '192.168.11.4',
    [string]$Device115 = '192.168.11.5',
    [string]$Device116 = '192.168.11.6',
    [string]$RelayConfigPath = '',
    [string]$RaceControlUrl = $env:MOMO_RACE_CONTROL_WS_URL,
    [string]$RaceControlViewerToken = $env:MOMO_RACE_CONTROL_VIEWER_TOKEN,
    [string]$AyameSignalingUrl = $env:MOMO_AYAME_SIGNALING_URL,
    [string]$AyamePilotRoom113 = $env:MOMO_AYAME_PILOT_ROOM_113,
    [string]$AyamePilotRoom116 = $env:MOMO_AYAME_PILOT_ROOM_116,
    [string]$AyameClientIdPrefix = 'momo-relay',
    [string]$OperationsAllowCidr = '127.0.0.1/32',
    [string]$GarageAllowCidr = '192.168.11.0/24',
    [string]$GameplayAllowCidr = '127.0.0.1/32',
    [ValidateSet('legacy', 'pit-marker', 'hybrid', 'disabled')]
    [string]$HealthRecoveryMode = $(if ([string]::IsNullOrWhiteSpace($env:MOMO_RELAY_HEALTH_RECOVERY_MODE)) { 'hybrid' } else { $env:MOMO_RELAY_HEALTH_RECOVERY_MODE }),
    [ValidateRange(1, 86400)]
    [int]$FuelDriveDurationSeconds = 120,
    [string]$TelemetryLogDirectory = $(if ([string]::IsNullOrWhiteSpace($env:MOMO_RELAY_TELEMETRY_LOG_DIR)) { 'C:\fpv-telemetry-logs' } else { $env:MOMO_RELAY_TELEMETRY_LOG_DIR }),
    [ValidateRange(0, 8760)]
    [int]$TelemetryLogRetentionHours = 24,
    [string]$ObserverAudioSource = 'all',
    [string]$ObserverCrashDumpDirectory = '',
    [string]$GoExecutable = $env:MOMO_GO_EXE,
    [switch]$RestartRelay,
    [switch]$RestartObserver,
    [switch]$RebuildRelay
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ($HealthRecoveryMode -in @('pit-marker', 'hybrid')) {
    if ([string]::IsNullOrWhiteSpace($env:MOMO_RELAY_GAMEPLAY_TOKEN)) {
        throw "MOMO_RELAY_GAMEPLAY_TOKEN is required when HealthRecoveryMode is $HealthRecoveryMode."
    }
    if ([string]::IsNullOrWhiteSpace($RaceControlUrl)) {
        throw "RaceControlUrl is required when HealthRecoveryMode is $HealthRecoveryMode."
    }
}

$repoRoot = Split-Path -Parent $PSScriptRoot
$relayDirectory = Join-Path $repoRoot 'tools\momo-relay'
$relayExe = Join-Path $relayDirectory 'momo-local-relay-device-input-v15.exe'
$observerExe = Join-Path $repoRoot '_build\windows_x86_64\release\momo\Release\momo.exe'
$relayLogDirectory = $relayDirectory
$resolvedRelayConfigPath = if ([string]::IsNullOrWhiteSpace($RelayConfigPath)) {
    ''
} else {
    [System.IO.Path]::GetFullPath($RelayConfigPath.Trim())
}
if (-not [string]::IsNullOrWhiteSpace($resolvedRelayConfigPath) -and -not (Test-Path -LiteralPath $resolvedRelayConfigPath -PathType Leaf)) {
    throw "Relay config was not found: $resolvedRelayConfigPath"
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
$relayNeedsBuild = -not (Test-Path -LiteralPath $relayExe) -or
    (($relaySourceFiles | Measure-Object -Property LastWriteTime -Maximum).Maximum -gt (Get-Item -LiteralPath $relayExe).LastWriteTime)

if (($RebuildRelay -or $RestartRelay) -and $relayRunning.Count -gt 0) {
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

if ($relayNeedsBuild) {
    throw "Relay source is newer than $relayExe. Run this script with -RebuildRelay."
}

if (-not (Test-Path -LiteralPath $relayExe)) {
    throw "Required executable was not found: $relayExe"
}
if ($relayRunning.Count -eq 0) {
    $relayArgs = @(
        '-listen', ':8090',
        '-operations-allow-cidr', $OperationsAllowCidr,
        '-gameplay-allow-cidr', $GameplayAllowCidr,
        '-health-recovery-mode', $HealthRecoveryMode,
        '-fuel-drive-duration', "$($FuelDriveDurationSeconds)s",
        '-garage-allow-cidr', '127.0.0.1/32',
        '-garage-allow-cidr', $GarageAllowCidr
    )
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
    if (-not [string]::IsNullOrWhiteSpace($TelemetryLogDirectory)) {
        $relayArgs += '-telemetry-log-dir', $TelemetryLogDirectory.Trim()
        $relayArgs += '-telemetry-log-retention', "$($TelemetryLogRetentionHours)h"
    }
    $ayamePilotRooms = if ([string]::IsNullOrWhiteSpace($resolvedRelayConfigPath)) {
        @(
            if (-not [string]::IsNullOrWhiteSpace($AyamePilotRoom113)) { "11.3=$($AyamePilotRoom113.Trim())" }
            if (-not [string]::IsNullOrWhiteSpace($AyamePilotRoom116)) { "11.6=$($AyamePilotRoom116.Trim())" }
        )
    } else { @() }
    if (-not [string]::IsNullOrWhiteSpace($resolvedRelayConfigPath) -and -not [string]::IsNullOrWhiteSpace($AyameSignalingUrl)) {
        $relayArgs += '-ayame-signaling-url', $AyameSignalingUrl.Trim()
        $relayArgs += '-ayame-client-id-prefix', $AyameClientIdPrefix.Trim()
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
}
else {
    Write-Host 'Relay is already running.'
}

$observerRunning = @(Get-CimInstance Win32_Process | Where-Object {
    $_.Name -eq 'momo.exe' -and
    $_.CommandLine -like '*p2p-recv-multi*' -and
    $_.CommandLine -like '*ws://127.0.0.1:8090/ws?role=observer*'
})
if ($RestartObserver -and $observerRunning.Count -gt 0) {
    foreach ($process in $observerRunning) {
        Stop-Process -Id $process.ProcessId -Force
    }
    $observerRunning = @()
}
if ($observerRunning.Count -eq 0) {
    $observerArgs = @(
        '--use-sdl', '--window-width', '1280', '--window-height', '720',
        '--shared-frame-name', 'Local\MomoObserverFrameV1',
        'p2p-recv-multi',
        '--source', '11.3=ws://127.0.0.1:8090/ws?role=observer&device=11.3',
        '--source-flip', '11.3=HV',
        '--source', '11.4=ws://127.0.0.1:8090/ws?role=observer&device=11.4',
        '--source-flip', '11.4=HV',
        '--source', '11.5=ws://127.0.0.1:8090/ws?role=observer&device=11.5',
        '--source-flip', '11.5=HV',
        '--source', '11.6=ws://127.0.0.1:8090/ws?role=observer&device=11.6',
        '--source-flip', '11.6=HV'
    )
    if (-not [string]::IsNullOrWhiteSpace($ObserverAudioSource)) {
        $observerArgs += '--audio-source', $ObserverAudioSource.Trim()
    }
    $observerHash = (Get-FileHash -LiteralPath $observerExe -Algorithm SHA256).Hash
    Add-Content -LiteralPath (Join-Path $relayLogDirectory 'observer-unity.launch.log') -Value "$(Get-Date -Format o) start sha256=$observerHash crash_dumps=$resolvedObserverCrashDumpDirectory"
    Start-Process -FilePath $observerExe -ArgumentList $observerArgs `
        -WorkingDirectory $relayDirectory `
        -RedirectStandardOutput (Join-Path $relayLogDirectory 'observer-unity.stdout.log') `
        -RedirectStandardError (Join-Path $relayLogDirectory 'observer-unity.stderr.log') | Out-Null
    Write-Host 'Observer started: Local\MomoObserverFrameV1'
    Write-Host "Observer logs: $relayLogDirectory\webrtc_logs_*"
    Write-Host "Observer crash dumps: $resolvedObserverCrashDumpDirectory"
    if (-not [string]::IsNullOrWhiteSpace($ObserverAudioSource)) {
        Write-Host "Observer audio source: $($ObserverAudioSource.Trim())"
    }
}
else {
    Write-Host 'Observer is already running.'
}
