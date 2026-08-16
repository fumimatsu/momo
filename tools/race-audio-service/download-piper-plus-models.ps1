[CmdletBinding()]
param(
    [string]$Destination = (Join-Path $PSScriptRoot 'models')
)

$ErrorActionPreference = 'Stop'
$assets = @(
    @{
        Name = 'css10-ja-6lang-fp16.onnx'
        Url = 'https://huggingface.co/ayousanz/piper-plus-css10-ja-6lang/resolve/main/css10-ja-6lang-fp16.onnx'
        SHA256 = '5ebc51dbf897238523f3df0d6e0f6c93033bc5cda3f8602a8379ebe2a4738c42'
    },
    @{
        Name = 'css10-ja-6lang-config.json'
        Url = 'https://huggingface.co/ayousanz/piper-plus-css10-ja-6lang/resolve/main/config.json'
        SHA256 = '3c613dacf139349c20159433559b2e94e6364e9a340d95245f3de6006f453fcc'
    },
    @{
        Name = 'tsukuyomi-chan-6lang-fp16.onnx'
        Url = 'https://huggingface.co/ayousanz/piper-plus-tsukuyomi-chan/resolve/main/tsukuyomi-chan-6lang-fp16.onnx'
        SHA256 = '5289e9b6eaf21080803b7fe1c4dc85b5491d4c216121207a41df18dd5f68e5d7'
    },
    @{
        Name = 'tsukuyomi-chan-6lang-config.json'
        Url = 'https://huggingface.co/ayousanz/piper-plus-tsukuyomi-chan/resolve/main/config.json'
        SHA256 = '516058f405ec914140f34832a9d8bb5d8272ba62af9bc7ffb29349715a539780'
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

$nltkData = Join-Path $Destination 'nltk_data'
New-Item -ItemType Directory -Path $nltkData -Force | Out-Null
$previousNltkData = $env:NLTK_DATA
$env:NLTK_DATA = $nltkData
Push-Location $PSScriptRoot
try {
    $env:MOMO_PIPER_NLTK_DATA = $nltkData
    uv run python -c "import nltk, os; target=os.environ['MOMO_PIPER_NLTK_DATA']; assert nltk.download('averaged_perceptron_tagger', download_dir=target); assert nltk.download('averaged_perceptron_tagger_eng', download_dir=target); assert nltk.download('cmudict', download_dir=target)"
}
finally {
    Pop-Location
    Remove-Item Env:MOMO_PIPER_NLTK_DATA -ErrorAction SilentlyContinue
    $env:NLTK_DATA = $previousNltkData
}
