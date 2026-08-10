# Momo tools の配置と別 PC 導入

このディレクトリには、車体上の Momo を複数台まとめる Relay、Native Observer、
Native Pilot、および Viewer 配布同期用の運用ツールを置く。

Relay を別 PC へ移す場合、実行ファイルだけを手作業でコピーして正本にしてはならない。
`fumimatsu/momo` の `master` を取得し、その PC で Relay を build する。Relay の `.exe`、
Momo の `_build/`、log、crash dump は Git 管理外であるため、clone だけでは実行環境は完成しない。

## リポジトリの責任範囲

| リポジトリ | 主な責任 | Relay PC で必要になる条件 |
| --- | --- | --- |
| `https://github.com/fumimatsu/momo.git` | Relay、Native Observer、Native Pilot、起動スクリプト | Relay では必須 |
| `https://github.com/fumimatsu/momo-race-control.git` | Race Control Worker、管理画面、計測 API | Race Control も同じ PC へ移す場合 |
| `https://github.com/fumimatsu/momo-fpv-viewer.git` | Relay Viewer の正本、Gamepad 設定、FFB Bridge | Viewer 更新または Pilot PC の FFB 導入時 |
| `https://github.com/fumimatsu/momo-fpv.git` | Raspberry Pi 運用、M5StickS3 firmware、実機記録 | 車体更新や現地運用を行う場合 |

Relay の実装は `momo/tools/momo-relay/` にある。`momo-fpv` や
`momo-fpv-viewer` から Relay 本体を build しない。

## 推奨配置

```text
Raspberry Pi / Momo P2P
  11.3 :8080 ----+
  11.4 :8080 ----+
  11.5 :8080 ----+--> Relay PC :8090 --> Local Pilot browser
  11.6 :8080 ----+          |          --> Native Observer
                             |          --> Ayame external Pilot (optional)
                             |
Race Control :8787 ----------+

Pilot PC
  Browser --------------------------> Relay PC :8090
  FFB Bridge ws://127.0.0.1:24725 <-- Browser on the same Pilot PC
```

車体側 Momo は Local P2P のままにする。外部 Pilot が必要な場合も、車体を Ayame へ
切り替えるのではなく、Relay に車体別 Ayame room を追加する。

## Relay PC の準備

Windows では次を用意する。

- Git
- PowerShell 7
- `tools/momo-relay/go.mod` が指定する Go toolchain
- 車体 LAN への接続
- Relay の `8090/tcp` を Private network から受ける Windows Firewall rule

Native Observer も動かす場合は、Windows 版 Momo の build toolchain も必要になる。

```powershell
git clone -b master https://github.com/fumimatsu/momo.git C:\src\momo
Set-Location C:\src\momo
git status --short --branch
```

## Relay 単体の build と起動

Relay だけを動かす場合、Momo 本体の build は不要である。

```powershell
Set-Location C:\src\momo\tools\momo-relay
go build -trimpath -o momo-local-relay.exe .
```

Race Control を使わない最小の 4 台構成は次のとおり。

```powershell
& .\momo-local-relay.exe `
  -listen ':8090' `
  -source '11.3=ws://192.168.11.3:8080/ws' `
  -source '11.4=ws://192.168.11.4:8080/ws' `
  -source '11.5=ws://192.168.11.5:8080/ws' `
  -source '11.6=ws://192.168.11.6:8080/ws' `
  -operations-allow-cidr '127.0.0.1/32' `
  -garage-allow-cidr '127.0.0.1/32' `
  -garage-allow-cidr '192.168.11.0/24'
```

`-source` の指定順が Relay と Observer の固定枠順になる。Race Control を使う場合は、
同じ順序で `-race-car` を指定し、device と `carId` を一致させる。

```powershell
& .\momo-local-relay.exe `
  -listen ':8090' `
  -source '11.5=ws://192.168.11.5:8080/ws' `
  -source '11.6=ws://192.168.11.6:8080/ws' `
  -source '11.3=ws://192.168.11.3:8080/ws' `
  -source '11.4=ws://192.168.11.4:8080/ws' `
  -race-car '11.5=CP-1' `
  -race-car '11.6=CP-2' `
  -race-car '11.3=CP-3' `
  -race-car '11.4=CP-4' `
  -race-url 'ws://<race-control-host>:8787/ws/races/race-test' `
  -race-viewer-token '<VIEWER_TOKEN>' `
  -operations-allow-cidr '127.0.0.1/32' `
  -garage-allow-cidr '127.0.0.1/32' `
  -garage-allow-cidr '192.168.11.0/24' `
  -telemetry-log-dir 'C:\fpv-telemetry-logs'
```

token を command history、log、Git 管理ファイルへ残さない。常設運用では環境変数または
アクセス制限したローカル設定から起動スクリプトへ渡す。

## Relay と Native Observer の同居

Native Observer は次の Windows build artifact を使う。

```text
C:\src\momo\_build\windows_x86_64\release\momo\Release\momo.exe
```

artifact がなければ、リポジトリ root で build する。

```powershell
Set-Location C:\src\momo
python3 run.py build windows_x86_64
```

Relay を起動スクリプトが期待する名前で build する。

```powershell
Set-Location C:\src\momo\tools\momo-relay
go build -trimpath -o momo-local-relay-device-input-v15.exe .
```

Race Control が同じ PC にある場合は `127.0.0.1` を使う。

```powershell
Set-Location C:\src\momo
.\tools\start-mads-observer.ps1 `
  -RaceControlUrl 'ws://127.0.0.1:8787/ws/races/race-test' `
  -RaceControlViewerToken '<VIEWER_TOKEN>'
