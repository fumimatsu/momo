[CmdletBinding()]
param(
    [string]$Destination = (Join-Path $PSScriptRoot 'models')
)

$ErrorActionPreference = 'Stop'
$assets = @(
    @{
        Name = 'kokoro-v1.0.onnx'
        Url = 'https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/kokoro-v1.0.onnx'
        SHA256 = '7d5df8ecf7d4b1878015a32686053fd0eebe2bc377234608764cc0ef3636a6c5'
    },
    @{
        Name = 'voices-v1.0.bin'
        Url = 'https://github.com/thewh1teagle/kokoro-onnx/releases/download/model-files-v1.0/voices-v1.0.bin'
        SHA256 = 'bca610b8308e8d99f32e6fe4197e7ec01679264efed0cac9140fe9c29f1fbf7d'
    }
)

New-Item -ItemType Directory -Path $Destination -Force | Out-Null
foreach ($asset in $assets) {
    $path = Join-Path $Destination $asset.Name
    if (-not (Test-Path -LiteralPath $path)) {
        Invoke-WebRequest -Uri $asset.Url -OutFile $path
    }
    $hash = Get-FileHash -LiteralPath $path -Algorithm SHA256
    if ($hash.Hash.ToLowerInvariant() -ne $asset.SHA256) {
        throw "SHA-256 mismatch for $($asset.Name): expected $($asset.SHA256), got $($hash.Hash.ToLowerInvariant())"
    }
    [pscustomobject]@{
        Name = $asset.Name
        Bytes = (Get-Item -LiteralPath $path).Length
        SHA256 = $hash.Hash.ToLowerInvariant()
    }
}
