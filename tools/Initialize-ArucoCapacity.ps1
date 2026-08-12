[CmdletBinding()]
param(
    [string]$PythonExecutable = "python",
    [string]$EnvironmentPath = ""
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
$versions = & $venvPython -c "import cv2, numpy, psutil; assert hasattr(cv2, 'aruco'); print(f'OpenCV={cv2.__version__} NumPy={numpy.__version__} psutil={psutil.__version__}')"
if ($LASTEXITCODE -ne 0) { throw "AruCo capacity dependency validation failed" }
$ffmpeg = & $venvPython -c "import imageio_ffmpeg; print(imageio_ffmpeg.get_ffmpeg_exe())"
if ($LASTEXITCODE -ne 0 -or -not (Test-Path -LiteralPath $ffmpeg)) { throw "Bundled FFmpeg resolution failed" }
Write-Host "AruCo capacity environment: $EnvironmentPath"
Write-Host "Python: $venvPython"
Write-Host "FFmpeg: $ffmpeg"
Write-Host $versions
