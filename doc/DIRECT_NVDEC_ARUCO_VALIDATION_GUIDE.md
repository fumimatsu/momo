# Direct NVDEC ArUco Validation Guide

## 目的

NVIDIA GPUを搭載したWindows PCで、PyNvVideoCodecによるdirect NVDECと既存CPU ArUcoを
同じ条件で再現・測定する。これはMarker Observer本体ではなく、次の経路のcapacity probeである。

```text
H.264 recording
  -> PyNvVideoCodec / NVDEC
  -> host NV12 Y plane
  -> resize
  -> OpenCV CPU ArUco
```

Phase 2では`use_device_memory=True`へ切り替え、NVDEC後のresize、threshold、quad candidate抽出を
GPUへ移す。このガイドのparity reportを、GPU化前後の退行判定に使う。

## 対応条件

- Windows 10/11 x64
- Python 3.12
- NVIDIA Turing、Ampere、Ada、Hopper、Blackwell GPU
- pre-Blackwell GPUはWindows display driver 531.61以上
- PowerShell 5.1以上
- 同一比較には指定の元録画

GPU名とdriverは次で確認する。

```powershell
nvidia-smi --query-gpu=name,driver_version,memory.total --format=csv
```

`nvidia-smi`がPATHにない場合は、通常のNVIDIA driver配置
`C:\Windows\System32\nvidia-smi.exe`も確認する。PyNvVideoCodecのimportだけでは、対象codecを
実際にdecodeできることまでは保証しない。必ず1 sourceのsmoke testまで実行する。

## 1. RepositoryとPython環境

```powershell
git clone https://github.com/fumimatsu/momo.git D:\src\momo
Set-Location D:\src\momo
git switch master
git pull --ff-only

.\tools\Initialize-ArucoCapacity.ps1 -IncludeNvCodec
```

既定では`tools/.artifacts/aruco-venv/`へ仮想環境を作り、次を固定versionで導入する。

- PyNvVideoCodec 2.2.0
- NVIDIA CUDA Runtime 12.9.79
- OpenCV contrib headless 4.10.0.84
- NumPy 2.5.2
- psutil 7.2.2

仮想環境、CUDA DLL、測定結果は`tools/.artifacts/`配下にあり、Gitには追加しない。

## 2. 共通入力の準備

元録画を別PCへ渡し、変換前のhashを確認する。

```powershell
Get-FileHash D:\recordings\cpu-shadow-20260731T122739445Z-732b1f8f.webm -Algorithm SHA256
```

期待値:

```text
91A843493429A61C40E65474C829B60A83079C7DD0D63BCF8ABD7712D9E20E5D
```

上下を正したH.264入力を作る。

```powershell
.\tools\Prepare-ArucoCapacityInput.ps1 `
  -InputPath D:\recordings\cpu-shadow-20260731T122739445Z-732b1f8f.webm `
  -RotateDegrees 180
```

変換後hashはencoder buildで変わり得るため、PC間の同一性は元録画hashで判断する。

## 3. Smoke test

```powershell
$input = '.\tools\.artifacts\aruco-input\cpu-shadow-20260731T122739445Z-732b1f8f-upright-h264.mp4'
.\tools\Measure-ArucoCapacity.ps1 `
  -InputPath $input -SourceCounts 1 -DurationSeconds 10 `
  -DetectionHz 50 -Decoder nvcodec
```

`decode`と`detect`が47.5 FPS以上で、worker errorがないことを確認する。失敗した場合は台数試験へ
進まず、driver、GPU認識、Python architecture、CUDA runtime、入力codecを確認する。

## 4. 30秒capacity境界

RTX 5070 / Ryzen 7 9700Xでの既知結果は参考値であり、別PCへ台数を転用しない。

RTX 3060 / Core i7-8700 PCは次から開始する。

```powershell
.\tools\Measure-ArucoCapacity.ps1 `
  -InputPath $input -SourceCounts 1,4,5,6,8 -DurationSeconds 30 `
  -DetectionHz 50 -Decoder nvcodec `
  -OutputPath ".\tools\.artifacts\aruco-capacity-suite\50hz-nvcodec-$env:COMPUTERNAME\capacity-report.json"
