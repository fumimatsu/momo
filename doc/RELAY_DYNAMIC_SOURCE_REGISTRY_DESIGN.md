# Relay 動的 source registry 設計

## 目的

Relay を 4 台固定の起動引数から、車両が増減してもプロセスを再起動せずに source を登録できる構成へ移行する。
各車両の Pi / Momo は従来どおり LAN P2P endpoint を 1 本だけ提供し、Relay が上流へ接続する。Pilot は同じ
source に対して LAN Relay または Relay 経由 Ayame のどちらかを選ぶ。

Pi を Relay 接続と Direct Ayame の間で切り替える方式は採用しない。映像入力、serial、操縦権を二重化せず、
経路選択を Relay より下流へ限定する。

## source の識別

source ID は IP address ではなく、車両へ固定した安定 ID を使用する。既存の `11.3` から `11.6` は互換のため
維持できるが、新規車両では hostname または車両管理 ID を推奨する。DHCP で IP が変わっても source ID、
Ayame room、Race Control car ID の対応を変えない。

1 source は次を持つ。

```json
{
  "id": "momo-fpv-17",
  "url": "ws://192.168.11.17:8080/ws",
  "raceCarId": "CP-17",
  "ayamePilotEnabled": true,
  "ayamePilotRoom": "momo-relay-momo-fpv-17-ext"
}
```

- `id`: Relay 内の不変 ID
- `url`: Relay から Pi / Momo へ接続する LAN endpoint
- `raceCarId`: Race Control の車両 ID。レースへ参加しない source は省略できる
- `ayamePilotEnabled`: 外部 Pilot を許可するか
- `ayamePilotRoom`: source 専用 room。別 source と共有しない

## 接続と排他

LAN と Ayame は別の Pilot lease を持たない。どちらも source の同じ `reservePilot` を取得する。

| 状態 | 結果 |
| --- | --- |
| `11.3` を LAN Pilot が使用中に Ayame Pilot が接続 | Ayame を拒否 |
| `11.3` を Ayame Pilot が使用中に LAN Pilot が接続 | LAN を HTTP 409 で拒否 |
| `11.3` を Ayame、`11.4` を LAN で使用 | 許可 |
| 別 source の Observer / MADSYSTEM | 継続 |

Ayame room lock は外部 Browser 同士の重複を防ぐ。LAN と Ayame の交差排他は Relay の source lease が担う。
room lock だけに依存してはならない。

## 静的設定と動的 registry

`-config` は運用上必ず存在する source の静的 seed として残す。`-source-registry` は Relay が所有する別 JSON で、
管理 API から追加した source だけを保存する。静的設定を API から削除することはできない。

```powershell
$env:MOMO_RELAY_ADMIN_TOKEN = '<random-admin-token>'
$env:MOMO_AYAME_SIGNALING_KEY = '<backend-signaling-key>'

./momo-local-relay-device-input-v15.exe `
  -config C:\fpv\relay-config.json `
  -source-registry C:\fpv\relay-dynamic-sources.json `
  -source-admin-allow-cidr 192.168.11.0/24 `
  -ayame-signaling-url wss://133.88.123.51.nip.io/signaling `
  -ayame-room-prefix momo-relay
```

`-ayame-room-prefix momo-relay` を指定すると、明示的に opt-out していない source に
`momo-relay-<source-id>-ext` を割り当てる。`11.3` は `momo-relay-11-3-ext` となり、既存 room と一致する。
Relay は source ID、Race Control car ID、Ayame room の重複を起動時と追加時の両方で拒否する。

registry は同一 directory の一時ファイルへ同期してから置換する。置換中の異常に備えて `.bak` を使用し、
主ファイルがない場合は backup を読み込む。Git 管理対象にはしない。

## 管理 API

API は `-source-admin-allow-cidr` と `MOMO_RELAY_ADMIN_TOKEN` の両方を要求する。インターネットへ公開しない。

### 一覧

```http
GET /api/v1/sources
Authorization: Bearer <MOMO_RELAY_ADMIN_TOKEN>
```

内部 LAN URL を含むため、Garage 用の `GET /api/v1/pilot-devices` とは分離する。

### 追加

```http
POST /api/v1/sources
Authorization: Bearer <MOMO_RELAY_ADMIN_TOKEN>
Content-Type: application/json

