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
Write-Host "AruCo capacity environment: $EnvironmentPath"
