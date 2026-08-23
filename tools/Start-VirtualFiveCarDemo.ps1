[CmdletBinding()]
param(
    [string]$InputPath = "$HOME\Downloads\cpu-shadow-20260731T093643811Z-9ee91411\cpu-shadow-20260731T093643811Z-9ee91411.webm",
    [string]$FFmpegPath = '',
    [string]$GoExecutable = '',
    [string]$ListenHost = '127.0.0.1',
    [int]$VirtualSourcePort = 18880,
    [int]$RelayPort = 18190,
    [ValidateRange(1, 64)]
    [int]$CarCount = 5,
    [ValidateRange(1, 60)]
    [int]$FrameRate = 50,
    [ValidateRange(0, 600)]
    [double]$ClipDurationSeconds = 0,
    [switch]$InputAlreadyUpright,
    [switch]$SpreadStartPositions,
    [ValidateRange(0, 100)]
    [int]$SpreadStartMaxPercent = 0,
    [string]$RaceControlWsUrl = '',
    [string]$RaceControlViewerToken = '',
    [switch]$ForceTranscode,
    [switch]$NoOpen
)

$ErrorActionPreference = 'Stop'
$repoRoot = Split-Path -Parent $PSScriptRoot
$relayRoot = Join-Path $PSScriptRoot 'momo-relay'
$artifactRoot = Join-Path $PSScriptRoot '.artifacts\virtual-five-car'
$runtimePath = Join-Path $artifactRoot 'runtime.json'
$virtualSourceExe = Join-Path $artifactRoot 'momo-virtual-source.exe'
$relayExe = Join-Path $artifactRoot 'momo-relay.exe'
$virtualSourceLog = Join-Path $artifactRoot 'momo-virtual-source.log'
$virtualSourceErrorLog = Join-Path $artifactRoot 'momo-virtual-source.err.log'
$relayLog = Join-Path $artifactRoot 'momo-relay.log'
$relayErrorLog = Join-Path $artifactRoot 'momo-relay.err.log'

New-Item -ItemType Directory -Force -Path $artifactRoot | Out-Null
if (Test-Path -LiteralPath $runtimePath -PathType Leaf) {
    & (Join-Path $PSScriptRoot 'Stop-VirtualFiveCarDemo.ps1')
}
foreach ($port in @($VirtualSourcePort, $RelayPort)) {
    if (Get-NetTCPConnection -State Listen -LocalPort $port -ErrorAction SilentlyContinue) {
        throw "TCP port $port is already in use. Pass a different port explicitly."
    }
}
if (-not (Test-Path -LiteralPath $InputPath -PathType Leaf)) {
    throw "Replay input was not found: $InputPath"
}
if (-not [string]::IsNullOrWhiteSpace($RaceControlWsUrl) -and [string]::IsNullOrWhiteSpace($RaceControlViewerToken)) {
    throw 'RaceControlViewerToken is required when RaceControlWsUrl is configured'
}
$inputItem = Get-Item -LiteralPath $InputPath
$inputIdentity = '{0}|{1}|{2}|{3}|{4}|{5}' -f $inputItem.FullName, $inputItem.Length, $inputItem.LastWriteTimeUtc.Ticks, $FrameRate, [bool]$InputAlreadyUpright, $ClipDurationSeconds
$inputIdentityBytes = [Text.Encoding]::UTF8.GetBytes($inputIdentity)
$inputIdentityHash = [Security.Cryptography.SHA256]::HashData($inputIdentityBytes)
$inputFingerprint = ([Convert]::ToHexString($inputIdentityHash)).Substring(0, 12).ToLowerInvariant()
$inputBaseName = [IO.Path]::GetFileNameWithoutExtension($inputItem.Name) -replace '[^A-Za-z0-9._-]', '-'
$h264Path = Join-Path $artifactRoot ("{0}-{1}-{2}fps.h264" -f $inputBaseName, $inputFingerprint, $FrameRate)

