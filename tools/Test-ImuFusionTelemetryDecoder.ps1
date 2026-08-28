param(
    [string]$VisualStudioPath = ''
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
$buildRoot = Join-Path $repoRoot '_build\host-tests\imu-fusion-telemetry-decoder'
$source = Join-Path $repoRoot 'src\serial_data_channel\imu_fusion_telemetry_decoder.cpp'
$testSource = Join-Path $repoRoot 'test\imu_fusion_telemetry_decoder_test.cpp'
$executable = Join-Path $buildRoot 'imu_fusion_telemetry_decoder_test.exe'

if (-not $VisualStudioPath) {
    $vswhere = Join-Path ${env:ProgramFiles(x86)} 'Microsoft Visual Studio\Installer\vswhere.exe'
    if (Test-Path -LiteralPath $vswhere) {
        $VisualStudioPath = (& $vswhere -latest -products * `
            -requires Microsoft.VisualStudio.Component.VC.Tools.x86.x64 `
            -property installationPath | Select-Object -First 1).Trim()
    }
}
if (-not $VisualStudioPath) {
    throw 'Visual Studio C++ tools were not found. Pass -VisualStudioPath explicitly.'
}

$developerCommand = Join-Path $VisualStudioPath 'Common7\Tools\VsDevCmd.bat'
if (-not (Test-Path -LiteralPath $developerCommand)) {
    throw "VsDevCmd.bat was not found: $developerCommand"
}

New-Item -ItemType Directory -Force -Path $buildRoot | Out-Null
$arguments = @(
    '/nologo',
    '/std:c++17',
    '/EHsc',
    '/W4',
    '/WX',
    "/I`"$(Join-Path $repoRoot 'src')`"",
    "`"$source`"",
    "`"$testSource`"",
    "/Fe:`"$executable`""
) -join ' '
$command = "`"$developerCommand`" -arch=x64 -host_arch=x64 >nul && cd /d `"$buildRoot`" && cl $arguments"
& $env:ComSpec /d /s /c $command
if ($LASTEXITCODE -ne 0) {
    throw "Host test compile failed with exit code $LASTEXITCODE"
}

& $executable
if ($LASTEXITCODE -ne 0) {
    throw "Host test failed with exit code $LASTEXITCODE"
}
