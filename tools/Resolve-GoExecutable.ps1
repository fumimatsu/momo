[CmdletBinding()]
param(
    [string]$RequestedPath = "",
    [string]$RequiredVersionPattern = "",
    [string[]]$AdditionalCandidates = @()
)

$ErrorActionPreference = "Stop"
Set-StrictMode -Version Latest

$candidates = [System.Collections.Generic.List[string]]::new()
function Add-GoCandidate {
    param([string]$Path)

    if ([string]::IsNullOrWhiteSpace($Path)) {
        return
    }
    $expanded = [Environment]::ExpandEnvironmentVariables($Path.Trim())
    if (-not $candidates.Contains($expanded)) {
        $candidates.Add($expanded)
    }
}

Add-GoCandidate $RequestedPath
Add-GoCandidate $env:MOMO_GO_EXE
foreach ($candidate in $AdditionalCandidates) {
    Add-GoCandidate $candidate
}

$repoRoot = Split-Path -Parent $PSScriptRoot
Add-GoCandidate (Join-Path $PSScriptRoot "momo-relay\.toolchain\go\bin\go.exe")
Add-GoCandidate (Join-Path $repoRoot ".toolchain\go\bin\go.exe")

$pathCommand = Get-Command go.exe -ErrorAction SilentlyContinue
if ($null -ne $pathCommand) {
    Add-GoCandidate $pathCommand.Source
}

$codexToolchainRoot = Join-Path $HOME ".codex\toolchains"
if (Test-Path -LiteralPath $codexToolchainRoot -PathType Container) {
    Get-ChildItem -LiteralPath $codexToolchainRoot -Directory -Filter "go*" -ErrorAction SilentlyContinue |
        Sort-Object Name -Descending |
        ForEach-Object { Add-GoCandidate (Join-Path $_.FullName "go\bin\go.exe") }
}

Add-GoCandidate (Join-Path $HOME "scoop\apps\go\current\bin\go.exe")
Add-GoCandidate (Join-Path $env:LOCALAPPDATA "Programs\Go\bin\go.exe")
if (-not [string]::IsNullOrWhiteSpace($env:GOROOT)) {
    Add-GoCandidate (Join-Path $env:GOROOT "bin\go.exe")
}
if (-not [string]::IsNullOrWhiteSpace($env:ProgramFiles)) {
    Add-GoCandidate (Join-Path $env:ProgramFiles "Go\bin\go.exe")
}

$wingetRoot = Join-Path $env:LOCALAPPDATA "Microsoft\WinGet\Packages"
if (Test-Path -LiteralPath $wingetRoot -PathType Container) {
    Get-ChildItem -LiteralPath $wingetRoot -Directory -Filter "GoLang.Go*" -ErrorAction SilentlyContinue |
        Sort-Object Name -Descending |
        ForEach-Object {
            Add-GoCandidate (Join-Path $_.FullName "go\bin\go.exe")
            Add-GoCandidate (Join-Path $_.FullName "bin\go.exe")
        }
}

foreach ($drive in Get-PSDrive -PSProvider FileSystem -ErrorAction SilentlyContinue) {
    if ([string]::IsNullOrWhiteSpace($drive.Root) -or $drive.Root -notmatch '^[A-Za-z]:\\$') {
        continue
    }
    Add-GoCandidate (Join-Path $drive.Root "Go\bin\go.exe")
    foreach ($parent in @("app", "tools")) {
        $parentPath = Join-Path $drive.Root $parent
        if (-not (Test-Path -LiteralPath $parentPath -PathType Container)) {
            continue
        }
        Get-ChildItem -LiteralPath $parentPath -Directory -Filter "go*" -ErrorAction SilentlyContinue |
            Sort-Object Name -Descending |
            ForEach-Object {
                Add-GoCandidate (Join-Path $_.FullName "bin\go.exe")
                Add-GoCandidate (Join-Path $_.FullName "go\bin\go.exe")
            }
    }
}
if (-not [string]::IsNullOrWhiteSpace(${env:ProgramFiles(x86)})) {
    Add-GoCandidate (Join-Path ${env:ProgramFiles(x86)} "Go\bin\go.exe")
}

foreach ($registryPath in @(
    "HKLM:\SOFTWARE\GoProgrammingLanguage",
    "HKLM:\SOFTWARE\WOW6432Node\GoProgrammingLanguage",
    "HKCU:\SOFTWARE\GoProgrammingLanguage"
)) {
    if (Test-Path -LiteralPath $registryPath) {
        $registryValues = Get-ItemProperty -LiteralPath $registryPath -ErrorAction SilentlyContinue
        $installRootProperty = if ($null -ne $registryValues) {
            $registryValues.PSObject.Properties['InstallRoot']
        }
        else {
            $null
        }
        $installRoot = if ($null -ne $installRootProperty) { [string]$installRootProperty.Value } else { "" }
        if (-not [string]::IsNullOrWhiteSpace($installRoot)) {
            Add-GoCandidate (Join-Path $installRoot "bin\go.exe")
        }
    }
}

foreach ($root in @($env:MOMO_TOOLCHAIN_ROOTS -split ";")) {
    if ([string]::IsNullOrWhiteSpace($root) -or -not (Test-Path -LiteralPath $root -PathType Container)) {
        continue
    }
    Get-ChildItem -LiteralPath $root -Directory -Filter "go*" -ErrorAction SilentlyContinue |
        Sort-Object Name -Descending |
        ForEach-Object {
            Add-GoCandidate (Join-Path $_.FullName "bin\go.exe")
            Add-GoCandidate (Join-Path $_.FullName "go\bin\go.exe")
        }
}

$inspected = [System.Collections.Generic.List[string]]::new()
foreach ($candidate in $candidates) {
    if (-not (Test-Path -LiteralPath $candidate -PathType Leaf)) {
        continue
    }

    $resolved = (Resolve-Path -LiteralPath $candidate).Path
    $version = (& $resolved version 2>&1) -join " "
    if ($LASTEXITCODE -ne 0) {
        $inspected.Add("$resolved (version command failed)")
        continue
    }
    $inspected.Add("$resolved ($version)")
    if ([string]::IsNullOrWhiteSpace($RequiredVersionPattern) -or $version -match $RequiredVersionPattern) {
        Write-Verbose "Resolved Go toolchain: $resolved ($version)"
        $resolved
        exit 0
    }
}

$details = if ($inspected.Count -gt 0) { $inspected -join "; " } else { "none" }
$requirement = if ([string]::IsNullOrWhiteSpace($RequiredVersionPattern)) { "any version" } else { "pattern '$RequiredVersionPattern'" }
throw "Go executable matching $requirement was not found. Checked PATH, MOMO_GO_EXE, repository and Codex toolchains, GOROOT, Scoop, WinGet, standard installers, registry, common drive roots, and MOMO_TOOLCHAIN_ROOTS. Existing candidates: $details"
