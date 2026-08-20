# MADSYSTEM と Native Observer の起動

## 現行構成

Relay と Race Control が `192.168.11.100` で動作している場合、この PC では
Native Observer と MADSYSTEM を起動する。Web Observer は確認画面であり、
Native Observer の代わりにはならない。

```text
11.3 - 11.6 Momo
        |
        v
Relay 192.168.11.100:8090 -----> Web Observer (任意)
        |
        v
Native Observer / momo.exe p2p-recv-multi
        |-- Local\MomoObserverFrameV1 (1920x1080 BGRA)
        |       |
        |       v
        |   MADSYSTEM MomoObserverSharedFrameSource
        |
        `-- Local\MomoObserverLumaV1 (marker worker 用 Y plane)

MADSYSTEM ----------------------> Race Control 192.168.11.100:8787
        `------------------------> Relay Gameplay API 192.168.11.100:8090
```

Race Directory API、local cache、Relay DRIVE 状態、Race Control roster lock の確認は
[Race Directory の 11.100 引継ぎ](race-directory-11-100-handoff.md)を参照する。
Native Observer は映像と shared memory の経路であり、Race Directory の cloud credential や
roster identity を保持しない。

## 起動前確認

MADSYSTEM の machine-local 設定は次にある。

```text
%USERPROFILE%\AppData\LocalLow\MADX\MADSYSTEM\MomoRaceControl\settings.json
```

値は次の組み合わせにする。token の実値は log、command line、Git 管理ファイルへ出さない。

```json
{
  "raceControlBaseUrl": "http://192.168.11.100:8787",
  "raceId": "race-test",
  "sourceId": "MADSYSTEM-01",
  "relayGameplayBaseUrl": "http://192.168.11.100:8090"
}
```

Native Observer の Windows build artifact がない場合は先に build する。

```powershell
Set-Location C:\src\momo
python3 run.py build windows_x86_64
```

## 起動順

### 1. 11.100 の到達確認

```powershell
Invoke-WebRequest -UseBasicParsing `
  'http://192.168.11.100:8090/observer.html' -TimeoutSec 5
```

HTTP `200` を確認する。`/api/v1/status` は token 認証ではなく
`-operations-allow-cidr` の送信元 IP 制限を使う。`403` の場合は 11.100 の Relay 起動引数を確認し、
Internet 側を許可せず管理 LAN だけを追加する。

### 2. Native Observer

```powershell
Set-Location C:\src\momo
.\tools\start-mads-observer.ps1 `
  -SkipRelay `
  -ObserverRelayWebSocketUrl 'ws://192.168.11.100:8090/ws' `
  -ObserverSharedOutputFps 50 `
  -ObserverAudioGain 2.5 `
  -RestartObserver
```

この command はローカル Relay を起動しない。11.100 の Relay へ `observer` role で接続し、
次の 2 つを公開する。

- `Local\MomoObserverFrameV1`: MADSYSTEM 用 BGRA composite
- `Local\MomoObserverLumaV1`: GPU marker worker 用 Y plane

MADSYSTEM と映像表示が不要で marker worker だけを使う構成では、
`-ObserverVisualOutput off` を追加する。通常の MADSYSTEM 運用では指定しない。

Marker 出力は `50 Hz` を標準とし、`start-mads-observer.ps1` の既定値も `50` とする。
台数追加後の実測で publication rate、処理 p95、CPU / GPU の運用余力を満たせない場合だけ、
`-ObserverSharedOutputFps 25` を明示して `25 Hz` profile へ切り替える。台数だけを理由に自動では落とさない。

### 3. MADSYSTEM

```powershell
$exe = 'C:\src\MADSYSTEM\.artifacts\unity\Windows\MADSYSTEM.exe'
Start-Process -FilePath $exe -WorkingDirectory (Split-Path -Parent $exe)
```

Native Observer より先に MADSYSTEM を起動すると、一時的に
`CAMERA_DEVICE_NOT_EXIST` が出る。後から shared memory に接続できれば起動失敗ではない。

### 4. Web Observer

運営確認が必要な場合だけ開く。

```text
http://192.168.11.100:8090/observer.html
```

`relayHost` query は別 origin で開発する場合の指定であり、11.100 が配信する本番画面には不要。

## 起動確認

Native Observer process を確認する。

```powershell
Get-CimInstance Win32_Process | Where-Object {
  $_.Name -eq 'momo.exe' -and $_.CommandLine -like '*p2p-recv-multi*'
} | Select-Object ProcessId, CommandLine
```

command line に次が含まれることを確認する。

- `ws://192.168.11.100:8090/ws?role=observer`
- `--shared-frame-name Local\MomoObserverFrameV1`
- `--shared-luma-name Local\MomoObserverLumaV1`
- `--shared-output-fps 50`

MADSYSTEM の接続確認は `Player.log` を使う。

```powershell
$log = Join-Path $env:USERPROFILE `
  'AppData\LocalLow\MADX\MADSYSTEM\Player.log'
Get-Content -LiteralPath $log -Tail 500 | Select-String `
  'MomoObserverSharedFrameSource|MomoRaceControl|CAMERA_DEVICE'
```

正常時は次が出る。

```text
[MomoObserverSharedFrameSource] Connected to 'Local\MomoObserverFrameV1' (1920x1080 BGRA).
[MomoRaceControl] 送信成功: Command ...
```

Web Observer では `DATA WS OPEN`、`RACE WS`、車両ごとの `STREAMING` と FPS を確認する。

## GPU marker worker を使う場合

MADSYSTEM の External marker mode を使う場合だけ、Native Observer の後に追加起動する。

```powershell
Set-Location C:\src\momo
.\tools\Run-GpuMarkerObserverLuma.ps1 `
  -InputMappingName 'Local\MomoObserverLumaV1' `
  -OutputMappingName 'Local\MomoMarkerObservationsV1' `
  -DetectionHz 50 `
  -RequiredSourceCount 4
```

この経路は次の順になる。

```text
Local\MomoObserverLumaV1
  -> GPU marker worker
  -> Local\MomoMarkerObservationsV1
  -> MADSYSTEM External marker reader
```

BGRA composite を読む現行 `legacy` 経路と役割を混同しない。

## 終了

MADSYSTEM を先に終了し、その後 Native Observer を終了する。Web Observer は任意の時点で閉じてよい。
11.100 の Relay と Race Control は別 PC の共用 service なので、この PC から停止しない。
