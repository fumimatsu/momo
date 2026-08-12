[CmdletBinding()]
param(
    [ValidateNotNullOrEmpty()][int[]]$SourceCounts = @(4, 8, 12, 16, 24, 32),
    [ValidateRange(5, 86400)][int]$DurationSeconds = 60,
    [ValidateRange(0, 3600)][int]$WarmupSeconds = 10,
    [ValidateRange(1, 120)][int]$Fps = 30,
    [ValidateRange(1, 128)][int]$PacketsPerFrame = 8,
    [ValidateRange(64, 1400)][int]$PayloadBytes = 1200,
    [ValidateRange(0, 120)][int]$TelemetryHz = 15,
    [ValidateRange(0, 8)][int]$ObserversPerSource = 0,
    [string]$PilotSource = "",
    [string]$RecoverySource = "",
    [ValidateRange(5, 120)][int]$RecoveryTimeoutSeconds = 30,
    [ValidateRange(1, 65535)][int]$SimulatorPort = 18080,
    [ValidateRange(1, 65535)][int]$RelayPort = 18090,
    [ValidateRange(1, 65535)][int]$LoadClientPort = 18100,
    [ValidateRange(1, 300)][int]$StartupTimeoutSeconds = 60,
    [string]$GoExecutable = "",
    [string]$OutputPath = ""
)

$ErrorActionPreference = "Stop"
$repoRoot = Split-Path -Parent $PSScriptRoot
$relayDir = Join-Path $PSScriptRoot "momo-relay"
$simDir = Join-Path $PSScriptRoot "momo-relay-sim"
$loadDir = Join-Path $PSScriptRoot "momo-relay-load"
$artifactRoot = if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    Join-Path $PSScriptRoot (".artifacts\relay-scale-matrix\" + (Get-Date -Format "yyyyMMdd-HHmmss"))
} else {
    [System.IO.Path]::GetFullPath($OutputPath)
}
New-Item -ItemType Directory -Force -Path $artifactRoot | Out-Null

if ([string]::IsNullOrWhiteSpace($GoExecutable)) {
    $candidates = @(@(
        $env:MOMO_GO_EXE,
        "D:\app\go1.26.5\go\bin\go.exe",
        (Join-Path $relayDir ".toolchain\go\bin\go.exe"),
        $(if (Get-Command go -ErrorAction SilentlyContinue) { (Get-Command go).Source } else { $null })
    ) | Where-Object { -not [string]::IsNullOrWhiteSpace($_) -and (Test-Path -LiteralPath $_) })
    if ($candidates.Count -gt 0) { $GoExecutable = $candidates[0] }
}
if ([string]::IsNullOrWhiteSpace($GoExecutable) -or -not (Test-Path -LiteralPath $GoExecutable)) {
    throw "Go 1.26 executable was not found. Set -GoExecutable or MOMO_GO_EXE."
}