if ([string]::IsNullOrWhiteSpace($FFmpegPath)) {
    $ffmpeg = Get-Command ffmpeg.exe -ErrorAction SilentlyContinue
    if ($ffmpeg) {
        $FFmpegPath = $ffmpeg.Source
    }
    else {
        $bundled = 'C:\src\Remotion\node_modules\@remotion\compositor-win32-x64-msvc\ffmpeg.exe'
        if (Test-Path -LiteralPath $bundled -PathType Leaf) {
            $FFmpegPath = $bundled
        }
    }
}
if ([string]::IsNullOrWhiteSpace($FFmpegPath) -or -not (Test-Path -LiteralPath $FFmpegPath -PathType Leaf)) {
    throw 'ffmpeg.exe was not found. Pass -FFmpegPath explicitly.'
}
$go = & (Join-Path $PSScriptRoot 'Resolve-GoExecutable.ps1') -RequestedPath $GoExecutable -RequiredVersionPattern 'go1\.26(?:\.|\s)'

if ($ForceTranscode -or -not (Test-Path -LiteralPath $h264Path -PathType Leaf)) {
    Write-Host 'Preparing upright H.264 loop input...'
    $videoFilter = if ($InputAlreadyUpright) { 'scale=960:528' } else { 'hflip,vflip,scale=960:528' }
    $durationArguments = if ($ClipDurationSeconds -gt 0) {
        @('-t', $ClipDurationSeconds.ToString('0.###', [Globalization.CultureInfo]::InvariantCulture))
    }
    else {
        @()
    }
    & $FFmpegPath -hide_banner -loglevel warning -y -i $InputPath -an `
        -vf $videoFilter -r $FrameRate @durationArguments `
        -c:v libx264 -preset veryfast -tune zerolatency -profile:v baseline -level 3.1 `
        -pix_fmt yuv420p -g $FrameRate -keyint_min $FrameRate -sc_threshold 0 `
        -x264-params 'aud=1:repeat-headers=1' -f h264 $h264Path
    if ($LASTEXITCODE -ne 0) {
        throw "ffmpeg failed with exit code $LASTEXITCODE"
    }
}

Push-Location $relayRoot
try {
    & $go build -o $virtualSourceExe .\cmd\momo-virtual-source
    if ($LASTEXITCODE -ne 0) { throw 'momo-virtual-source build failed' }
    & $go build -o $relayExe .
    if ($LASTEXITCODE -ne 0) { throw 'momo-relay build failed' }
}
finally {
    Pop-Location
}

$sourceIDs = @(1..$CarCount | ForEach-Object { 'virtual-{0:d2}' -f $_ })
$sourceArguments = @()
foreach ($index in 1..$CarCount) {
    $sourceID = $sourceIDs[$index - 1]
    $sourceArguments += @('-source', "$sourceID=ws://${ListenHost}:$VirtualSourcePort/ws/$sourceID")
    $sourceArguments += @('-race-car', "$sourceID=CP-$index")
}

