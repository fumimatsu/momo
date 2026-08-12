[CmdletBinding()]
param(
    [string]$RelayUrl = "http://127.0.0.1:8090",
    [ValidateRange(1, 86400)][int]$DurationSeconds = 300,
    [ValidateRange(1, 60)][int]$SampleIntervalSeconds = 1,
    [ValidateRange(0, 3600)][int]$WarmupSeconds = 10,
    [ValidateRange(1, 32)][int]$ExpectedSources = 4,
    [int]$MinStreamingSources = -1,
    [ValidateRange(1, 100)][double]$MaxCpuPercent = 60,
    [ValidateRange(1, 65536)][double]$MaxMemoryGrowthMB = 128,
    [ValidateRange(0, 60000)][double]$MaxRtpAgeMs = 1000,
    [ValidateRange(0, 240)][double]$MinIngressFps = 20,
    [ValidateRange(0, 1000000)][long]$MaxRaceWriteErrorIncrease = 0,
    [ValidateRange(0, 1000000)][long]$MaxTelemetryDropIncrease = 0,
    [int]$ProcessId = 0,
    [string]$OutputPath = ""
)

$ErrorActionPreference = "Stop"

function Get-Percentile {
    param([double[]]$Values, [double]$Percentile)
    if (-not $Values -or $Values.Count -eq 0) { return $null }
    $sorted = @($Values | Sort-Object)
    $index = [Math]::Ceiling(($Percentile / 100.0) * $sorted.Count) - 1
    return [double]$sorted[[Math]::Max(0, [Math]::Min($index, $sorted.Count - 1))]
}

function Resolve-RelayProcessId {
    param([uri]$Uri)
    if ($Uri.Host -notin @("127.0.0.1", "localhost", "::1")) { return 0 }
    $port = if ($Uri.IsDefaultPort) { if ($Uri.Scheme -eq "https") { 443 } else { 80 } } else { $Uri.Port }
    try {
        $connection = Get-NetTCPConnection -State Listen -LocalPort $port -ErrorAction Stop | Select-Object -First 1
        return [int]$connection.OwningProcess
    } catch {
        return 0
    }
}

function Get-ClientCounterTotal {
    param($Sources, [string]$Property)
    $total = 0L
    foreach ($source in @($Sources)) {
        foreach ($client in @($source.downstream.clients)) {
            $value = $client.$Property
            if ($null -ne $value) { $total += [long]$value }
        }
    }
    return $total
}

$relayUri = [uri]$RelayUrl
$statusUrl = "{0}://{1}/api/v1/status" -f $relayUri.Scheme, $relayUri.Authority
if ($MinStreamingSources -lt 0) { $MinStreamingSources = $ExpectedSources }
if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    $stamp = Get-Date -Format "yyyyMMdd-HHmmss"
    $OutputPath = Join-Path $PSScriptRoot ".artifacts\relay-scale\$stamp-$ExpectedSources-sources"
}
$OutputPath = [System.IO.Path]::GetFullPath($OutputPath)
New-Item -ItemType Directory -Force -Path $OutputPath | Out-Null

if ($ProcessId -le 0) { $ProcessId = Resolve-RelayProcessId $relayUri }
$processAvailable = $ProcessId -gt 0
$previousCpuMs = $null
$previousProcessSampleAt = $null
$samples = [System.Collections.Generic.List[object]]::new()
$startedAt = Get-Date
$deadline = $startedAt.AddSeconds($DurationSeconds)

Write-Host "Relay scale measurement: $statusUrl"
Write-Host "Expected sources: $ExpectedSources / minimum streaming: $MinStreamingSources / duration: ${DurationSeconds}s"
Write-Host "Relay PID: $(if ($processAvailable) { $ProcessId } else { 'not detected (CPU and memory checks skipped)' })"

