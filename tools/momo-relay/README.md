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
同期対象ファイルは Viewer 正本の `tools/distribution-targets.json` にある `relay-web` target から読み込む。

外部 Pilot を Ayame / TURN 経由で接続する構成は、[Relay 経由 Ayame 外部 Pilot 設計](../../doc/RELAY_AYAME_EXTERNAL_PILOT_DESIGN.md) を参照する。現在は 1 source、1 Pilot の映像・操縦・telemetry・race state を実装している。外部 Pilot の command が 250 ms 途絶えた場合、Relay は対象 Pi へ neutral を送る。

Relay の接続・RTP・下流 Viewer 状態を可視化する Operations 画面の設計は、[Relay Operations Dashboard 設計](../../doc/RELAY_OPERATIONS_DASHBOARD_DESIGN.md) を参照する。

特定の Pilot PC で M5 音声のバイナリ DataChannel は受信できる一方、文字列 DataChannel が
受信できない場合の比較プローブと判定手順は、
[Relay DataChannel text / binary diagnostic](../../doc/RELAY_DATACHANNEL_TEXT_DIAGNOSTIC.md) を参照する。

## 車体テレメトリ記録

Relayは各`-source`の上流Momoから受信した`TEL:` text messageとPilotの走行入力を、全車共通のRelay時計で
1本のNDJSONへ記録できる。Relay Pilotが信頼性ありの`momo-drive` channelで`DRIVE:1`を送った
sourceだけを記録し、`DRIVE:0`、command/drive channel切断、Pilot切断で直ちに止める。走行入力は50Hzの
操縦経路を待たせないよう10Hzを上限に`drive_input`として保存する。各sampleにはsteering、要求・制限後の
power PWM、throttle、brake、gear、HP、Fuel、Boost、順位、Fuel消費率、アクセル変動量を含める。Viewerの
接続有無に依存しないため、車体座標、重力除去、軸符号を走行後に比較するための正本ログとして使う。Race Control接続時は、同じファイルに
`race_state`、`raceRunId`、phase、flag、sequenceに加え、Relayが確定した`vehicle_event`も記録する。
`vehicle_event`には衝撃クラス、強度、jerk、軸、ダメージ適用結果、抑制理由、適用前後HPを含む。

記録は明示指定時だけ有効にする。既定では無効で、容量を消費しない。

```powershell
.\tools\start-mads-observer.ps1 -RebuildRelay `
  -TelemetryLogDirectory 'E:\fpv-telemetry-logs'
```

環境変数`MOMO_RELAY_TELEMETRY_LOG_DIR`でも同じ保存先を指定できる。Relay単体では
`-telemetry-log-dir <directory>`を使う。既定では2時間ごとに整理可否を確認し、Race ControlがGreenでなく、
かつ前進走行中の車両がない場合だけ24時間より古い`telemetry-*.ndjson`を削除する。書き込み中のログは
常に除外し、安全条件を満たさない回は次の確認まで延期する。保持期間は`-telemetry-log-retention 48h`のように
変更でき、`0`で自動削除を無効にできる。

出力は`telemetry-<relay-session>.ndjson`で、先頭に`relay_session`、各車の`drive_state`、`drive_input`、`telemetry`、`vehicle_event`、
Race Controlを受信した場合の`race_state`、正常終了時の`relay_session_end`を時系列で入れる。
`telemetry`にはRelay受信UTC時刻、Relay開始からの単調経過時間、`sourceId`、`carId`、
上流接続generation、`TEL:`全文を含める。DataChannelはunreliableなため、ログはRelayへ届いた
sampleだけを表す。M5の`boot`と`seq`から欠損を検出する。

記録キューは有限で、満杯時はログsampleをdropして終了レコードの`queueDrops`へ数える。ファイルI/Oが
映像、RC command、Telemetry中継を待たせることはない。4台を30Hz、1 message最大256 bytesで送る場合、
wire上の生データ量は約111MB/時間であり、NDJSONのメタデータ込みでは約150MB/時間を見込む。4台が同時走行する
最悪条件では`drive_input`が約95MB/時間加わる。Relayの強制終了時は
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

API version 2ではsourceごとの集計に加え、接続clientごとのremote host、Pilot/Observer、
web/native client、WebSocket/DataChannel、最終Telemetry送出age、drop/errorを表示する。
送信先port、token、payloadは返さない。

## Relay source設定ファイル

台数が増えた運用では、繰り返しの`-source`、`-race-car`、`-ayame-pilot-room`を
JSONへまとめられる。雛形は`relay-config.example.json`である。

```powershell
./momo-relay.exe -config ./relay-config.json `
  -operations-allow-cidr 192.168.11.0/24

../start-mads-observer.ps1 -RelayConfigPath ./momo-relay/relay-config.json -RebuildRelay
```

