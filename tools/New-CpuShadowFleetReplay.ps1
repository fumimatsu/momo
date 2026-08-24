[CmdletBinding()]
param(
    [Parameter(Mandatory)] [string]$CaptureDirectory,
    [ValidateRange(1, 64)] [int]$CarCount = 20,
    [double[]]$PlaybackRates = @(0.92, 0.98, 1.04, 1.10),
    [ValidateRange(0, 64)] [int]$SecondaryCaptureEvery = 5,
    [ValidateRange(1, 120)] [int]$FrameRate = 50,
    [string]$OutputDirectory = '',
    [string]$FFmpegPath = '',
    [switch]$ForceTranscode
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

if ($PlaybackRates.Count -eq 0 -or @($PlaybackRates | Where-Object { $_ -lt 0.25 -or $_ -gt 4 }).Count -gt 0) {
    throw 'PlaybackRates must contain values between 0.25 and 4.'
}
$captureRoot = [IO.Path]::GetFullPath($CaptureDirectory)
if (-not (Test-Path -LiteralPath $captureRoot -PathType Container)) {
    throw "Capture directory was not found: $captureRoot"
}
if ([string]::IsNullOrWhiteSpace($OutputDirectory)) {
    $OutputDirectory = Join-Path $PSScriptRoot '.artifacts\cpu-shadow-fleet-replay'
}
$outputRoot = [IO.Path]::GetFullPath($OutputDirectory)
New-Item -ItemType Directory -Force -Path $outputRoot | Out-Null

if ([string]::IsNullOrWhiteSpace($FFmpegPath)) {
    $ffmpeg = Get-Command ffmpeg.exe -ErrorAction SilentlyContinue
    if ($ffmpeg) {
        $FFmpegPath = $ffmpeg.Source
    }
    else {
        $bundled = 'C:\src\Remotion\node_modules\@remotion\compositor-win32-x64-msvc\ffmpeg.exe'
        if (Test-Path -LiteralPath $bundled -PathType Leaf) { $FFmpegPath = $bundled }
    }
}
if ([string]::IsNullOrWhiteSpace($FFmpegPath) -or -not (Test-Path -LiteralPath $FFmpegPath -PathType Leaf)) {
    throw 'ffmpeg.exe was not found. Pass -FFmpegPath explicitly.'
}

$captures = @()
foreach ($summaryFile in Get-ChildItem -LiteralPath $captureRoot -File -Filter '*-summary.json') {
    $summary = Get-Content -LiteralPath $summaryFile.FullName -Raw | ConvertFrom-Json
    $baseName = $summaryFile.Name.Substring(0, $summaryFile.Name.Length - '-summary.json'.Length)
    $videoPath = Join-Path $captureRoot "$baseName.webm"
    $jsonlPath = Join-Path $captureRoot "$baseName.jsonl"
    if ((Test-Path -LiteralPath $videoPath -PathType Leaf) -and (Test-Path -LiteralPath $jsonlPath -PathType Leaf) -and [double]$summary.duration_ms -gt 1000) {
        $captures += [pscustomobject]@{
            BaseName = $baseName
            VideoPath = $videoPath
            JsonlPath = $jsonlPath
            DurationMS = [double]$summary.duration_ms
        }
    }
}
$captures = @($captures | Sort-Object DurationMS -Descending)
$requiredCaptureCount = if ($SecondaryCaptureEvery -gt 0) { 2 } else { 1 }
if ($captures.Count -lt $requiredCaptureCount) {
    throw "At least $requiredCaptureCount complete CPU-shadow capture(s) are required; found $($captures.Count)."
}

$culture = [Globalization.CultureInfo]::InvariantCulture
$assetCache = @{}
function Get-ReplayAsset {
    param([Parameter(Mandatory)]$Capture, [Parameter(Mandatory)][double]$Rate)
    $rateText = $Rate.ToString('0.######', $culture)
    $rateName = $Rate.ToString('0.00', $culture).Replace('.', 'p')
    $cacheKey = "$($Capture.VideoPath)|$rateText|$FrameRate"
    if ($assetCache.ContainsKey($cacheKey)) { return $assetCache[$cacheKey] }
    $outputPath = Join-Path $outputRoot "$($Capture.BaseName)-rate-$rateName-${FrameRate}fps.h264"
    if ($ForceTranscode -or -not (Test-Path -LiteralPath $outputPath -PathType Leaf)) {
        Write-Host "Transcoding $($Capture.BaseName) at ${rateText}x..."
        $filter = "hflip,vflip,scale=960:528,setpts=PTS/$rateText,fps=$FrameRate"
        & $FFmpegPath -hide_banner -loglevel warning -y -i $Capture.VideoPath -an `
            -vf $filter -c:v libx264 -preset veryfast -tune zerolatency -profile:v baseline -level 3.1 `
            -pix_fmt yuv420p -g $FrameRate -keyint_min $FrameRate -sc_threshold 0 `
            -x264-params 'aud=1:repeat-headers=1' -f h264 $outputPath
        if ($LASTEXITCODE -ne 0) { throw "ffmpeg failed for $($Capture.BaseName) at ${rateText}x" }
    }
    $asset = [pscustomobject]@{
        InputPath = $outputPath
        TargetDurationMS = $Capture.DurationMS / $Rate
    }
    $assetCache[$cacheKey] = $asset
    return $asset
}

$sources = @()
for ($index = 0; $index -lt $CarCount; $index++) {
    # Keep the long run as the primary source. The shorter secondary run is optional because
    # a capture without all configured markers cannot complete a timing lap.
    $captureIndex = if ($SecondaryCaptureEvery -gt 0 -and ($index + 1) % $SecondaryCaptureEvery -eq 0) { 1 } else { 0 }
    $capture = $captures[$captureIndex]
    $rate = [double]$PlaybackRates[$index % $PlaybackRates.Count]
    $asset = Get-ReplayAsset -Capture $capture -Rate $rate
    $groupIndex = [Math]::Floor($index / [Math]::Max(1, $PlaybackRates.Count))
    $startPercent = (($index * 37) + ($groupIndex * 11)) % 80
    $sources += [ordered]@{
        sourceId = 'virtual-{0:d2}' -f ($index + 1)
        inputPath = $asset.InputPath
        fps = $FrameRate
        startOffsetMs = [int64]($asset.TargetDurationMS * $startPercent / 100)
        telemetryPath = $capture.JsonlPath
        captureReplayRate = $rate
    }
}

$manifestPath = Join-Path $outputRoot "cpu-shadow-fleet-${CarCount}cars.json"
$usedCaptureCount = @($sources | ForEach-Object { $_.telemetryPath } | Sort-Object -Unique).Count
$manifest = [ordered]@{
    schemaVersion = 1
    generatedAt = [DateTimeOffset]::UtcNow.ToString('o')
    sourceCaptureCount = $usedCaptureCount
    sources = $sources
}
[IO.File]::WriteAllText($manifestPath, ($manifest | ConvertTo-Json -Depth 8) + [Environment]::NewLine, [Text.UTF8Encoding]::new($false))

$result = [pscustomobject]@{
    ManifestPath = $manifestPath
    CommandReplayJsonl = $captures[0].JsonlPath
    SourceCount = $CarCount
    CaptureCount = $usedCaptureCount
    AssetCount = $assetCache.Count
}
$result