do {
    $sampledAt = Get-Date
    $snapshot = Invoke-RestMethod -Uri $statusUrl -Method Get -TimeoutSec 5
    $sources = @($snapshot.sources)
    $streamingSources = @($sources | Where-Object { $_.state -eq "STREAMING" })
    $rtpAges = @($streamingSources | ForEach-Object { if ($null -ne $_.upstream.lastRtpAgeMs) { [double]$_.upstream.lastRtpAgeMs } })
    $ingressRates = @($streamingSources | ForEach-Object { [double]$_.upstream.ingressAccessUnitFps })

    $cpuPercent = $null
    $workingSetMB = $null
    if ($processAvailable) {
        try {
            $process = Get-Process -Id $ProcessId -ErrorAction Stop
            $cpuMs = $process.TotalProcessorTime.TotalMilliseconds
            $workingSetMB = [Math]::Round($process.WorkingSet64 / 1MB, 2)
            if ($null -ne $previousCpuMs) {
                $elapsedMs = ($sampledAt - $previousProcessSampleAt).TotalMilliseconds
                if ($elapsedMs -gt 0) {
                    $cpuPercent = [Math]::Round((($cpuMs - $previousCpuMs) / $elapsedMs) * 100 / [Environment]::ProcessorCount, 2)
                }
            }
            $previousCpuMs = $cpuMs
            $previousProcessSampleAt = $sampledAt
        } catch {
            $processAvailable = $false
            Write-Warning "Relay process $ProcessId is no longer available; CPU and memory checks will be skipped."
        }
    }

    $samples.Add([pscustomobject]@{
        Timestamp = $sampledAt.ToUniversalTime().ToString("o")
        ElapsedSeconds = [Math]::Round(($sampledAt - $startedAt).TotalSeconds, 2)
        SourceCount = $sources.Count
        StreamingCount = $streamingSources.Count
        MaxRtpAgeMs = if ($rtpAges.Count) { [Math]::Round(($rtpAges | Measure-Object -Maximum).Maximum, 2) } else { $null }
        MinIngressFps = if ($ingressRates.Count) { [Math]::Round(($ingressRates | Measure-Object -Minimum).Minimum, 2) } else { $null }
        RaceSubscribers = [int]$snapshot.raceStream.subscribers
        RaceWriteErrors = [long]$snapshot.raceStream.writeErrors
        RaceQueueReplacements = [long]$snapshot.raceStream.queueReplacements
        TelemetryDropped = Get-ClientCounterTotal $sources "telemetryDropped"
        TelemetrySendErrors = Get-ClientCounterTotal $sources "telemetrySendErrors"
        CpuPercent = $cpuPercent
        WorkingSetMB = $workingSetMB
    })

    if ((Get-Date) -lt $deadline) { Start-Sleep -Seconds $SampleIntervalSeconds }
} while ((Get-Date) -lt $deadline)

$samples | Export-Csv -NoTypeInformation -Encoding UTF8 -Path (Join-Path $OutputPath "samples.csv")
$measured = @($samples | Where-Object { $_.ElapsedSeconds -ge $WarmupSeconds })
if ($measured.Count -eq 0) { $measured = @($samples) }
$failures = [System.Collections.Generic.List[string]]::new()

$minimumSourceCount = [int](($measured.SourceCount | Measure-Object -Minimum).Minimum)
$minimumStreamingCount = [int](($measured.StreamingCount | Measure-Object -Minimum).Minimum)
if ($minimumSourceCount -lt $ExpectedSources) { $failures.Add("source count fell to $minimumSourceCount (expected $ExpectedSources)") }
if ($minimumStreamingCount -lt $MinStreamingSources) { $failures.Add("streaming count fell to $minimumStreamingCount (minimum $MinStreamingSources)") }