$virtualSource = $null
$relay = $null
try {
    $virtualSourceArguments = @('-listen', "${ListenHost}:$VirtualSourcePort", '-input', $h264Path, '-fps', "$FrameRate", '-sources', ($sourceIDs -join ','))
    if ($SpreadStartPositions) {
        $virtualSourceArguments += '-spread-starts'
        if ($SpreadStartMaxPercent -gt 0) {
            $virtualSourceArguments += @('-spread-start-max-percent', "$SpreadStartMaxPercent")
        }
    }
    $virtualSource = Start-Process -FilePath $virtualSourceExe -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput $virtualSourceLog -RedirectStandardError $virtualSourceErrorLog `
        -ArgumentList $virtualSourceArguments

    $relayEnvironmentNames = @(
        'MOMO_RELAY_SOURCE_REGISTRY',
        'MOMO_RACE_CONTROL_WS_URL',
        'MOMO_RACE_CONTROL_VIEWER_TOKEN',
        'MOMO_RACE_AUDIO_SERVICE_URL',
        'MOMO_RACE_AUDIO_SERVICE_TOKEN',
        'MOMO_AYAME_SIGNALING_KEY',
        'MOMO_AYAME_ROOM_PREFIX',
        'MOMO_RELAY_HEALTH_RECOVERY_MODE',
        'MOMO_TEAM_OBSERVER_DIRECTORY_CACHE',
        'MOMO_TEAM_OBSERVER_DIRECTORY_ORGANIZATION',
        'MOMO_TEAM_OBSERVER_DIRECTORY_EVENT',
        'MOMO_RELAY_GAMEPLAY_TOKEN',
        'MOMO_RELAY_ADMIN_TOKEN'
    )
    $previousRelayEnvironment = @{}
    try {
        foreach ($name in $relayEnvironmentNames) {
            $previousRelayEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, 'Process')
            [Environment]::SetEnvironmentVariable($name, $null, 'Process')
        }
        if (-not [string]::IsNullOrWhiteSpace($RaceControlWsUrl)) {
            [Environment]::SetEnvironmentVariable('MOMO_RACE_CONTROL_WS_URL', $RaceControlWsUrl.Trim(), 'Process')
            [Environment]::SetEnvironmentVariable('MOMO_RACE_CONTROL_VIEWER_TOKEN', $RaceControlViewerToken, 'Process')
        }
        $relay = Start-Process -FilePath $relayExe -WindowStyle Hidden -PassThru `
            -RedirectStandardOutput $relayLog -RedirectStandardError $relayErrorLog `
            -ArgumentList (@('-listen', "${ListenHost}:$RelayPort", '-health-recovery-mode', 'disabled', '-operations-allow-cidr', '127.0.0.0/8', '-garage-allow-cidr', '127.0.0.0/8') + $sourceArguments)
    }
    finally {
        foreach ($name in $relayEnvironmentNames) {
            [Environment]::SetEnvironmentVariable($name, $previousRelayEnvironment[$name], 'Process')
        }
    }

    @{
        createdAt = (Get-Date).ToString('o')
        virtualSourcePid = $virtualSource.Id
        relayPid = $relay.Id
        virtualSourceUrl = "http://${ListenHost}:$VirtualSourcePort"
        relayUrl = "http://${ListenHost}:$RelayPort"
        frameRate = $FrameRate
        clipDurationSeconds = $ClipDurationSeconds
        carCount = $CarCount
        sources = $sourceIDs
        inputPath = $inputItem.FullName
        h264Path = $h264Path
        spreadStartPositions = [bool]$SpreadStartPositions
        spreadStartMaxPercent = $SpreadStartMaxPercent
        raceControlConnected = -not [string]::IsNullOrWhiteSpace($RaceControlWsUrl)
    } | ConvertTo-Json | Set-Content -LiteralPath $runtimePath -Encoding utf8

    $deadline = (Get-Date).AddSeconds(30)
    do {
        Start-Sleep -Milliseconds 500
        try {
            $status = Invoke-RestMethod -Uri "http://${ListenHost}:$RelayPort/api/v1/status" -TimeoutSec 2
        }
        catch {
            $status = $null
        }
    } until ($status -or (Get-Date) -ge $deadline)
    if (-not $status) {
        throw "Virtual Relay did not start. See $relayErrorLog"
    }
}
catch {
    foreach ($process in @($relay, $virtualSource)) {
        if ($process -and (Get-Process -Id $process.Id -ErrorAction SilentlyContinue)) {
            Stop-Process -Id $process.Id -Force
        }
    }
    Remove-Item -LiteralPath $runtimePath -Force -ErrorAction SilentlyContinue
    throw
}

$observerUrl = "http://${ListenHost}:$RelayPort/observer.html?relayHost=${ListenHost}:$RelayPort"
Write-Host "Virtual sources: $($sourceIDs -join ', ')"
Write-Host "Relay: $($status.sources.Count) configured sources"
Write-Host "Team Observer: $observerUrl"
Write-Host "Stop: powershell -ExecutionPolicy Bypass -File `"$PSScriptRoot\Stop-VirtualFiveCarDemo.ps1`""
if (-not $NoOpen) {
    Start-Process $observerUrl
}
