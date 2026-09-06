[CmdletBinding()]
param()

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$testRoot = Join-Path ([IO.Path]::GetTempPath()) ('momo-launch-test-' + [Guid]::NewGuid().ToString('N'))
$fixtureTools = Join-Path $testRoot 'tools'
$fixtureRelay = Join-Path $fixtureTools 'momo-relay'
Microsoft.PowerShell.Management\New-Item -ItemType Directory -Path (Join-Path $fixtureRelay 'web') -Force | Out-Null
Copy-Item -LiteralPath (Join-Path $PSScriptRoot 'start-mads-observer.ps1') -Destination $fixtureTools
foreach ($file in @('relay.go', 'go.mod', 'go.sum', 'web\index.html')) {
    [IO.File]::WriteAllText((Join-Path $fixtureRelay $file), 'synthetic launcher fixture')
}
$relayExecutable = Join-Path $fixtureRelay 'momo-local-relay-device-input-v15.exe'
$observerExecutable = Join-Path $testRoot 'momo.exe'
foreach ($file in @($relayExecutable, $observerExecutable)) {
    [IO.File]::WriteAllText($file, 'Never executed: process launch is mocked')
    [IO.File]::SetLastWriteTimeUtc($file, [DateTime]::UtcNow.AddHours(1))
}

