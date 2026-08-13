[CmdletBinding()]
param(
    [string]$PythonExecutable = "python",
    [string]$EnvironmentPath = "",
    [switch]$IncludeNvCodec
)

$ErrorActionPreference = "Stop"
if ([string]::IsNullOrWhiteSpace($EnvironmentPath)) {
    $EnvironmentPath = Join-Path $PSScriptRoot ".artifacts\aruco-venv"
}
$EnvironmentPath = [System.IO.Path]::GetFullPath($EnvironmentPath)
& $PythonExecutable -m venv $EnvironmentPath
if ($LASTEXITCODE -ne 0) { throw "Python virtual environment creation failed" }
$venvPython = Join-Path $EnvironmentPath "Scripts\python.exe"
& $venvPython -m pip install --upgrade pip
if ($LASTEXITCODE -ne 0) { throw "pip upgrade failed" }
& $venvPython -m pip install -r (Join-Path $PSScriptRoot "aruco-capacity-requirements.txt")
if ($LASTEXITCODE -ne 0) { throw "AruCo capacity dependencies installation failed" }
if ($IncludeNvCodec) {
    $nvSmiCandidates = @()
    $nvSmiCommand = Get-Command nvidia-smi.exe -ErrorAction SilentlyContinue
    if ($null -ne $nvSmiCommand) { $nvSmiCandidates += $nvSmiCommand.Source }
    if (-not [string]::IsNullOrWhiteSpace($env:WINDIR)) {
        $nvSmiCandidates += (Join-Path $env:WINDIR "System32\nvidia-smi.exe")
    }
    $nvSmi = @($nvSmiCandidates | Where-Object {
        -not [string]::IsNullOrWhiteSpace($_) -and (Test-Path -LiteralPath $_ -PathType Leaf)
    } | Select-Object -Unique -First 1)
    if ($nvSmi.Count -eq 0) {
        throw "NVIDIA driver utility was not found. Install a supported NVIDIA display driver before using -IncludeNvCodec."
    }
    $nvCodecGpu = & $nvSmi[0] --query-gpu=name,driver_version,memory.total --format=csv,noheader
    if ($LASTEXITCODE -ne 0 -or @($nvCodecGpu).Count -eq 0) {
        throw "NVIDIA GPU/driver validation failed: $($nvSmi[0])"
    }
    & $venvPython -m pip install -r (Join-Path $PSScriptRoot "aruco-capacity-nvcodec-requirements.txt")
    if ($LASTEXITCODE -ne 0) { throw "AruCo NVDEC dependencies installation failed" }
    $nvcodecProbe = @'
import os
import pathlib
import sys
cuda_root = pathlib.Path(sys.prefix) / "Lib" / "site-packages" / "nvidia" / "cuda_runtime"
if os.name == "nt" and cuda_root.is_dir():
    os.environ["CUDA_PATH"] = str(cuda_root)
    os.add_dll_directory(str(cuda_root / "bin"))
import PyNvVideoCodec
print("PyNvVideoCodec=ready")
'@
    $nvcodecVersion = & $venvPython -c $nvcodecProbe
    if ($LASTEXITCODE -ne 0) { throw "PyNvVideoCodec import validation failed" }
}
$versions = & $venvPython -c "import cv2, numpy, psutil; assert hasattr(cv2, 'aruco'); print(f'OpenCV={cv2.__version__} NumPy={numpy.__version__} psutil={psutil.__version__}')"
if ($LASTEXITCODE -ne 0) { throw "AruCo capacity dependency validation failed" }
$ffmpeg = & $venvPython -c "import imageio_ffmpeg; print(imageio_ffmpeg.get_ffmpeg_exe())"
if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $ffmpeg)) { throw "Bundled FFmpeg resolution failed" }
Write-Host "AruCo capacity environment: $EnvironmentPath"
Write-Host "Python: $venvPython"
Write-Host "FFmpeg: $ffmpeg"
Write-Host $versions
if ($IncludeNvCodec) { Write-Host $nvcodecVersion }
if ($IncludeNvCodec) { Write-Host "NVIDIA GPU: $($nvCodecGpu -join '; ')" }