`version`は現在`1`、有効source数は`1..32`である。未知の項目、重複source ID、重複car ID、
`ws://`/`wss://`以外のURLは起動時に拒否する。`enabled: false`で予備sourceを設定に残せる。
`-config`は`-upstream`、`-source`、`-race-car`、`-ayame-pilot-room`と併用しない。

## Relay負荷測定

起動中RelayのStatus APIとRelayプロセスを1秒間隔で採取し、合否と原票を保存する。

```powershell
../Measure-RelayScale.ps1 -ExpectedSources 4 -DurationSeconds 600
../Measure-RelayScale.ps1 -ExpectedSources 8 -DurationSeconds 600
../Measure-RelayScale.ps1 -ExpectedSources 12 -DurationSeconds 600
../Measure-RelayScale.ps1 -ExpectedSources 16 -DurationSeconds 600
```

結果は既定で`tools/.artifacts/relay-scale/<timestamp>-<count>-sources/`の`summary.json`と
`samples.csv`へ保存する。別PCのRelayを測る場合は`-RelayUrl`を指定する。その場合、Relay PCの
CPU/メモリは取得できないため、`-ProcessId`はRelayと同じPCで実行する時だけ有効である。
暫定閾値と更新方針は
[Scalable Marker Observer and Program Observer Design](../../doc/SCALABLE_MARKER_AND_PROGRAM_OBSERVER_DESIGN.md)を参照する。

擬似Momoとread-only Viewerを同時起動して4から32台を比較する場合はmatrixを使う。`PilotSource`を
指定すると1台だけ50 Hzのcommandとdrive channelを追加し、`RecoverySource`を指定すると対象Momoを
1回切断して、既存Viewerまでの復旧と他sourceの継続を判定する。

```powershell
../Invoke-RelayScaleMatrix.ps1 `
  -SourceCounts 4,8,12,16,24,32 `
  -DurationSeconds 600 -WarmupSeconds 30 `
  -ObserversPerSource 1 -PilotSource sim-01

../Invoke-RelayScaleMatrix.ps1 `
  -SourceCounts 16 -DurationSeconds 600 `
  -ObserversPerSource 1 -PilotSource sim-01 -RecoverySource sim-08
```

成果物は`tools/.artifacts/relay-scale-matrix/`へ保存する。擬似H.264 RTPは中継負荷用であり、
復号結果や画質を検証する入力ではない。

## ArUco capacity測定

実走録画を複数sourceとして実時間再生し、描画なしでH.264復号とArUco検出の上限を測る。
標準検出周期は25 Hzで、50 FPS入力の2フレームに1回を処理する。

```powershell
../Initialize-ArucoCapacity.ps1
../Prepare-ArucoCapacityInput.ps1 `
  -InputPath D:\recordings\cpu-shadow.webm -RotateDegrees 180
../Invoke-ArucoCapacitySuite.ps1 `
  -InputPath ..\.artifacts\aruco-input\cpu-shadow-upright-h264.mp4 `
  -SourceCounts 1,2,4,6,8,10,12,16 -DurationSeconds 60
```