# All process and registry effects are intercepted. The production launcher is
# copied unchanged into a synthetic repository so its filesystem checks run too.
function Get-CimInstance {
    param($ClassName)
    if ($ClassName -ne 'Win32_Process') { throw 'Unexpected CIM query' }
    return $launcherMock.Processes
}
function Stop-Process {
    param([int]$Id, [switch]$Force)
    $launcherMock.Stopped.Add($Id)
}
function Start-Process {
    param($FilePath, $ArgumentList, $WorkingDirectory, $RedirectStandardOutput, $RedirectStandardError, $WindowStyle, $Environment)
    if ($FilePath -notin @($relayExecutable, $observerExecutable)) { throw 'Unexpected process path' }
    $launcherMock.Started.Add([pscustomobject]@{ FilePath = $FilePath; Arguments = @($ArgumentList) })
}
function New-Item {
    param($Path, $ItemType, [switch]$Force)
    if ($Path -like 'HKCU:\*') { $launcherMock.RegistryWrites++; return }
    $resolvedPath = [IO.Path]::GetFullPath($Path)
    if (-not $resolvedPath.StartsWith($testRoot + '\', [StringComparison]::OrdinalIgnoreCase)) {
        throw 'Launcher tried to write outside the synthetic repository'
    }
    Microsoft.PowerShell.Management\New-Item -Path $Path -ItemType $ItemType -Force:$Force
}
function New-ItemProperty {
    param($Path, $Name, $PropertyType, $Value, [switch]$Force)
    if ($Path -notlike 'HKCU:\*') { throw 'Unexpected registry target' }
    $launcherMock.RegistryWrites++
}
function Invoke-FakeMarkerPython {
    $launcherMock.PythonArguments = @($args)
    # Supply a real native exit code to the unmodified wrapper's exit path.
    & $env:ComSpec /c 'exit 0'
}

function Test-ObserverCase {
    param([string]$CaseName, [switch]$SkipObserver, [switch]$RestartObserver, [switch]$ExistingObserver,
        [switch]$StartRelay, [switch]$MissingObserver, [switch]$UnknownOption,
        [int]$ExpectedNativeStarts = 0, [int]$ExpectedStops = 0)
    $processes = @(
        [pscustomobject]@{ Name = 'momo.exe'; ProcessId = 41002; CommandLine = 'momo.exe p2p-marker-recv' }
        [pscustomobject]@{ Name = 'momo.exe'; ProcessId = 41003; CommandLine = 'momo.exe p2p-recv' }
    )
    if ($ExistingObserver) {
        $processes += [pscustomobject]@{ Name = 'momo.exe'; ProcessId = 41001; CommandLine = 'momo.exe p2p-recv-multi' }
    }
    $launcherMock = @{ Processes = $processes; Stopped = [Collections.Generic.List[int]]::new();
        Started = [Collections.Generic.List[object]]::new(); RegistryWrites = 0 }
    $parameters = @{
        SkipRelay = -not $StartRelay; SkipObserver = [bool]$SkipObserver; RestartObserver = [bool]$RestartObserver
        ObserverExecutable = if ($MissingObserver) { Join-Path $testRoot 'missing.exe' } else { $observerExecutable }
        HealthRecoveryMode = 'disabled'; RaceControlUrl = ''; RaceAudioServiceUrl = ''
        RelaySourceRegistryPath = ''; RaceDirectoryRefreshConfig = ''; RaceDirectoryRefreshScript = ''
        TeamObserverDirectoryCache = ''; TeamObserverDirectoryOrganization = ''; TeamObserverDirectoryEvent = ''
        AyameSignalingUrl = ''; AyamePilotRoom113 = ''; AyamePilotRoom114 = ''; AyamePilotRoom115 = ''; AyamePilotRoom116 = ''
        AyameRoomPrefix = ''; TelemetryLogDirectory = ''
    }
    if ($UnknownOption) { $parameters.UnknownLauncherOption = $true }
    $failure = ''
    try { & (Join-Path $fixtureTools 'start-mads-observer.ps1') @parameters 6>$null }
    catch { $failure = $_.Exception.Message }
    if (($UnknownOption -and $failure -notlike '*UnknownLauncherOption*') -or (-not $UnknownOption -and $failure)) {
        throw "${CaseName}: unexpected launcher result: $failure"
    }
    $nativeStarts = @($launcherMock.Started | Where-Object FilePath -eq $observerExecutable)
    $relayStarts = @($launcherMock.Started | Where-Object FilePath -eq $relayExecutable)
    if ($nativeStarts.Count -ne $ExpectedNativeStarts -or $relayStarts.Count -ne [int][bool]$StartRelay -or
        $launcherMock.Stopped.Count -ne $ExpectedStops) { throw "${CaseName}: unexpected process action count" }
    if (@($launcherMock.Stopped | Where-Object { $_ -ne 41001 }).Count -ne 0) { throw "${CaseName}: unrelated process stopped" }
    if (($SkipObserver -or $UnknownOption) -and $launcherMock.RegistryWrites -ne 0) { throw "${CaseName}: unnecessary WER registry write" }
    if ($ExpectedNativeStarts -gt 0 -and $nativeStarts[0].Arguments -notcontains '--shared-frame-name') {
        throw "${CaseName}: ordinary Native Observer startup changed"
    }
    Write-Host "PASS observer launcher: $CaseName"
}

function Test-MarkerCase {
    param([string]$CaseName, [hashtable]$Extra = @{}, [string]$ExpectedWait = '20', [switch]$ExpectFailure)
    $launcherMock = @{ PythonArguments = @() }
    $failure = ''
    try {
        & (Join-Path $PSScriptRoot 'Run-GpuMarkerObserverLumaV2.ps1') -Python Invoke-FakeMarkerPython @Extra
    } catch { $failure = $_.Exception.Message }
    if ($ExpectFailure) {
        if (-not $failure -or $launcherMock.PythonArguments.Count -ne 0) { throw "${CaseName}: invalid arguments reached Python" }
    } else {
        if ($failure) { throw "${CaseName}: unexpected Marker result: $failure" }
        $waitIndex = [Array]::IndexOf($launcherMock.PythonArguments, '--wait-for-mapping-seconds')
        if ($waitIndex -lt 0 -or $launcherMock.PythonArguments[$waitIndex + 1] -cne $ExpectedWait) {
            throw "${CaseName}: mapping wait was not forwarded exactly"
        }
        if ($launcherMock.PythonArguments -notcontains '--input-mapping-name' -or
            $launcherMock.PythonArguments -notcontains '--output-mapping-name') { throw "${CaseName}: mapping identity was lost" }
    }
    Write-Host "PASS Marker launcher: $CaseName"
}

$savedCulture = [Globalization.CultureInfo]::CurrentCulture
try {
    Test-ObserverCase -CaseName 'skip-without-native-binary' -SkipObserver -MissingObserver
    Test-ObserverCase -CaseName 'skip-preserves-managed-observer' -SkipObserver -ExistingObserver
    Test-ObserverCase -CaseName 'skip-and-restart-stops-only-native' -SkipObserver -RestartObserver -ExistingObserver -MissingObserver -ExpectedStops 1
    Test-ObserverCase -CaseName 'skip-and-restart-with-no-native' -SkipObserver -RestartObserver -MissingObserver
    Test-ObserverCase -CaseName 'relay-starts-without-native' -SkipObserver -StartRelay -MissingObserver
    Test-ObserverCase -CaseName 'ordinary-native-start' -ExpectedNativeStarts 1
    Test-ObserverCase -CaseName 'ordinary-native-restart' -RestartObserver -ExistingObserver -ExpectedNativeStarts 1 -ExpectedStops 1
    Test-ObserverCase -CaseName 'unknown-option-fails-before-effects' -UnknownOption
    Test-MarkerCase -CaseName 'default-wait'
    Test-MarkerCase -CaseName 'coordinator-120-seconds' -Extra @{ WaitForMappingSeconds = 120 } -ExpectedWait '120'
    Test-MarkerCase -CaseName 'zero-wait' -Extra @{ WaitForMappingSeconds = 0 } -ExpectedWait '0'
    Test-MarkerCase -CaseName 'maximum-wait' -Extra @{ WaitForMappingSeconds = 300 } -ExpectedWait '300'
    [Globalization.CultureInfo]::CurrentCulture = [Globalization.CultureInfo]::GetCultureInfo('de-DE')
    Test-MarkerCase -CaseName 'fractional-wait-is-invariant' -Extra @{ WaitForMappingSeconds = 120.5 } -ExpectedWait '120.5'
    Test-MarkerCase -CaseName 'negative-wait-is-rejected' -Extra @{ WaitForMappingSeconds = -1 } -ExpectFailure
    Test-MarkerCase -CaseName 'excessive-wait-is-rejected' -Extra @{ WaitForMappingSeconds = 301 } -ExpectFailure
    Test-MarkerCase -CaseName 'unknown-option-is-rejected' -Extra @{ UnknownMarkerOption = 1 } -ExpectFailure
    Write-Host 'Observer launchers: 16 cases passed.' -ForegroundColor Green
} finally {
    [Globalization.CultureInfo]::CurrentCulture = $savedCulture
    $resolvedTestRoot = [IO.Path]::GetFullPath($testRoot)
    $tempPrefix = [IO.Path]::GetFullPath([IO.Path]::GetTempPath()).TrimEnd('\') + '\'
    if (-not $resolvedTestRoot.StartsWith($tempPrefix, [StringComparison]::OrdinalIgnoreCase) -or
        [IO.Path]::GetFileName($resolvedTestRoot) -notmatch '^momo-launch-test-[0-9a-f]{32}$') {
        throw 'Refusing to remove a launcher test directory outside the temporary root'
    }
    if (Test-Path -LiteralPath $resolvedTestRoot) { Remove-Item -LiteralPath $resolvedTestRoot -Recurse -Force }
}
