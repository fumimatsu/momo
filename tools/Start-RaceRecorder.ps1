[CmdletBinding()]
param(
    [string]$Listen = '127.0.0.1:8792',
    [string]$RelayWebSocketUrl = 'ws://127.0.0.1:8090/ws',
    [string]$StorageRoot = 'C:\fpv-recordings',
    [ValidateRange(0, 1048576)]
    [int64]$MinimumFreeGiB = 10,
    [ValidateRange(1, 64)]
    [int]$MaximumSources = 64,
    [string]$StartTimeout = '4s',
    [string]$SegmentDuration = '2m',
    [string]$TokenPath = (Join-Path $env:LOCALAPPDATA 'MomoFPV\secrets\race-recorder-token.txt'),
    [string]$GoExecutable = $env:MOMO_GO_EXE,
    [switch]$Rebuild,
    [switch]$Restart
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = Split-Path -Parent $PSScriptRoot
$moduleRoot = Join-Path $PSScriptRoot 'momo-relay'
$executable = Join-Path $moduleRoot 'momo-race-recorder.exe'
$resolvedStorageRoot = [IO.Path]::GetFullPath($StorageRoot)
$relayUri = $null
if (-not [Uri]::TryCreate($RelayWebSocketUrl.Trim(), [UriKind]::Absolute, [ref]$relayUri) `
    -or $relayUri.Scheme -notin @('ws', 'wss')) {
    throw "RelayWebSocketUrl must be an absolute ws:// or wss:// URL: $RelayWebSocketUrl"
}
if ($SegmentDuration -notmatch '^[1-9][0-9]*(?:ms|s|m|h)$') {
    throw 'SegmentDuration must be a positive Go duration such as 30s or 2m.'
}
if ($StartTimeout -notmatch '^[1-9][0-9]*(?:ms|s|m)$') {
    throw 'StartTimeout must be a positive Go duration such as 4s.'
}

$token = $env:MOMO_RACE_RECORDER_TOKEN
if ([string]::IsNullOrWhiteSpace($token)) {
    if (-not (Test-Path -LiteralPath $TokenPath -PathType Leaf)) {
        throw "Recorder token is unavailable. Set MOMO_RACE_RECORDER_TOKEN or create: $TokenPath"
    }
    $token = [IO.File]::ReadAllText($TokenPath).Trim()
}
if ($token.Trim().Length -lt 32) {
    throw 'Recorder token must contain at least 32 characters.'
}

$running = @(Get-CimInstance Win32_Process -Filter "Name='momo-race-recorder.exe'" -ErrorAction SilentlyContinue)
if ($running.Count -gt 0 -and -not $Restart) {
    throw "Momo Race Recorder is already running (PID $($running.ProcessId -join ', ')). Use -Restart to replace it."
}
if ($Restart) {
    foreach ($process in $running) {
        Stop-Process -Id $process.ProcessId -Force -ErrorAction Stop
    }
}

if ($Rebuild -or -not (Test-Path -LiteralPath $executable -PathType Leaf)) {
    $go = & (Join-Path $PSScriptRoot 'Resolve-GoExecutable.ps1') -RequestedPath $GoExecutable
    Push-Location $moduleRoot
    try {
        & $go build -trimpath -o $executable .\cmd\momo-race-recorder
        if ($LASTEXITCODE -ne 0) {
            throw "Recorder build failed with exit code $LASTEXITCODE."
        }
    }
    finally {
        Pop-Location
    }
}

New-Item -ItemType Directory -Path $resolvedStorageRoot -Force | Out-Null
$logDirectory = Join-Path $resolvedStorageRoot '_logs'
New-Item -ItemType Directory -Path $logDirectory -Force | Out-Null
$stamp = Get-Date -Format 'yyyyMMdd-HHmmss'
$stdout = Join-Path $logDirectory "recorder-$stamp.stdout.log"
$stderr = Join-Path $logDirectory "recorder-$stamp.stderr.log"
$arguments = @(
    '-listen', $Listen,
    '-relay-ws', $RelayWebSocketUrl.Trim(),
    '-storage-root', $resolvedStorageRoot,
    '-minimum-free-gib', $MinimumFreeGiB,
    '-maximum-sources', $MaximumSources,
    '-start-timeout', $StartTimeout,
    '-segment-duration', $SegmentDuration
)

$previousToken = $env:MOMO_RACE_RECORDER_TOKEN
$env:MOMO_RACE_RECORDER_TOKEN = $token.Trim()
try {
    $process = Start-Process -FilePath $executable -ArgumentList $arguments `
        -WorkingDirectory $moduleRoot -RedirectStandardOutput $stdout `
        -RedirectStandardError $stderr -WindowStyle Hidden -PassThru
}
finally {
    $env:MOMO_RACE_RECORDER_TOKEN = $previousToken
}

$statusUri = "http://$Listen/api/v1/status"
$headers = @{ Authorization = "Bearer $($token.Trim())" }
$deadline = (Get-Date).AddSeconds(15)
$status = $null
do {
    if ($process.HasExited) {
        throw "Recorder exited with code $($process.ExitCode). See $stderr"
    }
    try {
        $status = Invoke-RestMethod -Uri $statusUri -Headers $headers -TimeoutSec 2
    }
    catch {
        Start-Sleep -Milliseconds 250
    }
} while ($null -eq $status -and (Get-Date) -lt $deadline)
if ($null -eq $status -or $status.type -ne 'race_recorder_status') {
    Stop-Process -Id $process.Id -Force -ErrorAction SilentlyContinue
    throw "Recorder did not become ready. See $stderr"
}

[pscustomobject]@{
    ProcessId = $process.Id
    State = $status.state
    ApiUrl = "http://$Listen"
    StorageRoot = $resolvedStorageRoot
    Executable = $executable
    StdoutLog = $stdout
    StderrLog = $stderr
}
