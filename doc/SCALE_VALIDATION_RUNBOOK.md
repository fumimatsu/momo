# Momo Scale Validation Runbook

## 目的

RelayのWebRTC中継能力と、実走映像を使ったArUco検出能力を別PCで同じ条件により測定する。
結果は平均値ではなく、最も遅いsourceのFPS、p95遅延、process tree CPU、切断復旧を比較する。

## 標準条件

| 項目 | 値 |
| --- | --- |
| 入力映像 | 960x528、50 FPS、上下を正したH.264 |
| ArUco | OpenCV 4.10、DICT_4X4_50、MADSYSTEM互換parameter |
| 認識縮尺 | 0.6 |
| 検出周期 | 25 Hz（50 FPS入力の2フレームに1回） |
| 最低検出FPS | 23.75 Hz（目標の95%） |
| 検出遅延p95 | 40 ms以下 |
| process tree CPU p95 | 全論理CPU正規化で60%以下 |
| Relay映像負荷 | 30 FPS、約2.304 Mbps/source |

元録画`cpu-shadow-20260731T122739445Z-732b1f8f.webm`のSHA-256は
`91A843493429A61C40E65474C829B60A83079C7DD0D63BCF8ABD7712D9E20E5D`である。
録画はサイズと利用範囲の都合でGitへ含めない。同じファイルを試験PCへ渡し、hashを確認する。

## 前提

- Windows 10/11 x64、PowerShell 5.1以上
- Python 3.12以上
- Relay試験を行う場合はGo 1.26
- QSVは対応Intel GPUとdriver、CUDAは対応NVIDIA GPUとdriver
- 有線1GbE以上。32台や複数fan-outの本番検証は2.5GbEを推奨
- 測定中はWindows Update、ウイルススキャン、録画、ゲームなどを止める

FFmpegはPython環境へ同梱版を導入する。別途FFmpegを配置する必要はない。ただし同梱版が対象GPUの
hardware decodeに対応しない場合は、対応buildを`-FfmpegExecutable`で指定する。

## 1. 取得と準備

```powershell
git clone https://github.com/fumimatsu/momo.git D:\src\momo
Set-Location D:\src\momo
git switch codex/relay-scale-foundation

.\tools\Initialize-ArucoCapacity.ps1
Get-FileHash D:\recordings\cpu-shadow-20260731T122739445Z-732b1f8f.webm -Algorithm SHA256
.\tools\Prepare-ArucoCapacityInput.ps1 `
  -InputPath D:\recordings\cpu-shadow-20260731T122739445Z-732b1f8f.webm `
  -RotateDegrees 180
```

準備後の入力は`tools/.artifacts/aruco-input/`へ保存される。encoder build差で正規化後hashが変わる
可能性があるため、PC間の同一性は元録画hashで判断する。

## 2. ArUco capacity

最初は30秒で境界を探し、採用候補の最大台数だけ600秒、最終的には本番PCで1時間測る。

```powershell
.\tools\Invoke-ArucoCapacitySuite.ps1 `
  -InputPath .\tools\.artifacts\aruco-input\cpu-shadow-20260731T122739445Z-732b1f8f-upright-h264.mp4 `
  -SourceCounts 1,2,4,6,8,10,12,16 `
  -DurationSeconds 30

.\tools\Measure-ArucoCapacity.ps1 `
  -InputPath .\tools\.artifacts\aruco-input\cpu-shadow-20260731T122739445Z-732b1f8f-upright-h264.mp4 `
  -SourceCounts 8 -DurationSeconds 600 -Decoder qsv
```

suiteは`opencv`、`qsv`、`cuda`を最後まで試し、失敗した台数があっても残りのdecoderを継続する。
`suite-summary.json`に各decoderの最大合格台数、`environment.json`にPC構成を保存する。
hardware名が表示されても実際に初期化できない場合は、そのdecoder reportを不合格として扱う。

## 3. Relay scale

擬似Momo、Relay、read-only Observer、Pilotを同一PC上で起動する。試験用portだけを使用し、
本番Relayを必要としない。

```powershell
$env:MOMO_GO_EXE = 'C:\Program Files\Go\bin\go.exe'
.\tools\Invoke-RelayScaleMatrix.ps1 `
  -SourceCounts 4,8,12,16,24,32 `
  -DurationSeconds 60 -WarmupSeconds 10 `
  -ObserversPerSource 1 -PilotSource sim-01

.\tools\Invoke-RelayScaleMatrix.ps1 `
  -SourceCounts 32 -DurationSeconds 600 -WarmupSeconds 30 `
  -ObserversPerSource 1 -PilotSource sim-01 -RecoverySource sim-16
```

32 source × Observer 2台のfan-outを測る場合は`-ObserversPerSource 2`とする。試験結果は
`tools/.artifacts/relay-scale-matrix/`に保存される。

## 4. 結果の受け渡し

次のディレクトリをPC名と日付が分かるzipへまとめる。映像と仮想環境は含めない。

- `tools/.artifacts/aruco-capacity-suite/<timestamp>/`
- 採用候補の10分または1時間`report.json`
- `tools/.artifacts/relay-scale-matrix/<timestamp>/`
- 元録画のSHA-256

比較時は最大合格台数だけでなく、CPU p95が50%付近に収まる台数を通常割当とする。60%ぎりぎりの
台数はhard capにも使用せず、OS・Relay・Race Control・温度変化の余力を残す。

## 5. 合否後の判断

- 25 Hzを維持できないPCは検出周期を下げず、担当source数を減らす。
- 1台だけ遅い場合もnode全体を合格にしない。
- QSV/CUDAが失敗した場合はsoftware結果を採用し、driver更新後に再測定する。
- 10分合格は候補確定、1時間複数回合格を本番上限確定とする。
- 録画capacityはWebRTC end-to-end保証ではない。Marker Observer完成後にRelay受信込みで再測定する。

## 6. 50 Hz比較プロファイル

50 FPS入力を全フレーム認識する場合は、25 Hzの通常試験とは成果物ディレクトリを分ける。
合格条件は最低入力・検出47.5 FPS、検出処理latency p95 20 ms以下、process tree CPU p95 60%以下である。
計測器は入力映像が47.5 FPS未満の場合、補間による見かけ上の50 Hz計測を防ぐため開始前に失敗する。

最初は1、2、4台から開始し、合格が続く間だけ6、8台へ増やす。

```powershell
.\tools\Invoke-ArucoCapacitySuite.ps1 `
  -InputPath .\tools\.artifacts\aruco-input\cpu-shadow-20260731T122739445Z-732b1f8f-upright-h264.mp4 `
  -SourceCounts 1,2,4,6,8 `
  -DurationSeconds 30 -DetectionHz 50 `
  -OutputDirectory .\tools\.artifacts\aruco-capacity-suite\50hz-$env:COMPUTERNAME
```

最大合格台数が分かったら、その台数だけ10分確認する。decoderはsuiteで最も余力があった経路を指定する。

```powershell
.\tools\Measure-ArucoCapacity.ps1 `
  -InputPath .\tools\.artifacts\aruco-input\cpu-shadow-20260731T122739445Z-732b1f8f-upright-h264.mp4 `
  -SourceCounts 4 -DurationSeconds 600 -DetectionHz 50 -Decoder qsv `
  -OutputPath .\tools\.artifacts\aruco-capacity-suite\50hz-$env:COMPUTERNAME\qsv-soak-report.json
```

25 Hzと50 Hzは同じPC、電源プラン、driver、入力hash、recognition qualityで比較する。
台数上限だけでなく、`environment.json`と各reportの`acceptance`、CPU p95、検出latency p95を一緒に残す。
