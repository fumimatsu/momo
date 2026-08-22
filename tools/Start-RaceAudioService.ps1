[CmdletBinding()]
param(
    [string]$Listen = '127.0.0.1:18090',
    [ValidateSet('voicevox', 'kokoro', 'fixture', 'piper-plus')]
    [string]$Engine = 'voicevox',
    [string]$VoicevoxUrl = 'http://127.0.0.1:50021',
    [int]$VoicevoxSpeaker = 51,
    [string]$TokenPath = (Join-Path $env:LOCALAPPDATA 'MomoFPV\secrets\race-audio-service-token.txt'),
    [string]$RuntimeRoot = (Join-Path $env:LOCALAPPDATA 'MomoFPV\race-audio-service'),
    [switch]$Restart
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$listenParts = $Listen.Split(':')
$parsedPort = 0
if ($listenParts.Count -ne 2 -or -not [int]::TryParse($listenParts[1], [ref]$parsedPort)) {
    throw "Listen must use host:port format: $Listen"
}
$port = $parsedPort
$healthUrl = "http://127.0.0.1:$port/healthz"
$existing = Get-NetTCPConnection -LocalPort $port -State Listen -ErrorAction SilentlyContinue | Select-Object -First 1
if ($null -ne $existing -and $Restart) {
    Stop-Process -Id $existing.OwningProcess -Force
    Start-Sleep -Milliseconds 500
    $existing = $null
}
if ($null -ne $existing) {
    $health = Invoke-RestMethod -Uri $healthUrl -TimeoutSec 3
    if ($health.status -ne 'ok' -or -not ([string]$health.engine).StartsWith($Engine)) {
        throw "Port $port already hosts a different service: $($health | ConvertTo-Json -Compress)"
    }
    Write-Host "Race Audio Service is already ready: $healthUrl ($($health.engine))"
    return
}
if (-not (Test-Path -LiteralPath $TokenPath -PathType Leaf)) {
    throw "Race Audio Service token file was not found: $TokenPath"
}
$token = [IO.File]::ReadAllText($TokenPath).Trim()
if ($token.Length -lt 32) {
    throw 'Race Audio Service token must contain at least 32 characters.'
}
if ($Engine -eq 'voicevox') {
    [void](Invoke-RestMethod -Uri "$($VoicevoxUrl.TrimEnd('/'))/version" -TimeoutSec 3)
}

$serviceDirectory = Join-Path $PSScriptRoot 'race-audio-service'
$logDirectory = Join-Path $RuntimeRoot 'logs'
New-Item -ItemType Directory -Force -Path $logDirectory | Out-Null
$arguments = @('run', 'python', '.\race_audio_service.py', '--listen', $Listen, '--engine', $Engine)
if ($Engine -eq 'voicevox') {
    $arguments += @('--voicevox-url', $VoicevoxUrl, '--voicevox-speaker', [string]$VoicevoxSpeaker)
}
$process = Start-Process -FilePath 'uv.exe' -ArgumentList $arguments `
    -WorkingDirectory $serviceDirectory -WindowStyle Hidden -PassThru `
    -Environment @{ MOMO_RACE_AUDIO_SERVICE_TOKEN = $token } `
    -RedirectStandardOutput (Join-Path $logDirectory 'race-audio-service.stdout.log') `
    -RedirectStandardError (Join-Path $logDirectory 'race-audio-service.stderr.log')
$token = $null

$health = $null
$deadline = (Get-Date).AddSeconds(30)
do {
    Start-Sleep -Milliseconds 500
    if ($process.HasExited) { break }
    try {
        $health = Invoke-RestMethod -Uri $healthUrl -TimeoutSec 2
        if ($health.status -eq 'ok') { break }
    }
    catch {}
} while ((Get-Date) -lt $deadline)
if ($null -eq $health -or $health.status -ne 'ok') {
    if (-not $process.HasExited) { Stop-Process -Id $process.Id -Force }
    $stderr = Join-Path $logDirectory 'race-audio-service.stderr.log'
    if (Test-Path -LiteralPath $stderr) { Get-Content -LiteralPath $stderr -Tail 80 }
    throw 'Race Audio Service did not become healthy.'
}
[IO.File]::WriteAllText((Join-Path $RuntimeRoot 'race-audio-service.pid'), [string]$process.Id)
Write-Host "Race Audio Service started: $healthUrl ($($health.engine), PID $($process.Id))"