{
  "id": "momo-fpv-17",
  "url": "ws://192.168.11.17:8080/ws",
  "sourceKind": "vehicle",
  "displayName": "CAR 17",
  "raceCarId": "CP-17"
}
```

管理 API は operator PC または信頼済み管理 service から呼び、`url` を明示する。Relay admin token を Pi へ
配布して自己登録させてはならない。1 台の侵害で他 source の一覧、更新、削除まで可能になるためである。

`sourceKind`は`vehicle`または`venue`で、省略時は`vehicle`とする。`displayName`は省略時にsource IDとなる。
`venue`は俯瞰カメラ等のread-only映像sourceであり、`raceCarId`、Ayame Pilot、Pilot WebSocket、Garage、
車両別Race Control stateの対象にしない。Program Observer等がObserver roleで映像を選択する。

追加は Race Control phase が `green` の間は拒否する。登録後は上流 Momo への接続を開始する。
`vehicle`だけがAyame Pilot roomとLAN Garageの対象になる。

### 再登録と IP 更新

```http
PUT /api/v1/sources/momo-fpv-17
Authorization: Bearer <MOMO_RELAY_ADMIN_TOKEN>
Content-Type: application/json

{
  "url": "ws://192.168.11.117:8080/ws",
  "sourceKind": "vehicle",
  "displayName": "CAR 17",
  "raceCarId": "CP-17"
}
```

車両が DHCP で別 IP になった場合は、安定した source ID を path に指定して新しい `url` を登録する。
同一定義の再送は接続を作り直さず成功する。URL または割当が変わった場合は、
レース中でも使用中でもないことを確認して source 接続を差し替える。

### 削除

```http
DELETE /api/v1/sources/<source-id>
Authorization: Bearer <MOMO_RELAY_ADMIN_TOKEN>
```

動的 source だけを削除できる。レース中、Drive 中、Pilot / Observer 接続中は拒否する。強制削除 API は設けない。
操縦中の source を消す必要がある場合は、先に Drive OFF と Viewer 切断を確定する。

## 外部 Pilot URL 発行

公開 Pages は Relay の LAN registry を直接参照しない。運営 PC の発行スクリプトが protected API から source を解決し、
VPS authn service へ短期 ticket を要求する。

```powershell
$env:MOMO_RELAY_BASE_URL = 'http://192.168.11.100:8090'
$env:MOMO_RELAY_ADMIN_TOKEN = '<relay-admin-token>'
$env:FPV_LOCK_BASE_URL = 'https://133.88.123.51.nip.io/fpv-lock'
$env:FPV_LOCK_ADMIN_TOKEN = '<vps-admin-token>'

python C:\src\momo-fpv\tools\vps\issue_fpv_pilot_ticket.py `
  --source momo-fpv-17 `
  --viewer-url https://fumimatsu.github.io/momo-fpv-viewer-pages/pilot.html
```

生成 URL は `signaling=ayame`、`relayTransport=1`、`device`、`carId`、`roomId` を registry と一致させ、
ticket は URL fragment にだけ入れる。

## 台数上限と分割

動的登録は Relay の処理能力を増やさない。現行 hard cap は 32 source とする。上限を外して 1 process に詰め込まない。

- source ごとに Pi への WebRTC、Ayame 待機 WSS、映像 RTP 転送、Telemetry state を持つ
- 外部 Pilot が TURN を使う場合、VPS 帯域は active Pilot 数に比例する
- Observer / MADSYSTEM の decode と ArUco は Relay より先に CPU / GPU 上限へ達する可能性がある
- 通常割当は負荷試験で CPU p95 50% 付近に収まる台数とし、60% を hard cap にしない

32 台を超える場合は Relay node を分け、room prefix に会場または node ID を含める。

```text
tokorozawa-r1-momo-fpv-17-ext
tokorozawa-r2-momo-fpv-41-ext
```

## 残る control plane

現行実装は LAN 内の動的登録と、運営 PC からの URL 発行までを扱う。公開ユーザーが車両一覧から選び、
自分で ticket を取得する機能はまだ持たない。これには Relay admin token と VPS admin token を Browser へ渡さない
session broker が必要である。

broker は Race Control Worker または専用 service に置き、次だけを公開する。

1. Relay から署名付き source availability heartbeat を受ける
2. 公開可能な source ID、表示名、使用状態だけを返す
3. 認可済み操作に対して VPS ticket を代理発行する
4. room ID と ticket を監査ログへ残すが、token 本文は残さない

Relay の内部 URL、admin token、Ayame backend signaling key は公開 catalog へ含めない。

車両自身による自動 enrollment も後続とする。実装する場合は Relay admin token を共有せず、source ID に
束縛した車両別 token と、作成・自分自身の URL 更新だけを許す専用 endpoint を使う。任意 source の参照、
Race Control car ID / Ayame policy の変更、削除は許可しない。

## 受け入れ条件

- Relay 再起動なしで 5 台目を追加し、Garage へ表示できる
- 追加 source が LAN Pilot と Ayame Pilot の両方で接続できる
- 同一 source の LAN / Ayame 同時接続が拒否される
- 異なる source は LAN / Ayame を混在して同時使用できる
- registry からの再起動復元後も source ID と room ID が変わらない
- 同じ source ID の再登録で DHCP 後の IP を更新できる
- 重複 source ID、car ID、room ID が拒否される
- レース中または使用中の source を追加・削除できない
- admin token なし、許可 CIDR 外からの管理 API が拒否される
