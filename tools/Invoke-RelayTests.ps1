[CmdletBinding()]
param(
    [string]$GoExecutable = "",
    [switch]$Race,
    [string]$Run = ""
)

$ErrorActionPreference = "Stop"
$resolver = Join-Path $PSScriptRoot "Resolve-GoExecutable.ps1"
$go = & $resolver -RequestedPath $GoExecutable -RequiredVersionPattern "go1\.26(?:\.|\s)"
$relayDirectory = Join-Path $PSScriptRoot "momo-relay"
$arguments = [System.Collections.Generic.List[string]]::new()
$arguments.Add("test")
if ($Race) {
    $cgoEnabled = (& $go env CGO_ENABLED 2>&1) -join " "
    if ($LASTEXITCODE -ne 0 -or $cgoEnabled.Trim() -ne "1") {
        throw "Go race tests require CGO_ENABLED=1 and a working C compiler. This is separate from Go executable discovery; use the Linux CI job or a configured local CGO toolchain. Found CGO_ENABLED=$cgoEnabled"
    }
    $arguments.Add("-race")
}
if (-not [string]::IsNullOrWhiteSpace($Run)) {
    $arguments.Add("-run")
    $arguments.Add($Run)
}
$arguments.Add("./...")

Write-Host "Using Go: $go"
Push-Location $relayDirectory
try {
    & $go @arguments
    if ($LASTEXITCODE -ne 0) {
        throw "Relay tests failed with exit code $LASTEXITCODE"
    }
}
finally {
    Pop-Location
}
