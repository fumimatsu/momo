# Local Relay Web UI 運用

## Race Control / Relay / Observer の一括起動

同一PCの3サービスは、次のスクリプトで依存順に起動できる。Race Control の
`D:\src\momo-race-control\.dev.vars` から `VIEWER_TOKEN` を読み、Relay の購読にだけ渡す。
トークン値は画面やログへ表示しない。

```powershell
.\tools\start-mads-stack.ps1
```

すでに正常起動しているサービスは維持する。Relay の埋込みWeb資産が実行ファイルより新しい場合だけ
Relay を再ビルドし、Race Control 連携なしで起動中の場合だけ再起動する。管理画面も開く場合は
`-OpenAdmin` を付ける。

別 PC への導入、リポジトリの責任範囲、Race Control / Observer / FFB Bridge を含む配置手順は
[Momo tools の配置と別 PC 導入](../README.md) を参照する。

`web/` は Relay バイナリへ `go:embed` で埋め込まれる。Pilot、Gamepad、Web Observer の
ファイルを変更しただけでは、起動済み Relay の UI は変わらない。Relay を再ビルドして
再起動した後に反映される。

`web/` は配布コピーである。正本は `momo-fpv-viewer/variants/relay/` と
`momo-fpv-viewer/variants/observer/` にあり、更新時は
`tools/sync-relay-viewer.ps1` を使う。詳細は [Viewer の正本と Relay 配布](../../docs/viewer-integration.md) を参照する。

外部 Pilot を Ayame / TURN 経由で接続する構成は、[Relay 経由 Ayame 外部 Pilot 設計](../../doc/RELAY_AYAME_EXTERNAL_PILOT_DESIGN.md) を参照する。現在は 1 source、1 Pilot の映像・操縦・telemetry・race state を実装している。外部 Pilot の command が 250 ms 途絶えた場合、Relay は対象 Pi へ neutral を送る。

Relay の接続・RTP・下流 Viewer 状態を可視化する Operations 画面の設計は、[Relay Operations Dashboard 設計](../../doc/RELAY_OPERATIONS_DASHBOARD_DESIGN.md) を参照する。

特定の Pilot PC で M5 音声のバイナリ DataChannel は受信できる一方、文字列 DataChannel が
受信できない場合の比較プローブと判定手順は、
[Relay DataChannel text / binary diagnostic](../../doc/RELAY_DATACHANNEL_TEXT_DIAGNOSTIC.md) を参照する。

## 車体テレメトリ記録

Relayは各`-source`の上流Momoから受信した`TEL:` text messageを、全車共通のRelay時計で
1本のNDJSONへ記録できる。Relay Pilotが信頼性ありの`momo-drive` channelで`DRIVE:1`を送った
sourceだけを記録し、`DRIVE:0`、command/drive channel切断、Pilot切断で直ちに止める。Viewerの
接続有無に依存しないため、車体座標、重力除去、軸符号を走行後に比較するための正本ログとして使う。Race Control接続時は、同じファイルに
`race_state`、`raceRunId`、phase、flag、sequenceも記録する。

記録は明示指定時だけ有効にする。既定では無効で、容量を消費しない。

```powershell
.\tools\start-mads-observer.ps1 -RebuildRelay `
  -TelemetryLogDirectory 'E:\fpv-telemetry-logs'
