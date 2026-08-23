[CmdletBinding()]
param(
    [Parameter(Mandatory)]
    [string]$InputPath,
    [ValidateNotNullOrEmpty()]
    [int[]]$SourceCounts = @(1, 5, 8, 12, 20, 32),
    [ValidateRange(1, 60)]
    [int]$FrameRate = 50,
    [ValidateRange(5, 3600)]
    [int]$MeasurementSeconds = 30,
    [ValidateRange(5, 600)]
    [int]$SourceReadyTimeoutSeconds = 120,
    [ValidateSet(50, 40, 33, 25)]
    [int]$InitialDetectionHz = 50,
    [ValidateRange(1, 8)]
    [int]$ConnectParallelism = 4,
    [ValidateRange(250, 5000)]
    [int]$RuntimeSampleIntervalMs = 1000,
    [string]$MomoExecutable = '',
    [string]$Python = '',
    [string]$OutputDirectory = '',
    [switch]$NoAdaptive,
    [switch]$NoRuntimeMetrics,
    [switch]$ContinueOnValidationFailure
)

$ErrorActionPreference = 'Stop'

function Get-Distribution {
    param([object[]]$Values)
    $numbers = @($Values | Where-Object { $_ -ne $null } | ForEach-Object { [double]$_ } | Sort-Object)
    if ($numbers.Count -eq 0) {
        return [ordered]@{ samples = 0; average = 0; p50 = 0; p95 = 0; maximum = 0 }
    }
    function Select-Percentile([double]$Percent) {
        $index = [Math]::Max(0, [Math]::Min($numbers.Count - 1, [Math]::Ceiling($numbers.Count * $Percent / 100.0) - 1))
        return $numbers[$index]
    }
    return [ordered]@{
        samples = $numbers.Count
        average = [Math]::Round(($numbers | Measure-Object -Average).Average, 3)
        p50 = [Math]::Round((Select-Percentile 50), 3)
        p95 = [Math]::Round((Select-Percentile 95), 3)
        maximum = [Math]::Round($numbers[-1], 3)
    }
}

function Get-NvidiaGpuSample {
    param([string]$Executable)
    if ([string]::IsNullOrWhiteSpace($Executable)) { return $null }
    $line = & $Executable `
        '--query-gpu=utilization.gpu,utilization.memory,utilization.encoder,utilization.decoder,memory.used,power.draw,clocks.current.graphics' `
        '--format=csv,noheader,nounits' 2>$null | Select-Object -First 1
    if ([string]::IsNullOrWhiteSpace($line)) { return $null }
    $fields = @($line -split ',' | ForEach-Object { $_.Trim() })
    if ($fields.Count -ne 7) { return $null }
    return [ordered]@{
        gpuPercent = [double]$fields[0]
        memoryControllerPercent = [double]$fields[1]
        encoderPercent = [double]$fields[2]
        decoderPercent = [double]$fields[3]
        memoryUsedMiB = [double]$fields[4]
        powerW = [double]$fields[5]
        graphicsClockMHz = [double]$fields[6]
    }
}

