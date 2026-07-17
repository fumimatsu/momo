param(
    [string]$MappingName = 'Local\MomoObserverFrameV1',
    [ValidateRange(1, 120)]
    [int]$Fps = 50
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$width = 1920
$height = 1080
$stride = $width * 4
$bufferCount = 3
$headerSize = 64
$frameSize = [int64]$stride * $height
$mappingSize = [int64]$headerSize + $frameSize * $bufferCount

try {
    $existing = [System.IO.MemoryMappedFiles.MemoryMappedFile]::OpenExisting(
        $MappingName,
        [System.IO.MemoryMappedFiles.MemoryMappedFileRights]::ReadWrite)
    $existing.Dispose()
    throw "Shared mapping '$MappingName' already exists. Stop Observer or choose another -MappingName."
}
catch [System.IO.FileNotFoundException] {
    # Create a new test mapping.
}

$mapping = [System.IO.MemoryMappedFiles.MemoryMappedFile]::CreateOrOpen(
    $MappingName,
    $mappingSize,
    [System.IO.MemoryMappedFiles.MemoryMappedFileAccess]::ReadWrite)
$view = $mapping.CreateViewAccessor(
    0,
    $mappingSize,
    [System.IO.MemoryMappedFiles.MemoryMappedFileAccess]::ReadWrite)

try {
    # Initialize the same 64 byte SharedFrameHeader used by Observer.
    $view.Write(0, [uint32]0x3146504D) # MFP1
    $view.Write(4, [uint16]1)
    $view.Write(6, [uint16]$headerSize)
    $view.Write(8, [uint32]$width)
    $view.Write(12, [uint32]$height)
    $view.Write(16, [uint32]$stride)
    $view.Write(20, [uint32]0x41524742) # BGRA
    $view.Write(24, [uint32]$bufferCount)
    $view.Write(28, [int32]0)
    $view.Write(32, [int32]0)
    $view.Write(40, [int64]0)
    $view.Write(48, [int64]0)

    # Build one opaque green BGRA frame (#00FF00) and copy it to all buffers.
    $frame = New-Object byte[] $frameSize
    $frame[0] = 0
    $frame[1] = 255
    $frame[2] = 0
    $frame[3] = 255
    $filled = 4
    while ($filled -lt $frame.Length) {
        $copyLength = [Math]::Min($filled, $frame.Length - $filled)
        [Buffer]::BlockCopy($frame, 0, $frame, $filled, $copyLength)
        $filled += $copyLength
    }
    for ($buffer = 0; $buffer -lt $bufferCount; $buffer++) {
        $view.WriteArray($headerSize + $buffer * $frameSize, $frame, 0, $frame.Length) | Out-Null
    }

    Write-Host "Publishing '$MappingName': ${width}x${height} BGRA / ${bufferCount} buffers / ${Fps} fps. Press Ctrl+C to stop."
    $sequence = [int64]0
    $activeBuffer = 0
    $intervalMs = [Math]::Max(1, [int][Math]::Round(1000.0 / $Fps))
    while ($true) {
        $sequence++ # odd: write in progress
        $view.Write(40, $sequence)
        $timestampNs = [DateTimeOffset]::UtcNow.ToUnixTimeMilliseconds() * 1000000
        $view.Write(48, [int64]$timestampNs)
        $view.Write(28, [int32]$activeBuffer)
        $sequence++ # even: complete frame
        $view.Write(40, $sequence)
        $activeBuffer = ($activeBuffer + 1) % $bufferCount
        Start-Sleep -Milliseconds $intervalMs
    }
}
finally {
    if ($null -ne $view) {
        $view.Dispose()
    }
    if ($null -ne $mapping) {
        $mapping.Dispose()
    }
}
