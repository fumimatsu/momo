# Local Relay Web UI 運用

`web/` は Relay バイナリへ `go:embed` で埋め込まれる。`pilot.html`、`pilot.js`、`ffb-bridge.js`、`gamepad.html`、`gamepad.js`、`gamepad-profile.js` を変更しただけでは、起動済み Relay の UI は変わらない。Relay を再ビルドして再起動した後に反映される。

`web/` は配布コピーである。正本は `momo-fpv-viewer/variants/relay/` にあり、更新時は
`tools/sync-relay-viewer.ps1` を使う。詳細は [Viewer の正本と Relay 配布](../../docs/viewer-integration.md) を参照する。

外部 Pilot を Ayame / TURN 経由で接続する構成は、[Relay 経由 Ayame 外部 Pilot 設計](../../doc/RELAY_AYAME_EXTERNAL_PILOT_DESIGN.md) を参照する。現在は 1 source、1 Pilot の映像・操縦・telemetry・race state を実装している。外部 Pilot の command が 250 ms 途絶えた場合、Relay は対象 Pi へ neutral を送る。

Relay の接続・RTP・下流 Viewer 状態を可視化する Operations 画面の設計は、[Relay Operations Dashboard 設計](../../doc/RELAY_OPERATIONS_DASHBOARD_DESIGN.md) を参照する。

## Operations Dashboard

Relay を再ビルドして起動すると、運営用の読み取り専用画面を配信する。

```text
http://<relay-host>:8090/operations.html
```

`/operations.html` と `/api/v1/status` は既定で loopback からしか開けない。別の運営PCから見る時は、
Relay 起動時に管理 LAN の CIDR を明示する。

```powershell
.\tools\start-mads-observer.ps1 -RebuildRelay -OperationsAllowCidr '192.168.11.0/24'
```

Windows Firewall も同じ管理用サブネットだけに制限する。Relay、Pi、Observer をインターネットへ
公開するための機能ではない。

Relay の Pilot URL は query string を使う。Pi 直結 Momo の静的ファイル配信と違い、hash は使わない。

```text
http://<relay-host>:8090/pilot.html?device=11.4&audioControls=0
```

ハンコンの割り当てと FFB は、同じ Relay origin で次を開いて保存する。`relayPilotPath=flat` は Relay の `web/` がフラットな配布先であることを指定する。

```text
http://<relay-host>:8090/gamepad.html?viewer=relay-pilot&relayPilotPath=flat&device=11.4
```

- `audioControls=0` は Audio、Filter、Mic の音声 UI をすべて隠す。
- `mediaControls=0` は旧名として互換維持する。新規 URL では `audioControls=0` を使う。
- 後退ギア下限は `G1=1200`、`G2=1200`、`G3〜G5=1000`。

## Ayame 外部 Pilot 試験

Pi は従来どおり Local Relay へ P2P 接続する。Relay が Ayame room のもう一方の peer となり、
H.264 RTP を再エンコードせず外部 Viewer へ配信する。`11.3` の direct Ayame モードとは排他である。
外部 Viewer は `momo-command`、`momo-telemetry`、`momo-race` を Relay と接続するため、
ローカル Pilot、Observer、Unity 計測と同時に動作する。

```powershell
.\tools\start-mads-observer.ps1 -RebuildRelay `
  -AyameSignalingUrl 'wss://133.88.123.51.nip.io/signaling' `
  -AyamePilotRoom113 'momo-relay-11-3-ext'
```

外部 Viewer は `relayTransport=1` を指定した Relay Pilot 版を使う。Pi 直結用の `viewer.html` は
`serial` DataChannel を作るため、この URL の代わりに使ってはならない。

```text
https://fumimatsu.github.io/momo-fpv-viewer/variants/relay/pilot.html?signaling=ayame&relayTransport=1&ayameUrl=wss%3A%2F%2F133.88.123.51.nip.io%2Fsignaling&roomId=momo-relay-11-3-ext&clientId=auto&device=11.3&carId=CP-1&deviceStatus=off&autoReconnect=1&videoReconnect=1&iceMode=turn&roomLock=1&audioControls=0
```

`-ayame-pilot-room` を指定している source は Pilot lease を 1 件だけ使用する。既存 Local Pilot と同時に接続できない。
同じ source に Local Pilot と外部 Pilot は同時に接続できない。別 source の Local Pilot、Observer、Unity の接続は維持する。

## Race Control v2

Relay は Race Control の WebSocket を 1 本だけ受信し、各 Pilot Viewer へ reliable な
`momo-race` DataChannel で `race_state v2` を配る。Momo device の WebRTC/DataChannel
へレース状態を送らないため、映像・操縦の経路は変わらない。

固定 4 枠の対応は以下とする。未接続の枠を詰めてはならない。

| Relay device | Observer の位置 | `carId` |
| --- | --- | --- |
| `11.3` | 左上 | `CP-1` |
| `11.4` | 右上 | `CP-2` |
| `11.5` | 左下 | `CP-3` |
| 4 台目 | 右下 | `CP-4` |

`tools/start-mads-observer.ps1` へ Race Control 接続情報を渡す。

```powershell
.\tools\start-mads-observer.ps1 -RebuildRelay `
  -RaceControlUrl 'ws://127.0.0.1:8787/ws/races/race-test' `
  -RaceControlViewerToken '<VIEWER_TOKEN>'
```

`RaceControlUrl` を省略した場合、Race Control 連携は無効のまま Relay と Observer だけを起動する。環境変数 `MOMO_RACE_CONTROL_WS_URL` と `MOMO_RACE_CONTROL_VIEWER_TOKEN` も同じ用途で使える。

Pi 直結の `fpv-viewer.html` は Relay を経由しない。Pilot Browser が Race Control へ直接 WebSocket 接続するため、`raceUrl`、`raceToken`、`carId` を hash に指定する。

```text
http://<momo-device>:8080/html/fpv-viewer.html#raceUrl=ws%3A%2F%2F<race-control-host>%3A8787%2Fws%2Fraces%2Frace-test&raceToken=<VIEWER_TOKEN>&carId=CP-1
```

この直結 URL でも `carId` は固定映像枠 ID を使う。`11.3` のような Relay device ID や Pilot ID を指定してはならない。

## ハンコン表示 UI

`controlUi` で RC 操作 UI を切り替える。

- `controlUi=auto`：既定。ハンコン接続、Drive On、直近 500 ms 以内のハンコン入力が揃った時だけ、スライダーを表示専用 HUD に置き換える。
- `controlUi=manual`：常にスライダー操作 UI を表示する。
- `controlUi=drive`：常に表示専用 HUD を表示する。ハンコン未接続時は `WAITING WHEEL` / `SAFE` と表示する。

表示専用 HUD はハンドル角、アクセル、ブレーキ、現在ギア、Drive 状態を描画する。スライダーと個別ギアボタンは隠すが、Drive 切替・切断・全画面は残す。ハンコンのパドルでギアを変更する。

Pi 直結 UI は別配布物である。実ファイル名は `fpv-viewer.html` / `fpv-viewer.js`、URL は `#audioControls=0` のように hash を使う。Relay の `pilot.html` と混同してはならない。