function Get-RuntimeSample {
    param(
        [System.Collections.IDictionary]$Processes,
        [System.Collections.IDictionary]$PreviousCpu,
        [int]$LogicalProcessorCount,
        [string]$NvidiaSmi
    )
    $capturedAt = [DateTimeOffset]::UtcNow
    $childrenByParent = @{}
    $processInfoById = @{}
    foreach ($processRow in @(Get-CimInstance Win32_Process | Select-Object ProcessId, ParentProcessId, CreationDate)) {
        $processInfoById[[int]$processRow.ProcessId] = $processRow
        $parentId = [int]$processRow.ParentProcessId
        if (-not $childrenByParent.ContainsKey($parentId)) {
            $childrenByParent[$parentId] = [System.Collections.Generic.List[int]]::new()
        }
        $childrenByParent[$parentId].Add([int]$processRow.ProcessId)
    }
    $processValues = [ordered]@{}
    foreach ($label in $Processes.Keys) {
        $rootProcessId = [int]$Processes[$label]
        $pendingProcessIds = [System.Collections.Generic.Queue[int]]::new()
        $treeProcessIds = [System.Collections.Generic.List[int]]::new()
        $pendingProcessIds.Enqueue($rootProcessId)
        while ($pendingProcessIds.Count -gt 0) {
            $processId = $pendingProcessIds.Dequeue()
            $treeProcessIds.Add($processId)
            if ($childrenByParent.ContainsKey($processId)) {
                foreach ($childProcessId in $childrenByParent[$processId]) {
                    $parentInfo = $processInfoById[$processId]
                    $childInfo = $processInfoById[$childProcessId]
                    if (
                        $parentInfo -and $childInfo -and
                        $childInfo.CreationDate -lt $parentInfo.CreationDate
                    ) {
                        continue
                    }
                    $pendingProcessIds.Enqueue($childProcessId)
                }
            }
        }
        $processTree = @($treeProcessIds | ForEach-Object {
            Get-Process -Id $_ -ErrorAction SilentlyContinue
        } | Where-Object { $_ -ne $null })
        if ($processTree.Count -eq 0) {
            $processValues[$label] = $null
            continue
        }
        $cpuSeconds = [double](($processTree | Measure-Object -Property CPU -Sum).Sum)
        $cpuCorePercent = $null
        $cpuHostPercent = $null
        if ($PreviousCpu.ContainsKey($label)) {
            $previous = $PreviousCpu[$label]
            $elapsedSeconds = ($capturedAt - $previous.at).TotalSeconds
            if ($elapsedSeconds -gt 0) {
                $cpuCorePercent = [Math]::Max(0, ($cpuSeconds - $previous.cpuSeconds) / $elapsedSeconds * 100.0)
                $cpuHostPercent = $cpuCorePercent / [Math]::Max(1, $LogicalProcessorCount)
            }
        }
        $PreviousCpu[$label] = [pscustomobject]@{ at = $capturedAt; cpuSeconds = $cpuSeconds }
        $processValues[$label] = [ordered]@{
            rootPid = $rootProcessId
            pids = @($processTree.Id)
            cpuCorePercent = if ($cpuCorePercent -ne $null) { [Math]::Round($cpuCorePercent, 3) } else { $null }
            cpuHostPercent = if ($cpuHostPercent -ne $null) { [Math]::Round($cpuHostPercent, 3) } else { $null }
            workingSetMiB = [Math]::Round((($processTree | Measure-Object -Property WorkingSet64 -Sum).Sum) / 1MB, 3)
            privateMemoryMiB = [Math]::Round((($processTree | Measure-Object -Property PrivateMemorySize64 -Sum).Sum) / 1MB, 3)
            threadCount = [int](($processTree | ForEach-Object { $_.Threads.Count } | Measure-Object -Sum).Sum)
        }
    }
    return [ordered]@{
        at = $capturedAt.ToString('o')
        processes = $processValues
        gpu = Get-NvidiaGpuSample -Executable $NvidiaSmi
    }
}

function Get-RuntimeSummary {
    param([object[]]$Samples, [string[]]$ProcessLabels)
    $processSummary = [ordered]@{}
    foreach ($label in $ProcessLabels) {
        $values = @($Samples | ForEach-Object { $_.processes[$label] } | Where-Object { $_ -ne $null })
        $processSummary[$label] = [ordered]@{
            cpuCorePercent = Get-Distribution @($values | ForEach-Object { $_.cpuCorePercent })
            cpuHostPercent = Get-Distribution @($values | ForEach-Object { $_.cpuHostPercent })
            workingSetMiB = Get-Distribution @($values | ForEach-Object { $_.workingSetMiB })
            privateMemoryMiB = Get-Distribution @($values | ForEach-Object { $_.privateMemoryMiB })
            threadCount = Get-Distribution @($values | ForEach-Object { $_.threadCount })
        }
    }
    $gpuValues = @($Samples | ForEach-Object { $_.gpu } | Where-Object { $_ -ne $null })
    return [ordered]@{
        sampleCount = $Samples.Count
        processes = $processSummary
        gpu = [ordered]@{
            gpuPercent = Get-Distribution @($gpuValues | ForEach-Object { $_.gpuPercent })
            memoryControllerPercent = Get-Distribution @($gpuValues | ForEach-Object { $_.memoryControllerPercent })
            encoderPercent = Get-Distribution @($gpuValues | ForEach-Object { $_.encoderPercent })
            decoderPercent = Get-Distribution @($gpuValues | ForEach-Object { $_.decoderPercent })
            memoryUsedMiB = Get-Distribution @($gpuValues | ForEach-Object { $_.memoryUsedMiB })
            powerW = Get-Distribution @($gpuValues | ForEach-Object { $_.powerW })
            graphicsClockMHz = Get-Distribution @($gpuValues | ForEach-Object { $_.graphicsClockMHz })
        }
    }
}