```

fresh clone には `tools/momo-relay/.toolchain/` がない。`-RebuildRelay` はこの bundled Go を
参照するため、system Go で上記の手動 build を行った場合は `-RebuildRelay` を付けない。

## Race Control の配置

### Relay と同じ PC

```text
ws://127.0.0.1:8787/ws/races/race-test
```

### LAN 上の別 PC

```text
ws://<race-control-pc-ip>:8787/ws/races/race-test
```

Race Control を `0.0.0.0:8787` で待ち受けさせ、`8787/tcp` は管理 LAN だけに許可する。
`.dev.vars` の `VIEWER_TOKEN` を Relay に渡す。`ADMIN_TOKEN`、
`TIMING_INGEST_TOKEN`、`RACE_CONTROL_TOKEN` を Relay に渡してはならない。

### Cloudflare Workers

```text
wss://<worker-host>/ws/races/race-test
```

Relay から Worker へ接続できる Internet 経路が必要になる。車体 P2P と Race Control は
別経路であり、Worker 利用のために車体 Momo を Ayame へ変更する必要はない。

## Viewer と FFB Bridge

Relay に埋め込む `web/` は配布コピーで、正本は `momo-fpv-viewer` にある。Viewer を更新したら
同期してから Relay を再 build する。

```powershell
Set-Location C:\src\momo
.\tools\sync-relay-viewer.ps1
Set-Location .\tools\momo-relay
go build -trimpath -o momo-local-relay.exe .
```

FFB Bridge は Relay PC ではなく、ハンコンを接続した Pilot PC で動かす。Bridge URL は常に
次の loopback URL を使う。

```text
ws://127.0.0.1:24725
```

Relay の IP または hostname が変わった場合、Pilot PC の Bridge GUI にある
`Relay endpoint` とブラウザの origin を同じ値へ更新する。ブラウザの localStorage は origin ごとに
分かれるため、`gamepad.html` で Input / FFB 設定を保存し直し、Viewer を再読み込みする。
`24725/tcp` を LAN に公開してはならない。

## Firewall

| Port | 所有プロセス | 許可範囲 |
| --- | --- | --- |
| `8090/tcp` | Relay | Pilot / 運営用 Private LAN |
| `8787/tcp` | Race Control | 同一 PC または管理 LAN。別 PC 配置時だけ LAN 許可 |
| `24725/tcp` | FFB Bridge | Pilot PC の loopback のみ |

Relay の Operations API は `-operations-allow-cidr`、Garage は
`-garage-allow-cidr` でも制限する。Windows Firewall だけに依存しない。

## 起動後の確認

Relay PC で確認する。

```powershell
Invoke-RestMethod http://127.0.0.1:8090/api/v1/status |
  ConvertTo-Json -Depth 8
```

確認項目は次のとおり。

- source の順序と `raceCarId` が当日の枠割りと一致する。
- 起動済み車両が `STREAMING` になる。
- `ingressAccessUnitFps` が想定 FPS で増加する。
- `serialOpen` が `true` になる。
- Native Observer 起動時は `connectedObservers` が `1` になる。
- Pilot 接続時は `raceChannelsOpen` が `1` になる。
- Race Control の log に WebSocket `101 Switching Protocols` が出る。

ブラウザで開く画面は次のとおり。

```text
http://<relay-host>:8090/operations.html
http://<relay-host>:8090/garage.html
http://<relay-host>:8090/observer.html
http://<relay-host>:8090/gamepad.html?viewer=relay-pilot&relayPilotPath=flat&device=11.5
http://<relay-host>:8090/pilot.html?device=11.5&audioControls=0
```

Web Observer は Relay と同一 origin から配信する。別の Web サーバーや `relayHost` query は
開発時だけに使い、本番 URL として固定しない。

## 更新手順

実行中プロセスを入れ替える前に、未コミット変更と remote 差分を確認する。

```powershell
Set-Location C:\src\momo
git status --short --branch
git fetch --all --prune
git pull --ff-only origin master
```

source、`go.mod`、`go.sum`、または `tools/momo-relay/web/` が変わった場合は Relay を再 build する。
Native Observer の変更を含む場合は Windows 版 Momo も再 build する。起動済み binary を
上書きして終わりにせず、Relay と Observer を再起動して Operations API で接続状態を確認する。

PIT marker による HP 回復を有効にする場合は、token、CIDR、MADSYSTEM 設定、実ダメージ試験、
rollback を含む [PIT 回復機能 本番適用 Runbook](../doc/PIT_RECOVERY_PRODUCTION_ROLLOUT.md) に従う。

## 別 PC 移行チェックリスト

1. `fumimatsu/momo` の `master` を clone する。
2. Relay を移行先 PC で build する。
3. Observer が必要なら Windows 版 Momo を build する。
4. 車体 LAN で各 `192.168.11.x:8080` に到達できることを確認する。
5. Race Control の配置に合わせて `-race-url` を決める。
6. `VIEWER_TOKEN` だけを Relay に渡す。
7. `8090/tcp` の Firewall を Private LAN に限定する。
8. source 順と `carId` を当日の枠割りに合わせる。
9. Operations API で映像、serial、Observer、race channel を確認する。
10. `http://<relay-host>:8090/observer.html` で 4 枠、race state、telemetry を確認する。
11. Pilot PC の Bridge GUI で新しい Relay origin を許可し、Input / FFB 設定を保存し直す。

Relay の機能、画面、telemetry 記録、Ayame 外部 Pilot の詳細は
[momo-relay/README.md](momo-relay/README.md) を参照する。