$goVersion = & $GoExecutable version
if ($LASTEXITCODE -ne 0 -or $goVersion -notmatch 'go1\.26') {
    throw "Go 1.26 is required by tools/momo-relay/go.mod; found: $goVersion"
}
& (Join-Path $PSScriptRoot 'Get-ScaleTestEnvironment.ps1') `
    -GoExecutable $GoExecutable -OutputPath (Join-Path $artifactRoot 'environment.json') | Out-Null

$binDir = Join-Path $artifactRoot "bin"
New-Item -ItemType Directory -Force -Path $binDir | Out-Null
$relayExe = Join-Path $binDir "momo-relay.exe"
$simExe = Join-Path $binDir "momo-relay-sim.exe"
$loadExe = Join-Path $binDir "momo-relay-load.exe"

Push-Location $relayDir
try { & $GoExecutable build -o $relayExe .; if ($LASTEXITCODE -ne 0) { throw "Relay build failed" } }
finally { Pop-Location }
if ($ObserversPerSource -gt 0 -or -not [string]::IsNullOrWhiteSpace($PilotSource)) {
    Push-Location $loadDir
    try { & $GoExecutable build -o $loadExe .; if ($LASTEXITCODE -ne 0) { throw "Observer load client build failed" } }
    finally { Pop-Location }
}
Push-Location $simDir
try {
    & $GoExecutable build -o $simExe .
    if ($LASTEXITCODE -ne 0) { throw "Simulator build failed" }
}
finally { Pop-Location }

$simStdout = Join-Path $artifactRoot "simulator.stdout.log"
$simStderr = Join-Path $artifactRoot "simulator.stderr.log"
$simulator = Start-Process -FilePath $simExe -ArgumentList @(
    '-listen', "127.0.0.1:$SimulatorPort",
    '-fps', $Fps,
    '-packets-per-frame', $PacketsPerFrame,
    '-payload-bytes', $PayloadBytes,
    '-telemetry-hz', $TelemetryHz
) -RedirectStandardOutput $simStdout -RedirectStandardError $simStderr -WindowStyle Hidden -PassThru

$matrix = [System.Collections.Generic.List[object]]::new()
try {
    $deadline = (Get-Date).AddSeconds($StartupTimeoutSeconds)
    do {
        try { Invoke-RestMethod -Uri "http://127.0.0.1:$SimulatorPort/healthz" -TimeoutSec 2 | Out-Null; break }
        catch { Start-Sleep -Milliseconds 250 }
    } while ((Get-Date) -lt $deadline)
    if ((Get-Date) -ge $deadline) { throw "Simulator did not become ready" }

    foreach ($sourceCount in $SourceCounts) {
        if ($sourceCount -lt 1 -or $sourceCount -gt 32) { throw "Source count $sourceCount is outside 1..32" }
        $caseDir = Join-Path $artifactRoot ("{0:D2}-sources" -f $sourceCount)
        New-Item -ItemType Directory -Force -Path $caseDir | Out-Null
        $configPath = Join-Path $caseDir "relay-config.json"
        $sources = for ($i = 1; $i -le $sourceCount; $i++) {
            $id = "sim-{0:D2}" -f $i
            [ordered]@{ id = $id; url = "ws://127.0.0.1:$SimulatorPort/ws/$id"; raceCarId = "SIM-{0:D2}" -f $i }
        }
        $configJson = [ordered]@{ version = 1; sources = @($sources) } | ConvertTo-Json -Depth 5
        [System.IO.File]::WriteAllText($configPath, $configJson, [System.Text.UTF8Encoding]::new($false))

        $relayStdout = Join-Path $caseDir "relay.stdout.log"
        $relayStderr = Join-Path $caseDir "relay.stderr.log"
        $relay = Start-Process -FilePath $relayExe -ArgumentList @(
            '-config', $configPath,
            '-listen', "127.0.0.1:$RelayPort",
            '-health-recovery-mode', 'disabled',
            '-rtp-stall-timeout', '10s',
            '-upstream-start-timeout', '30s'
        ) -RedirectStandardOutput $relayStdout -RedirectStandardError $relayStderr -WindowStyle Hidden -PassThru
        $loadClient = $null
        $recoveryResult = $null
        try {
            $statusUrl = "http://127.0.0.1:$RelayPort/api/v1/status"
            $deadline = (Get-Date).AddSeconds($StartupTimeoutSeconds)
            $ready = $false
            do {
                try {
                    $status = Invoke-RestMethod -Uri $statusUrl -TimeoutSec 3
                    $streaming = @($status.sources | Where-Object { $_.state -eq 'STREAMING' }).Count
                    if (@($status.sources).Count -eq $sourceCount -and $streaming -eq $sourceCount) { $ready = $true; break }
                } catch {}
                Start-Sleep -Milliseconds 500
            } while ((Get-Date) -lt $deadline)
            if (-not $ready) { throw "Relay did not reach $sourceCount/$sourceCount streaming sources" }

            if ($ObserversPerSource -gt 0 -or -not [string]::IsNullOrWhiteSpace($PilotSource)) {
                if (-not [string]::IsNullOrWhiteSpace($PilotSource) -and $PilotSource -notin @($sources.id)) {
                    throw "Pilot source is not present in this case: $PilotSource"
                }
                $loadStdout = Join-Path $caseDir "observer-load.stdout.log"
                $loadStderr = Join-Path $caseDir "observer-load.stderr.log"
                $loadArguments = @(
                    '-relay-url', "http://127.0.0.1:$RelayPort",
                    '-source-count', $sourceCount,
                    '-observers-per-source', $ObserversPerSource,
                    '-listen', "127.0.0.1:$LoadClientPort"
                )
                if (-not [string]::IsNullOrWhiteSpace($PilotSource)) {
                    $loadArguments += @('-pilot-source', $PilotSource)
                }
                $loadClient = Start-Process -FilePath $loadExe -ArgumentList $loadArguments `
                    -RedirectStandardOutput $loadStdout -RedirectStandardError $loadStderr -WindowStyle Hidden -PassThru
                $expectedClients = $sourceCount * $ObserversPerSource + $(if ([string]::IsNullOrWhiteSpace($PilotSource)) { 0 } else { 1 })
                $deadline = (Get-Date).AddSeconds($StartupTimeoutSeconds)
                $clientsReady = $false
                do {
                    try {
                        $loadStatus = Invoke-RestMethod -Uri "http://127.0.0.1:$LoadClientPort/api/v1/status" -TimeoutSec 3
                        if ($loadStatus.connectedCount -eq $expectedClients) { $clientsReady = $true; break }
                    } catch {}
                    Start-Sleep -Milliseconds 500
                } while ((Get-Date) -lt $deadline)
                if (-not $clientsReady) { throw "Observer load did not reach $expectedClients/$expectedClients connected clients" }
            }

            if (-not [string]::IsNullOrWhiteSpace($RecoverySource)) {
                if ($ObserversPerSource -eq 0) { throw "RecoverySource requires at least one Observer per source" }
                if ($RecoverySource -notin @($sources.id)) { throw "Recovery source is not present in this case: $RecoverySource" }
                $loadStatusBefore = Invoke-RestMethod -Uri "http://127.0.0.1:$LoadClientPort/api/v1/status" -TimeoutSec 5
                $targetBefore = [long](@($loadStatusBefore.clients | Where-Object { $_.id -like "$RecoverySource/*" } | Measure-Object rtpFrames -Sum).Sum)
                $otherClients = @($loadStatusBefore.clients | Where-Object { $_.id -notlike "$RecoverySource/*" })
                $otherBefore = [long](@($otherClients | Measure-Object rtpFrames -Sum).Sum)
                $recoveryStarted = Get-Date
                $disconnectResult = Invoke-RestMethod -Method Post `
                    -Uri "http://127.0.0.1:$SimulatorPort/api/v1/disconnect?source=$([uri]::EscapeDataString($RecoverySource))" `
                    -TimeoutSec 5
                if ([int]$disconnectResult.closed -ne 1) { throw "Simulator did not disconnect exactly one $RecoverySource session" }
                $observedDown = $false
                $recovered = $false
                $deadline = (Get-Date).AddSeconds($RecoveryTimeoutSeconds)
                do {
                    $relayStatus = Invoke-RestMethod -Uri $statusUrl -TimeoutSec 3
                    $targetStatus = @($relayStatus.sources | Where-Object id -eq $RecoverySource)[0]
                    if ($targetStatus.state -ne 'STREAMING') { $observedDown = $true }
                    $currentLoad = Invoke-RestMethod -Uri "http://127.0.0.1:$LoadClientPort/api/v1/status" -TimeoutSec 3
                    $targetFrames = [long](@($currentLoad.clients | Where-Object { $_.id -like "$RecoverySource/*" } | Measure-Object rtpFrames -Sum).Sum)
                    if ($observedDown -and $targetStatus.state -eq 'STREAMING' -and $targetFrames -gt $targetBefore) {
                        $recovered = $true
                        break
                    }
                    Start-Sleep -Milliseconds 100
                } while ((Get-Date) -lt $deadline)
                $otherAfter = [long](@($currentLoad.clients | Where-Object { $_.id -notlike "$RecoverySource/*" } | Measure-Object rtpFrames -Sum).Sum)
                $unaffectedPassed = $otherClients.Count -eq 0 -or $otherAfter -gt $otherBefore
                $recoveryResult = [ordered]@{
                    source = $RecoverySource
                    passed = $recovered -and $unaffectedPassed
                    observedDown = $observedDown
                    recoveryMs = [Math]::Round(((Get-Date) - $recoveryStarted).TotalMilliseconds)
                    targetFrameGrowth = $targetFrames - $targetBefore
                    unaffectedFrameGrowth = $otherAfter - $otherBefore
                }
                if (-not $recoveryResult.passed) { throw "Relay recovery test failed for $RecoverySource" }
            }

            $measurementDir = Join-Path $caseDir "measurement"
            & (Join-Path $PSScriptRoot "Measure-RelayScale.ps1") `
                -RelayUrl "http://127.0.0.1:$RelayPort" `
                -DurationSeconds $DurationSeconds `
                -WarmupSeconds $WarmupSeconds `
                -ExpectedSources $sourceCount `
                -MinStreamingSources $sourceCount `
                -ProcessId $relay.Id `
                -OutputPath $measurementDir
            $exitCode = $LASTEXITCODE
            $summary = Get-Content (Join-Path $measurementDir "summary.json") -Raw | ConvertFrom-Json
            if ($ObserversPerSource -gt 0 -or -not [string]::IsNullOrWhiteSpace($PilotSource)) {
                $loadStatus = Invoke-RestMethod -Uri "http://127.0.0.1:$LoadClientPort/api/v1/status" -TimeoutSec 5
            }
            $pilotStatus = @($loadStatus.clients | Where-Object role -eq 'pilot')
            $pilotPassed = $pilotStatus.Count -eq 0 -or @($pilotStatus | Where-Object { -not $_.commandOpen -or -not $_.driveOpen -or $_.commandsSent -eq 0 }).Count -eq 0
            $caseFailures = [System.Collections.Generic.List[string]]::new()
            foreach ($failure in @($summary.failures)) { $caseFailures.Add([string]$failure) }
            if (-not $pilotPassed) { $caseFailures.Add("Pilot command/drive channel did not remain active") }
            if ($null -ne $recoveryResult -and -not $recoveryResult.passed) { $caseFailures.Add("Source recovery did not complete") }
            $matrix.Add([pscustomobject]@{
                sourceCount = $sourceCount
                passed = $exitCode -eq 0 -and [bool]$summary.passed -and $pilotPassed -and ($null -eq $recoveryResult -or $recoveryResult.passed)
                cpuPercentP95 = $summary.observed.cpuPercentP95
                memoryGrowthMB = $summary.observed.memoryGrowthMB
                maximumRtpAgeMs = $summary.observed.maximumRtpAgeMs
                minimumIngressFps = $summary.observed.minimumIngressFps
                viewerClients = if ($null -ne $loadClient) { [int]$loadStatus.connectedCount } else { 0 }
                viewerRtpFrames = if ($null -ne $loadClient) { [long]$loadStatus.rtpFrames } else { 0 }
                pilotCommandsSent = if ($null -ne $loadClient) { [long](@($loadStatus.clients | Where-Object role -eq 'pilot' | Measure-Object commandsSent -Sum).Sum) } else { 0 }
                recovery = $recoveryResult
                failures = @($caseFailures)
            })
        }
        finally {
            if ($null -ne $loadClient -and -not $loadClient.HasExited) { Stop-Process -Id $loadClient.Id -Force }
            if ($null -ne $loadClient) { $loadClient.WaitForExit() }
            if (-not $relay.HasExited) { Stop-Process -Id $relay.Id -Force }
            $relay.WaitForExit()
        }
    }
}
finally {
    if ($null -ne $simulator -and -not $simulator.HasExited) { Stop-Process -Id $simulator.Id -Force }
    if ($null -ne $simulator) { $simulator.WaitForExit() }
}

$result = [ordered]@{
    schemaVersion = 1
    measuredAt = (Get-Date).ToUniversalTime().ToString('o')
    host = $env:COMPUTERNAME
    workload = [ordered]@{
        fps = $Fps
        packetsPerFrame = $PacketsPerFrame
        payloadBytes = $PayloadBytes
        telemetryHz = $TelemetryHz
        observersPerSource = $ObserversPerSource
        pilotSource = $PilotSource
        recoverySource = $RecoverySource
        approximateVideoMbpsPerSource = [Math]::Round($Fps * $PacketsPerFrame * $PayloadBytes * 8 / 1000000, 3)
    }
    cases = @($matrix)
}
$result | ConvertTo-Json -Depth 8 | Set-Content -Encoding UTF8 -Path (Join-Path $artifactRoot "matrix-summary.json")
$matrix | Format-Table sourceCount, passed, cpuPercentP95, memoryGrowthMB, maximumRtpAgeMs, minimumIngressFps -AutoSize
Write-Host "Artifacts: $artifactRoot"
if (@($matrix | Where-Object { -not $_.passed }).Count -gt 0) { exit 1 }