suiteは`opencv`、`qsv`、`cuda`を順に測定し、PC構成とdriverも同じ成果物へ保存する。
個別測定の`Decoder`には、任意依存のPyNvVideoCodecを使う直接NVDEC経路`nvcodec`も指定できる。
`nvcodec`を使うnodeは`Initialize-ArucoCapacity.ps1 -IncludeNvCodec`で初期化する。`qsv`と`cuda`の
hardware経路では対応FFmpegが必要である。
別PCへ導入する場合は
[Direct NVDEC ArUco Validation Guide](../../doc/DIRECT_NVDEC_ARUCO_VALIDATION_GUIDE.md)に従い、
GPU/driver確認、同一入力hash、smoke、capacity、parity、soakの順で実施する。
合否は各sourceの出力FPS、検出FPS、検出latency p95、process tree CPU p95で判定する。
本番上限の決定には10分以上、最終確認には1時間を使う。別PCでの完全な手順は
[Scale Validation Runbook](../../doc/SCALE_VALIDATION_RUNBOOK.md)、結果の読み方と推奨配置は
[Scalable Marker Observer and Program Observer Design](../../doc/SCALABLE_MARKER_AND_PROGRAM_OBSERVER_DESIGN.md)を参照する。
50 FPS入力の全フレームを認識する比較試験は、suiteへ`-DetectionHz 50`を指定する。入力・検出47.5 FPS、
検出latency p95 20 ms以下が自動的に合格条件となる。
CPU基準と直接NVDECを同じframe indexで比較する場合は、次を実行する。レース運用対象IDの
一致率と、未知IDを含む完全一致率を分けて記録する。

```powershell
../Compare-ArucoBackends.ps1 `
  -InputPath ../.artifacts/aruco-input/cpu-shadow-upright-h264.mp4 -FrameCount 1500
