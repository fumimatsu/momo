param(
    [string]$Device113 = '192.168.11.3',
    [string]$Device114 = '192.168.11.4',
    [string]$Device115 = '192.168.11.5',
    [string]$RaceControlUrl = $env:MOMO_RACE_CONTROL_WS_URL,
    [string]$RaceControlViewerToken = $env:MOMO_RACE_CONTROL_VIEWER_TOKEN,
    [string]$AyameSignalingUrl = $env:MOMO_AYAME_SIGNALING_URL,
    [string]$AyamePilotRoom113 = $env:MOMO_AYAME_PILOT_ROOM_113,
    [string]$AyameClientIdPrefix = 'momo-relay',
    [string]$OperationsAllowCidr = '127.0.0.1/32',
    [switch]$RebuildRelay
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$relayDirectory = Join-Path $repoRoot 'tools\momo-relay'
$relayExe = Join-Path $relayDirectory 'momo-local-relay-device-input-v15.exe'
$goExe = Join-Path $relayDirectory '.toolchain\go\bin\go.exe'
$observerExe = Join-Path $repoRoot '_build\windows_x86_64\release\momo\Release\momo.exe'
$relayLogDirectory = $relayDirectory

foreach ($path in @($observerExe)) {
    if (-not (Test-Path -LiteralPath $path)) {
        throw "Required executable was not found: $path"
    }
}

$relayRunning = @(Get-CimInstance Win32_Process | Where-Object {
    $_.Name -match '^momo-local-relay-device-input(?:-v\d+)?\.exe$'
})
$relaySourceFiles = @(
    Get-Item -LiteralPath (Join-Path $relayDirectory 'main.go')
    Get-ChildItem -LiteralPath (Join-Path $relayDirectory 'web') -Recurse -File
)
$relayNeedsBuild = -not (Test-Path -LiteralPath $relayExe) -or
    (($relaySourceFiles | Measure-Object -Property LastWriteTime -Maximum).Maximum -gt (Get-Item -LiteralPath $relayExe).LastWriteTime)

if ($RebuildRelay) {
    if (-not (Test-Path -LiteralPath $goExe)) {
        throw "Go toolchain was not found: $goExe"
    }
    foreach ($process in $relayRunning) {
        Stop-Process -Id $process.ProcessId -Force
    }
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
    $relayRunning = @()
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
        '-source', "11.3=ws://$Device113`:8080/ws",
        '-source', "11.4=ws://$Device114`:8080/ws",
        '-source', "11.5=ws://$Device115`:8080/ws",
        '-race-car', '11.3=CP-1',
        '-race-car', '11.4=CP-2',
        '-race-car', '11.5=CP-3',
        '-operations-allow-cidr', $OperationsAllowCidr
    )
    if (-not [string]::IsNullOrWhiteSpace($RaceControlUrl)) {
        $relayArgs += '-race-url', $RaceControlUrl.Trim()
        if (-not [string]::IsNullOrWhiteSpace($RaceControlViewerToken)) {
            $relayArgs += '-race-viewer-token', $RaceControlViewerToken.Trim()
        }
    }
    if (-not [string]::IsNullOrWhiteSpace($AyamePilotRoom113)) {
        if ([string]::IsNullOrWhiteSpace($AyameSignalingUrl)) {
            throw 'AyamePilotRoom113 requires AyameSignalingUrl or MOMO_AYAME_SIGNALING_URL.'
        }
        $relayArgs += '-ayame-signaling-url', $AyameSignalingUrl.Trim()
        $relayArgs += '-ayame-client-id-prefix', $AyameClientIdPrefix.Trim()
        $relayArgs += '-ayame-pilot-room', "11.3=$($AyamePilotRoom113.Trim())"
    }
    Start-Process -FilePath $relayExe -ArgumentList $relayArgs `
        -RedirectStandardOutput (Join-Path $relayLogDirectory 'relay-unity.stdout.log') `
        -RedirectStandardError (Join-Path $relayLogDirectory 'relay-unity.stderr.log') `
        -WindowStyle Hidden | Out-Null
    Write-Host 'Relay started: http://127.0.0.1:8090/'
}
else {
    Write-Host 'Relay is already running.'
}

$observerRunning = Get-CimInstance Win32_Process | Where-Object {
    $_.CommandLine -like '*p2p-recv-multi*' -and
    $_.CommandLine -like '*ws://127.0.0.1:8090/ws?role=observer*'
}
if ($null -eq $observerRunning) {
    $observerArgs = @(
        '--use-sdl', '--window-width', '1280', '--window-height', '720',
        '--shared-frame-name', 'Local\MomoObserverFrameV1',
        'p2p-recv-multi',
        '--source', '11.3=ws://127.0.0.1:8090/ws?role=observer&device=11.3',
        '--source-flip', '11.3=HV',
        '--source', '11.4=ws://127.0.0.1:8090/ws?role=observer&device=11.4',
        '--source-flip', '11.4=HV',
        '--source', '11.5=ws://127.0.0.1:8090/ws?role=observer&device=11.5',
        '--source-flip', '11.5=HV'
    )
    Start-Process -FilePath $observerExe -ArgumentList $observerArgs `
        -RedirectStandardOutput (Join-Path $relayLogDirectory 'observer-unity.stdout.log') `
        -RedirectStandardError (Join-Path $relayLogDirectory 'observer-unity.stderr.log') | Out-Null
    Write-Host 'Observer started: Local\MomoObserverFrameV1'
}
else {
    Write-Host 'Observer is already running.'
}
