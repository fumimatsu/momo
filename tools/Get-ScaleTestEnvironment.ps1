[CmdletBinding()]
param(
    [string]$PythonExecutable = "",
    [string]$FfmpegExecutable = "",
    [string]$GoExecutable = "",
    [string]$OutputPath = ""
)

$ErrorActionPreference = "Stop"
if ([string]::IsNullOrWhiteSpace($PythonExecutable)) {
    $PythonExecutable = Join-Path $PSScriptRoot ".artifacts\aruco-venv\Scripts\python.exe"
}
if ([string]::IsNullOrWhiteSpace($FfmpegExecutable) -and (Test-Path -LiteralPath $PythonExecutable)) {
    $FfmpegExecutable = & $PythonExecutable -c "import imageio_ffmpeg; print(imageio_ffmpeg.get_ffmpeg_exe())"
}
$pythonVersion = if (Test-Path -LiteralPath $PythonExecutable) { (& $PythonExecutable --version 2>&1) -join ' ' } else { $null }
$opencvVersion = if (Test-Path -LiteralPath $PythonExecutable) { (& $PythonExecutable -c "import cv2; print(cv2.__version__)" 2>$null) } else { $null }
$numpyVersion = if (Test-Path -LiteralPath $PythonExecutable) { (& $PythonExecutable -c "import numpy; print(numpy.__version__)" 2>$null) } else { $null }
$ffmpegVersion = $null
$hardwareAccelerators = @()
if (-not [string]::IsNullOrWhiteSpace($FfmpegExecutable) -and (Test-Path -LiteralPath $FfmpegExecutable)) {
    $ffmpegVersion = (& $FfmpegExecutable -version 2>&1 | Select-Object -First 1)
    $hardwareAccelerators = @(& $FfmpegExecutable -hide_banner -hwaccels 2>&1 | Select-Object -Skip 1 | Where-Object { -not [string]::IsNullOrWhiteSpace($_) })
}
$os = Get-CimInstance Win32_OperatingSystem
$cpu = Get-CimInstance Win32_Processor | Select-Object -First 1
$gpus = @(Get-CimInstance Win32_VideoController | ForEach-Object {
    [ordered]@{ name = $_.Name; driverVersion = $_.DriverVersion; adapterRamBytes = [long]$_.AdapterRAM }
})
$networkAdapters = @()
if (Get-Command Get-NetAdapter -ErrorAction SilentlyContinue) {
    $networkAdapters = @(Get-NetAdapter -Physical -ErrorAction SilentlyContinue | Where-Object Status -eq 'Up' | ForEach-Object {
        [ordered]@{ name = $_.Name; interfaceDescription = $_.InterfaceDescription; linkSpeed = [string]$_.LinkSpeed }
    })
}
$activePowerPlan = (& powercfg /GETACTIVESCHEME 2>$null) -join ' '
try {
    $GoExecutable = & (Join-Path $PSScriptRoot 'Resolve-GoExecutable.ps1') `
        -RequestedPath $GoExecutable `
        -RequiredVersionPattern 'go1\.26(?:\.|\s)'
}
catch {
    Write-Warning $_.Exception.Message
    $GoExecutable = ""
}
$goVersion = if (-not [string]::IsNullOrWhiteSpace($GoExecutable) -and (Test-Path -LiteralPath $GoExecutable)) { (& $GoExecutable version) -join ' ' } else { $null }
$result = [ordered]@{
    schemaVersion = 1
    measuredAt = (Get-Date).ToUniversalTime().ToString('o')
    host = $env:COMPUTERNAME
    os = [ordered]@{ caption = $os.Caption; version = $os.Version; buildNumber = $os.BuildNumber }
    cpu = [ordered]@{ name = $cpu.Name.Trim(); cores = $cpu.NumberOfCores; logicalProcessors = $cpu.NumberOfLogicalProcessors }
    memoryBytes = [long]$os.TotalVisibleMemorySize * 1KB
    gpus = $gpus
    networkAdapters = $networkAdapters
    activePowerPlan = $activePowerPlan
    python = [ordered]@{ executable = $PythonExecutable; version = $pythonVersion; opencvVersion = $opencvVersion; numpyVersion = $numpyVersion }
    ffmpeg = [ordered]@{ executable = $FfmpegExecutable; version = $ffmpegVersion; hardwareAccelerators = $hardwareAccelerators }
    go = [ordered]@{ executable = $GoExecutable; version = $goVersion }
}
if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    $OutputPath = Join-Path $PSScriptRoot ".artifacts\scale-environment\$($env:COMPUTERNAME).json"
}
$OutputPath = [System.IO.Path]::GetFullPath($OutputPath)
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $OutputPath) | Out-Null
[System.IO.File]::WriteAllText($OutputPath, ($result | ConvertTo-Json -Depth 8), [System.Text.UTF8Encoding]::new($false))
$result
Write-Host "Environment report: $OutputPath"