```

direct NVDECの短時間上限と運用候補を分ける。短時間でCPU 60%直前まで通る台数をそのまま採用せず、
20%以上のCPU余力を残す候補台数で10分、次に1時間を通してからnode設定へ反映する。

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
Observer WebRTC session を映像専用で開く。telemetry、command 監査、vehicle event は各 source の
signaling WebSocket、全車共通の Race state は専用の `/ws/race-state` 1 本で受信する。
ブラウザへ Race Control token は渡さない。Relay が Race Control へ 1 本だけ認証接続し、
Web Observer へ Race state を重複させず配る。

映像接続を静的に絞る場合は`videoDevices`へRelay deviceをカンマ区切りで指定する。省略時は
従来どおり全台へ接続する。

```text
http://<relay-host>:8090/observer.html?videoDevices=11.3,11.5
```

これはProgram Observerへ向けた初期基盤であり、選択外sourceのWebRTCとsource別signalingを
作らない。そのため選択外sourceの個別Telemetryとvehicle eventも現在は受信しない。Race stateは
全車共通WebSocketから受信する。全車の運用監視が必要な画面では、現段階では指定なしを使う。

`raceFallback=http` は障害診断用の明示設定として維持する。指定しても Race WebSocket が
正常な間は HTTP polling を止め、WebSocket 切断中だけ 500 ms 間隔で最新状態を補完する。

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

Relay backend の signaling key は Viewer URL へ入れず、Relay 起動前に環境変数へ設定する。

```powershell
$env:MOMO_AYAME_SIGNALING_KEY = '<backend-only-random-key>'
```

Public Pilot は VPS authn service が発行した短期 `pilotTicket` を使う。ticket は room と Pilot role に
限定され、初回認証で消費される。発行手順は `momo-fpv/docs/ayame-vps-turn.md` を参照する。

```text
https://<public-pages>/pilot.html?signaling=ayame&relayTransport=1&ayameUrl=wss%3A%2F%2F133.88.123.51.nip.io%2Fsignaling&roomId=momo-relay-11-3-ext&clientId=auto&device=11.3&carId=CP-1&deviceStatus=off&autoReconnect=1&videoReconnect=1&iceMode=turn&roomLock=1&audioControls=0#pilotTicket=<short-lived-ticket>
```

`pilotTicket` は query string ではなく URL fragment に置く。これにより Pages と HTTP access log へ
ticket を送らず、Viewer は Ayame accept 後に address bar から ticket を削除する。

`-ayame-pilot-room` を指定している source は Pilot lease を 1 件だけ使用する。既存 Local Pilot と同時に接続できない。
同じ source に Local Pilot と外部 Pilot は同時に接続できない。別 source の Local Pilot、Observer、Unity の接続は維持する。

## Race Control v2

Relay は Race Control の WebSocket を 1 本だけ受信し、LAN Pilot へ signaling WebSocket、
Ayame 外部 Pilot へ reliable な `momo-race` DataChannel で `race_state v2` を配る。Momo device の WebRTC/DataChannel
へレース状態を送らないため、映像・操縦の経路は変わらない。
Race DataChannel は接続時に最新スナップショットを送り、以後は状態更新時だけ送る。周期再送は行わない。

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
raceRunIdあり、phaseが`green`の条件を満たさない衝撃は`race_inactive`としてイベントへ記録し、
HPと回復待ち時間を変更しない。phaseが`green`以外へ変化した時、またはRace Control切断時は
残っているダメージを即時回復する。Boost有効中の衝撃も`boost_active`としてHPを変更しない。

RelayはHP更新と同じ判定結果をReliable/Orderedな`momo-events` DataChannelへ配信する。
各sourceはレース単位で直近32件を保持し、channel open時に`vehicle_event_snapshot`を送る。
snapshotは履歴復元専用で、ViewerはFFB、点滅、音声、HP計算を再実行しない。live eventの
配信は64件の有界専用queueへ分離し、詰まりがTelemetry受信や操縦転送を待たせない。
詳細契約はViewer正本の`docs/authoritative-vehicle-events-implementation-plan.md`を参照する。

`momo-events`の自動E2Eは、ローカルRelayのWebSocket signalingへ実Pion PeerConnectionを接続し、
Reliable/Ordered channelのopen、接続前履歴を含む初回snapshot、V2 `impact_candidate`から確定live eventとHP減算、
同一event再送の重複抑止を検証する。同時に元の`TEL:`が非信頼`momo-telemetry`へ届き、
`momo-command`がopenのまま維持されることも確認する。これは車載Momoの`serial` DataChannelではなく、
Relayから外部Viewerへ配る確定イベント区間の試験である。

```powershell
.\tools\Invoke-RelayTests.ps1 -Run '^TestMomoEventsDataChannelEndToEnd$'
.\tools\Invoke-RelayTests.ps1
# CGO_ENABLED=1とC compilerがある環境、またはLinux CIで実行
.\tools\Invoke-RelayTests.ps1 -Race
```

`Invoke-RelayTests.ps1`は`PATH`だけに依存せず、`MOMO_GO_EXE`、repository内toolchain、
Codex toolchain、Scoop、標準installer、registry、`MOMO_TOOLCHAIN_ROOTS`の順にGoを探索する。
直接`go test`を実行して見つからない場合も、未導入と判断する前にこのwrapperを使う。

GitHub ActionsはWindowsで通常の全試験、Linuxで`go test -race ./...`を実行する。

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

Fuelは`green`中、Race Control接続が有効で、Drive ONかつ前進指令が継続している間だけ減る。
一定入力は従来どおり合計120秒の有効前進で100から0になり、`-fuel-drive-duration`で変更できる。
アクセルの上げ下げは約1.5秒の移動平均で評価し、細かな全開・戻しを繰り返すほど消費率を最大1.6倍にする。
一定フルアクセル自体には追加ペナルティを掛けず、Practiceでは従来どおりFuelを消費しない。
Fuel 0でもPITへ戻れるよう前進PWMを1590、後退PWMを対称の1410へ制限し、完全停止にはしない。
ダメージによる速度制限は前進だけに適用し、障害物から脱出するための後退出力は維持する。

通常ギア上限はG3である。前進中に溜まるBoostが100になると、G3から右パドルで2.5秒だけG4を起動できる。
充填時間は順位そのものではなくRace Controlの`intervalToAheadMs`と`lapDeltaToAhead`で決める。先頭は45秒、
同一周回では0秒差の40秒から8秒差以上の20秒まで線形に短縮し、1周遅れは16秒、3周差以上は12秒とする。
タイム差がまだ無い場合、およびRace Control未接続・`green`以外の整備走行では30秒へfallbackする。
`GEAR:4`の直接指定は拒否し、G4終了時はRelayがG3へ戻す。

Relayは旧client向けの`VHS:1`を維持し、HP、Fuel、Boost、実効gearをJSONの`VGS:1`でも配信する。

APIは既定でloopbackだけを許可する。MADSYSTEMを別PCで動かす場合は `-GameplayAllowCidr` を明示できるが、
Relay自身はHTTPのTLSを終端しない。平文tokenを信頼できないネットワークへ流してはならない。

Pi 直結 UI は別配布物である。実ファイル名は `fpv-viewer.html` / `fpv-viewer.js`、URL は `#audioControls=0` のように hash を使う。Relay の `pilot.html` と混同してはならない。