$repoRoot = Split-Path -Parent $PSScriptRoot
$artifactRoot = Join-Path $PSScriptRoot '.artifacts\dynamic-marker'
if ([string]::IsNullOrWhiteSpace($MomoExecutable)) {
    $MomoExecutable = Join-Path $repoRoot '_build\windows_x86_64\release\momo\Release\momo.exe'
}
if ([string]::IsNullOrWhiteSpace($Python)) {
    $Python = Join-Path $PSScriptRoot '.artifacts\aruco-venv\Scripts\python.exe'
}
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
    $stamp = [DateTime]::UtcNow.ToString('yyyyMMddTHHmmssZ')
    $OutputDirectory = Join-Path $artifactRoot "capacity-$stamp"
}

foreach ($path in @($InputPath, $MomoExecutable, $Python)) {
    if (-not (Test-Path -LiteralPath $path -PathType Leaf)) {
        throw "Required file was not found: $path"
    }
}
foreach ($count in $SourceCounts) {
    if ($count -lt 1 -or $count -gt 32) {
        throw "SourceCounts must contain only values in 1..32: $count"
    }
}

New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
$receiverOutput = Join-Path $OutputDirectory 'marker-receiver.stdout.log'
$receiverError = Join-Path $OutputDirectory 'marker-receiver.stderr.log'
$summaryPath = Join-Path $OutputDirectory 'summary.json'
$virtualRuntimePath = Join-Path $PSScriptRoot '.artifacts\virtual-five-car\runtime.json'
$manifestUrl = 'http://127.0.0.1:18190/api/v1/marker-sources?selection=all'
$results = [System.Collections.Generic.List[object]]::new()
$receiver = $null
$observer = $null
$failure = $null
$logicalProcessorCount = [Environment]::ProcessorCount
$nvidiaSmi = ''
if (-not $NoRuntimeMetrics) {
    $systemNvidiaSmi = Join-Path $env:WINDIR 'System32\nvidia-smi.exe'
    if (Test-Path -LiteralPath $systemNvidiaSmi -PathType Leaf) {
        $nvidiaSmi = $systemNvidiaSmi
    }
    else {
        $nvidiaCommand = Get-Command nvidia-smi.exe -ErrorAction SilentlyContinue
        if ($nvidiaCommand) {
            $nvidiaSmi = $nvidiaCommand.Source
        }
    }
}

