[CmdletBinding()]
param(
    [Parameter(Mandatory)][string]$InputPath,
    [ValidateSet(0, 90, 180, 270)][int]$RotateDegrees = 0,
    [string]$OutputPath = "",
    [string]$PythonExecutable = "",
    [string]$FfmpegExecutable = ""
)

$ErrorActionPreference = "Stop"
$InputPath = [System.IO.Path]::GetFullPath($InputPath)
if (-not (Test-Path -LiteralPath $InputPath -PathType Leaf)) { throw "Input video not found: $InputPath" }
if ([string]::IsNullOrWhiteSpace($PythonExecutable)) {
    $PythonExecutable = Join-Path $PSScriptRoot ".artifacts\aruco-venv\Scripts\python.exe"
}
if ([string]::IsNullOrWhiteSpace($FfmpegExecutable)) {
    if (-not (Test-Path -LiteralPath $PythonExecutable)) { throw "Run Initialize-ArucoCapacity.ps1 first" }
    $FfmpegExecutable = & $PythonExecutable -c "import imageio_ffmpeg; print(imageio_ffmpeg.get_ffmpeg_exe())"
}
if (-not (Test-Path -LiteralPath $FfmpegExecutable -PathType Leaf)) { throw "FFmpeg not found: $FfmpegExecutable" }
if ([string]::IsNullOrWhiteSpace($OutputPath)) {
    $baseName = [System.IO.Path]::GetFileNameWithoutExtension($InputPath)
    $OutputPath = Join-Path $PSScriptRoot ".artifacts\aruco-input\$baseName-upright-h264.mp4"
}
$OutputPath = [System.IO.Path]::GetFullPath($OutputPath)
New-Item -ItemType Directory -Force -Path (Split-Path -Parent $OutputPath) | Out-Null
$filter = switch ($RotateDegrees) {
    0 { $null }
    90 { "transpose=1" }
    180 { "hflip,vflip" }
    270 { "transpose=2" }
}
$arguments = @('-hide_banner', '-loglevel', 'warning', '-y', '-i', $InputPath, '-an')
if ($null -ne $filter) { $arguments += @('-vf', $filter) }
$arguments += @('-c:v', 'libx264', '-preset', 'fast', '-crf', '20', '-pix_fmt', 'yuv420p', '-movflags', '+faststart', $OutputPath)
& $FfmpegExecutable @arguments
if ($LASTEXITCODE -ne 0) { throw "Input normalization failed" }
$hash = (Get-FileHash -Algorithm SHA256 -LiteralPath $OutputPath).Hash
Write-Host "Prepared input: $OutputPath"
Write-Host "SHA256: $hash"