```

RTX 3060はAmpere NVDECでH.264 decodeに対応する。今回の960x528 / 50 FPS映像では、NVDECよりも
Core i7-8700上のCPU ArUcoが先に上限へ達する可能性が高い。合格条件は次のすべてである。

- 全sourceのdecode / detectが47.5 FPS以上
- detection latency p95が20ms以下
- process tree CPU p95が60%以下
- worker errorとread errorが0

最後に合格した台数が短時間の物理境界であり、そのまま運用台数にはしない。

## 5. CPU/NVDEC parity

```powershell
.\tools\Compare-ArucoBackends.ps1 `
  -InputPath $input -FrameCount 1500 `
  -OutputPath ".\tools\.artifacts\aruco-parity\$env:COMPUTERNAME\report.json"
```

運用marker ID 1、2、3のframe-set一致率99%以上と、3 detection frame以上のqualified group数一致を
要求する。unknown IDとisolatedな単発検出は診断へ残し、course allowlistと通過debounceなしに
race event化しない。

## 6. 運用候補のsoak

短時間合格台数から少なくとも20%のCPU余力を残す台数を選び、10分、次に1時間を測る。

```powershell
$candidate = 4
.\tools\Measure-ArucoCapacity.ps1 `
  -InputPath $input -SourceCounts $candidate -DurationSeconds 600 `
  -DetectionHz 50 -Decoder nvcodec `
  -OutputPath ".\tools\.artifacts\aruco-capacity-suite\50hz-nvcodec-$env:COMPUTERNAME\soak-10m.json"

.\tools\Measure-ArucoCapacity.ps1 `
  -InputPath $input -SourceCounts $candidate -DurationSeconds 3600 `
  -DetectionHz 50 -Decoder nvcodec `
  -OutputPath ".\tools\.artifacts\aruco-capacity-suite\50hz-nvcodec-$env:COMPUTERNAME\soak-1h.json"
```

`$candidate = 4`はRTX 3060 PCの確定値ではなく開始例である。10分合格を候補、1時間の複数回合格と
Relay/WebRTCからmarker eventまでのE2E合格を本番上限の条件とする。

## 7. 結果の受け渡し

次だけをPC名・日付付きzipへまとめる。元録画、変換映像、仮想環境は含めない。

- capacity `report.json`
- parity `report.json`
- 10分 / 1時間soak `report.json`
- suiteを使った場合の`environment.json`と`suite-summary.json`
- 元録画SHA-256

## ライセンス境界

このrepositoryはPyNvVideoCodec、CUDA runtime、OpenCV wheelを同梱せず、各PCでpipから導入する。
現在確認した依存関係は次のとおりである。

| Component | Version | License |
| --- | --- | --- |
| PyNvVideoCodec | 2.2.0 | MIT。packageのNOTICESにFFmpeg LGPL v3を含む |
| NVIDIA CUDA Runtime | 12.9.79 | NVIDIA proprietary EULA |
| OpenCV Python package | 4.10.0.84 | MIT wrapper、同梱OpenCVはApache 2.0 |
| NumPy | 2.5.2 | BSD-3-Clauseほか |
| psutil | 7.2.2 | BSD-3-Clause |
| imageio-ffmpeg | 0.6.0 | BSD-2-Clause。bundled FFmpegのlicenseはbuild依存 |

内部利用や各nodeでの通常のpip installと、wheel / CUDA DLLを含むoffline installerの第三者配布は
分けて扱う。後者を行う場合は、各packageのlicense/NOTICE全文、NVIDIA SDK/runtimeの再配布条件、
使用するFFmpeg buildのLGPL/GPL条件、H.264特許条件を配布物単位で再確認する。NVIDIA SDKやruntimeを
standalone productとして再配布しない。本項は法的助言ではなく、配布前確認事項の記録である。

## 関連文書

- [Scale Validation Runbook](SCALE_VALIDATION_RUNBOOK.md)
- [GPU ArUco Implementation Plan](GPU_ARUCO_IMPLEMENTATION_PLAN.md)
- [Scalable Marker and Program Observer Design](SCALABLE_MARKER_AND_PROGRAM_OBSERVER_DESIGN.md)