```

環境変数`MOMO_RELAY_TELEMETRY_LOG_DIR`でも同じ保存先を指定できる。Relay単体では
`-telemetry-log-dir <directory>`を使う。

出力は`telemetry-<relay-session>.ndjson`で、先頭に`relay_session`、各車の`drive_state`と`telemetry`、
Race Controlを受信した場合の`race_state`、正常終了時の`relay_session_end`を時系列で入れる。
`telemetry`にはRelay受信UTC時刻、Relay開始からの単調経過時間、`sourceId`、`carId`、
上流接続generation、`TEL:`全文を含める。DataChannelはunreliableなため、ログはRelayへ届いた
sampleだけを表す。M5の`boot`と`seq`から欠損を検出する。

記録キューは有限で、満杯時はログsampleをdropして終了レコードの`queueDrops`へ数える。ファイルI/Oが
映像、RC command、Telemetry中継を待たせることはない。4台を30Hz、1 message最大256 bytesで送る場合、
wire上の生データ量は約111MB/時間であり、NDJSONのメタデータ込みでは約150MB/時間を見込む。Relayの強制終了時は
終了レコードが無いことがあるが、1秒ごとにflushするため最後の完全なNDJSON行までは解析できる。

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

## LAN Pilot 車両選択

別 PC の Pilot は Relay が配信する `garage.html` を開き、映像が到着している未使用車体を
カードから選択する。選択後は同じブラウザで対象の `pilot.html` へ遷移する。Relay 直結の
同一 origin で動くため、CORS 設定は不要である。

```text
http://<relay-host>:8090/garage.html
```

Garage は `GET /api/v1/pilot-devices` だけを参照する。運営用 `/api/v1/status` の復旧情報や
DataChannel 診断を Pilot PC へ公開しない。

既定の Relay バイナリでは Garage は loopback のみ許可する。`start-mads-observer.ps1` は
`192.168.11.0/24` を既定値として渡す。別の LAN を使う場合は明示指定する。

```powershell
.\tools\start-mads-observer.ps1 -RebuildRelay -GarageAllowCidr '192.168.11.0/24'
```

`READY` だけ選択可能とする。接続済みまたは接続交渉中の Pilot がいる車両は `IN USE` と表示し、
選択できない。これは表示と Relay の既存 Pilot lease の両方で二重に防ぐ。

Relay の Pilot URL は query string を使う。Pi 直結 Momo の静的ファイル配信と違い、hash は使わない。
LAN Pilot は `momo-command` と `momo-drive` DataChannel を操縦上り専用に使い、race state、
telemetry、vehicle event、M5 音声は Relay signaling WebSocket で受信する。

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

## Web Observer

本番 Web Observer は Relay と同一 origin から配信する。

```text
http://<relay-host>:8090/observer.html
```

ページは query がなければ `location.host` を接続先 Relay として使い、4 台分の read-only
Observer WebRTC session を映像専用で開く。Race state、telemetry、command監査、vehicle eventは
各sourceのsignaling WebSocketで受信する。ブラウザへ Race Control token は渡さない。Relay が
Race Control へ1本だけ認証接続し、各Web Observer sessionへ必要な下りデータを配る。

`observer-config.json` の `device` と `carId` は Relay 起動時の `-source`、`-race-car` と
一致させる。設定または Viewer を変更した場合は `sync-relay-viewer.ps1`、Relay の再ビルド、
再起動、ブラウザの強制再読み込みまで実施する。

## Ayame 外部 Pilot 試験

Pi は従来どおり Local Relay へ P2P 接続する。Relay が Ayame room のもう一方の peer となり、
H.264 RTP を再エンコードせず外部 Viewer へ配信する。`11.3` の direct Ayame モードとは排他である。
外部 Viewer は `momo-command`、`momo-drive`、`momo-telemetry`、`momo-race`、`momo-events` を
WebRTC DataChannel で Relay と接続する。Ayame signaling WebSocket は Relay の下りデータを運ばないため、
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

Relay は Race Control の WebSocket を 1 本だけ受信し、LAN Pilot へ signaling WebSocket、
Ayame 外部 Pilot へ reliable な `momo-race` DataChannel で `race_state v2` を配る。Momo device の WebRTC/DataChannel
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

## 車体HPとピット回復

車体HP、衝突ダメージ、前進スロットル上限はRelayの `vehicle_health.go` が正本である。
通常運用ではM5/Piから届くV2 `impact_candidate`だけを公式判定へ使用する。V1 stateは
RAW診断とsynthetic testの互換用に残すが、V1 `impact`はHPを変更せず、
`legacy_event_unsupported`としてログへ記録する。

衝撃段階は`weak >= 10 m/s2`、`strong >= 12 m/s2 && jerk >= 250 m/s3`、
`severe >= 18 m/s2 && jerk >= 250 m/s3`である。strongは12 HP、severeは20 HPを減算し、
damage cooldownは600 msとする。同じ`carId:boot:sequence`の再送は重複として無視する。
HPダメージはPracticeを含む有効なレースセッション中だけ適用する。Race Control接続中、
raceRunIdあり、phaseが`green`、最終race state受信から5秒以内の条件を満たさない衝撃は、
`race_inactive`としてイベントへ記録するがHPと回復待ち時間を変更しない。

RelayはHP更新と同じ判定結果をReliable/Orderedな`momo-events` DataChannelへ配信する。
各sourceはレース単位で直近32件を保持し、channel open時に`vehicle_event_snapshot`を送る。
snapshotは履歴復元専用で、ViewerはFFB、点滅、音声、HP計算を再実行しない。live eventの
配信は64件の有界専用queueへ分離し、詰まりがTelemetry受信や操縦転送を待たせない。
詳細契約はViewer正本の`docs/authoritative-vehicle-events-implementation-plan.md`を参照する。

MADSYSTEMが同じピット用ArUco markerを連続認識し、1秒ごとにRelayへtickを送る。
Relayは有効なtick 1回につき10 HPと10 Fuelを同じlock内で回復する。API契約は
[Relay Pit Recovery Tick API](../../doc/PIT_RECOVERY_API.md)、責務分担は
[ピットレーン・ダメージ回復 設計検討](../../doc/PIT_LANE_DAMAGE_RECOVERY_DESIGN.md) を参照する。

回復モードは次の4種類である。既定は走行回復と PIT 回復を併用する `hybrid` とする。

| mode | 動作 |
| --- | --- |
| `legacy` | 安全時間経過後、前進指令中に従来の連続回復を行う |
| `pit-marker` | `green` 中にMADSYSTEMのtickを受理した時だけ10 HP回復する |
| `hybrid` | `legacy` の走行回復と `pit-marker` の10 HP回復を両方行う |
| `disabled` | 回復しない |

`pit-marker` と `hybrid` ではRace Control接続とgameplay tokenが必須である。tokenは引数へ入れず環境変数で渡す。

MADSYSTEM は HP 回復を `/api/v1/gameplay/pit-recovery-ticks`、PIT IN / OUT を
`/api/v1/gameplay/pit-presence-events` へ送る。Relay は presence と回復状態を
`PIT:1` として telemetry DataChannel へ配信する。presence 未対応の旧 client からの
回復 tick も受理するが、tick だけから PIT IN / OUT は推測しない。`serviceState=complete` は
HPとFuelがともに100の時だけ表示する。途中でPIT OUTしても回復済みの値は維持する。詳細は
[Relay PIT Gameplay API](../../doc/PIT_RECOVERY_API.md) を参照する。

```powershell
$env:MOMO_RELAY_GAMEPLAY_TOKEN = '<GAMEPLAY_TOKEN>'
.\tools\start-mads-observer.ps1 -RebuildRelay `
  -HealthRecoveryMode 'hybrid' `
  -FuelDriveDurationSeconds 120 `
  -RaceControlUrl 'ws://127.0.0.1:8787/ws/races/race-test' `
  -RaceControlViewerToken '<VIEWER_TOKEN>'