try {
    $receiverArguments = @(
        '--no-google-stun',
        'p2p-marker-recv',
        '--manifest-url', $manifestUrl,
        '--connect-parallelism', "$ConnectParallelism",
        '--connect-timeout-ms', '20000'
    )
    $receiver = Start-Process -FilePath $MomoExecutable -WindowStyle Hidden -PassThru `
        -RedirectStandardOutput $receiverOutput -RedirectStandardError $receiverError `
        -ArgumentList $receiverArguments

    foreach ($count in $SourceCounts) {
        & (Join-Path $PSScriptRoot 'Start-VirtualFiveCarDemo.ps1') `
            -InputPath $InputPath -CarCount $count -FrameRate $FrameRate -NoOpen
        if ($LASTEXITCODE -ne 0) {
            throw "Virtual source startup failed for $count sources"
        }

        & $Python (Join-Path $PSScriptRoot 'Wait-MarkerLumaV2Ready.py') `
            --required-source-count $count `
            --timeout-seconds $SourceReadyTimeoutSeconds
        if ($LASTEXITCODE -ne 0) {
            throw "MLY2 warm-up failed for $count sources"
        }

        $reportPath = Join-Path $OutputDirectory ("sources-{0:d2}.json" -f $count)
        $observerArguments = @(
            (Join-Path $PSScriptRoot 'Run-GpuMarkerObserverLumaV2.py'),
            '--required-source-count', "$count",
            '--duration-seconds', "$MeasurementSeconds",
            '--initial-detection-hz', "$InitialDetectionHz",
            '--output', $reportPath
        )
        if ($NoAdaptive) {
            $observerArguments += '--no-adaptive'
        }
        $observerOutput = Join-Path $OutputDirectory ("sources-{0:d2}.stdout.log" -f $count)
        $observerError = Join-Path $OutputDirectory ("sources-{0:d2}.stderr.log" -f $count)
        $runtimeMetricsPath = Join-Path $OutputDirectory ("sources-{0:d2}-runtime.json" -f $count)
        $observer = Start-Process -FilePath $Python -WindowStyle Hidden -PassThru `
            -RedirectStandardOutput $observerOutput -RedirectStandardError $observerError `
            -ArgumentList $observerArguments

        $runtimeSamples = [System.Collections.Generic.List[object]]::new()
        if (-not $NoRuntimeMetrics) {
            if (-not (Test-Path -LiteralPath $virtualRuntimePath -PathType Leaf)) {
                throw "Virtual runtime metadata was not found: $virtualRuntimePath"
            }
            $virtualRuntime = Get-Content -Raw -LiteralPath $virtualRuntimePath | ConvertFrom-Json
            $trackedProcesses = [ordered]@{
                markerReceiver = $receiver.Id
                virtualSource = [int]$virtualRuntime.virtualSourcePid
                relay = [int]$virtualRuntime.relayPid
                gpuObserver = $observer.Id
            }
            $previousCpu = @{}
            while (-not $observer.HasExited) {
                $runtimeSamples.Add((Get-RuntimeSample `
                    -Processes $trackedProcesses `
                    -PreviousCpu $previousCpu `
                    -LogicalProcessorCount $logicalProcessorCount `
                    -NvidiaSmi $nvidiaSmi))
                [void]$observer.WaitForExit($RuntimeSampleIntervalMs)
                $observer.Refresh()
            }
            $runtimeSummary = Get-RuntimeSummary `
                -Samples $runtimeSamples.ToArray() `
                -ProcessLabels @($trackedProcesses.Keys)
            [ordered]@{
                schemaVersion = 1
                measuredAt = [DateTimeOffset]::UtcNow.ToString('o')
                sampleIntervalMs = $RuntimeSampleIntervalMs
                logicalProcessorCount = $logicalProcessorCount
                nvidiaSmiAvailable = -not [string]::IsNullOrWhiteSpace($nvidiaSmi)
                trackedProcesses = $trackedProcesses
                summary = $runtimeSummary
                samples = $runtimeSamples
            } | ConvertTo-Json -Depth 10 | Set-Content -LiteralPath $runtimeMetricsPath -Encoding utf8
        }
        else {
            [void]$observer.WaitForExit()
            $runtimeSummary = $null
            $runtimeMetricsPath = $null
        }
        $observerExitCode = $observer.ExitCode
        if (Test-Path -LiteralPath $reportPath -PathType Leaf) {
            $report = Get-Content -Raw -LiteralPath $reportPath | ConvertFrom-Json
            $results.Add([pscustomobject]@{
                sourceCount = $count
                passed = [bool]$report.passed
                detectionEpochRateHz = $report.detectionEpochRateHz
                publicationRateHz = $report.publicationRateHz
                effectiveDetectionHz = $report.effectiveDetectionHz
                eligibleSourceRatio = $report.eligibleSourceRatio
                batchesPerEpoch = $report.batchesPerEpoch
                interBatchDetectionSkewMs = $report.interBatchDetectionSkewMs
                cycleTimeMs = $report.cycleTimeMs
                processingTimeMs = $report.processingTimeMs
                profileHistory = $report.profileHistory
                reportPath = $reportPath
                runtimeMetricsPath = $runtimeMetricsPath
                runtimeSummary = $runtimeSummary
            })
        }
        if ($observerExitCode -ne 0) {
            $message = "GPU marker validation failed for $count sources"
            if ($ContinueOnValidationFailure) {
                Write-Warning "$message; continuing with the remaining source counts"
            }
            else {
                throw $message
            }
        }
    }
}
catch {
    $failure = $_
}
finally {
    if ($observer -and -not $observer.HasExited) {
        Stop-Process -Id $observer.Id -Force -ErrorAction SilentlyContinue
        [void]$observer.WaitForExit(5000)
    }
    if ($receiver -and -not $receiver.HasExited) {
        Stop-Process -Id $receiver.Id -Force -ErrorAction SilentlyContinue
        [void]$receiver.WaitForExit(5000)
    }
    & (Join-Path $PSScriptRoot 'Stop-VirtualFiveCarDemo.ps1')
    [pscustomobject]@{
        schemaVersion = 1
        measuredAt = [DateTime]::UtcNow.ToString('o')
        frameRate = $FrameRate
        measurementSeconds = $MeasurementSeconds
        initialDetectionHz = $InitialDetectionHz
        adaptive = -not $NoAdaptive
        connectParallelism = $ConnectParallelism
        runtimeMetrics = -not $NoRuntimeMetrics
        runtimeSampleIntervalMs = $RuntimeSampleIntervalMs
        continuedAfterValidationFailure = [bool]$ContinueOnValidationFailure
        results = $results
        error = if ($failure) { $failure.Exception.Message } else { $null }
    } | ConvertTo-Json -Depth 8 | Set-Content -LiteralPath $summaryPath -Encoding utf8
}

Write-Host "Capacity summary: $summaryPath"
if ($failure) {
    throw $failure
}