$cpuValues = @($measured | Where-Object { $null -ne $_.CpuPercent } | ForEach-Object { [double]$_.CpuPercent })
$memoryValues = @($measured | Where-Object { $null -ne $_.WorkingSetMB } | ForEach-Object { [double]$_.WorkingSetMB })
$p95Cpu = Get-Percentile $cpuValues 95
$memoryGrowth = if ($memoryValues.Count) { [Math]::Round((($memoryValues | Measure-Object -Maximum).Maximum - ($memoryValues | Measure-Object -Minimum).Minimum), 2) } else { $null }
if ($null -ne $p95Cpu -and $p95Cpu -gt $MaxCpuPercent) { $failures.Add("CPU p95 is $p95Cpu% (maximum $MaxCpuPercent%)") }
if ($null -ne $memoryGrowth -and $memoryGrowth -gt $MaxMemoryGrowthMB) { $failures.Add("working-set growth is ${memoryGrowth}MB (maximum ${MaxMemoryGrowthMB}MB)") }

$maxObservedRtpAge = ($measured | Where-Object { $null -ne $_.MaxRtpAgeMs } | ForEach-Object { [double]$_.MaxRtpAgeMs } | Measure-Object -Maximum).Maximum
$minObservedIngressFps = ($measured | Where-Object { $null -ne $_.MinIngressFps } | ForEach-Object { [double]$_.MinIngressFps } | Measure-Object -Minimum).Minimum
if ($null -ne $maxObservedRtpAge -and $maxObservedRtpAge -gt $MaxRtpAgeMs) { $failures.Add("maximum RTP age is ${maxObservedRtpAge}ms (maximum ${MaxRtpAgeMs}ms)") }
if ($null -ne $minObservedIngressFps -and $minObservedIngressFps -lt $MinIngressFps) { $failures.Add("minimum ingress FPS is $minObservedIngressFps (minimum $MinIngressFps)") }

$raceErrorIncrease = [long]$measured[-1].RaceWriteErrors - [long]$measured[0].RaceWriteErrors
$telemetryDropIncrease = [long]$measured[-1].TelemetryDropped - [long]$measured[0].TelemetryDropped
if ($raceErrorIncrease -gt $MaxRaceWriteErrorIncrease) { $failures.Add("Race WS write errors increased by $raceErrorIncrease (maximum $MaxRaceWriteErrorIncrease)") }
if ($telemetryDropIncrease -gt $MaxTelemetryDropIncrease) { $failures.Add("Telemetry drops increased by $telemetryDropIncrease (maximum $MaxTelemetryDropIncrease)") }

$summary = [ordered]@{
    schemaVersion = 1
    passed = $failures.Count -eq 0
    startedAt = $startedAt.ToUniversalTime().ToString("o")
    durationSeconds = $DurationSeconds
    warmupSeconds = $WarmupSeconds
    relayUrl = $RelayUrl
    processId = if ($ProcessId -gt 0) { $ProcessId } else { $null }
    thresholds = [ordered]@{
        expectedSources = $ExpectedSources
        minimumStreamingSources = $MinStreamingSources
        maximumCpuPercentP95 = $MaxCpuPercent
        maximumMemoryGrowthMB = $MaxMemoryGrowthMB
        maximumRtpAgeMs = $MaxRtpAgeMs
        minimumIngressFps = $MinIngressFps
        maximumRaceWriteErrorIncrease = $MaxRaceWriteErrorIncrease
        maximumTelemetryDropIncrease = $MaxTelemetryDropIncrease
    }
    observed = [ordered]@{
        samples = $measured.Count
        minimumSourceCount = $minimumSourceCount
        minimumStreamingCount = $minimumStreamingCount
        cpuPercentP95 = $p95Cpu
        memoryGrowthMB = $memoryGrowth
        maximumRtpAgeMs = $maxObservedRtpAge
        minimumIngressFps = $minObservedIngressFps
        raceWriteErrorIncrease = $raceErrorIncrease
        telemetryDropIncrease = $telemetryDropIncrease
    }
    failures = @($failures)
}
$summary | ConvertTo-Json -Depth 8 | Set-Content -Encoding UTF8 -Path (Join-Path $OutputPath "summary.json")

Write-Host "Result: $(if ($summary.passed) { 'PASS' } else { 'FAIL' })"
Write-Host "Artifacts: $OutputPath"
foreach ($failure in $failures) { Write-Warning $failure }
if (-not $summary.passed) { exit 1 }