```

Fuelは`green`中、Race Control状態が新しく、Drive ONで前進指令が継続している間だけ減る。
既定は合計120秒の有効前進で100から0になり、`-fuel-drive-duration`で変更できる。
Fuel 0でもPITへ戻れるよう前進PWMを1速上限より10低い1590へ制限し、完全停止にはしない。

通常ギア上限はG3である。前進中に溜まるBoostが100になると、G3から右パドルで2.5秒だけG4を起動できる。
充填時間は4台時にP1=40秒、P2=34秒、P3=28秒、P4=22秒で、順位不明時は30秒とする。
`GEAR:4`の直接指定は拒否し、G4終了時はRelayがG3へ戻す。

Relayは旧client向けの`VHS:1`を維持し、HP、Fuel、Boost、実効gearをJSONの`VGS:1`でも配信する。

APIは既定でloopbackだけを許可する。MADSYSTEMを別PCで動かす場合は `-GameplayAllowCidr` を明示できるが、
Relay自身はHTTPのTLSを終端しない。平文tokenを信頼できないネットワークへ流してはならない。

Pi 直結 UI は別配布物である。実ファイル名は `fpv-viewer.html` / `fpv-viewer.js`、URL は `#audioControls=0` のように hash を使う。Relay の `pilot.html` と混同してはならない。
