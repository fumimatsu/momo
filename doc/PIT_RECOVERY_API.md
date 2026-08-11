# Relay PIT Gameplay API

## Status

- API version: 1
- Relay: 実装済み
- MADSYSTEM client: `codex/pit-recovery-publisher` の `281dea044` で実装済み
- Endpoint: `POST /api/v1/gameplay/pit-recovery-ticks`
- PIT presence endpoint: `POST /api/v1/gameplay/pit-presence-events`

MADSYSTEM が同一のピットマーカーを連続認識した時間を管理し、2 秒継続するたびに 1 tick を送る。
Relay は tick の正当性を検証し、受理した tick ごとに対象車両の HP と Fuel をそれぞれ 20 回復する。

MADSYSTEM は回復量を指定しない。HP、Fuel、回復量、速度上限の正本は Relay に置く。

## Relay 設定

ピット回復を有効にするには、Relay を `pit-marker` または `hybrid` で起動する。

- `-health-recovery-mode=pit-marker` または `hybrid`
- `-race-url` を設定する
- `MOMO_RELAY_GAMEPLAY_TOKEN` を設定する
- 各 source に一意の `-race-car DEVICE=CAR_ID` を設定する

既定の API 接続元は loopback のみである。MADSYSTEM と Relay を同じ PC で動かす構成を初期仕様とする。

```powershell
$env:MOMO_RELAY_GAMEPLAY_TOKEN = '<GAMEPLAY_TOKEN>'
.\tools\start-mads-observer.ps1 `
  -HealthRecoveryMode 'hybrid' `
  -RaceControlUrl 'ws://127.0.0.1:8787/ws/races/race-test' `
  -RaceControlViewerToken '<VIEWER_TOKEN>'
```

別 PC から呼ぶ場合は `-gameplay-allow-cidr` または `-GameplayAllowCidr` で接続元 CIDR を明示できる。
Relay の HTTP は TLS を終端しないため、token が平文で流れるネットワークへ公開してはならない。

## Request

```http
POST /api/v1/gameplay/pit-recovery-ticks HTTP/1.1
Authorization: Bearer <GAMEPLAY_TOKEN>
Content-Type: application/json
```

```json
{
  "schemaVersion": 1,
  "command": "pit_recovery_tick",
  "commandId": "rr_123:CP-1:pit-entry-7:tick-2",
  "sourceId": "madsystem",
  "raceRunId": "rr_123",
  "carId": "CP-1",
  "entryId": "pit-entry-7",
  "tick": 2
}
```

| Field | Type | Contract |
| --- | --- | --- |
| `schemaVersion` | integer | `1` 固定 |
| `command` | string | `pit_recovery_tick` 固定 |
| `commandId` | string | tick ごとに一意。同じ tick の再送では同じ値を使う |
| `sourceId` | string | `madsystem` 固定 |
| `raceRunId` | string | Relay が Race Control から受信している active run と一致させる |
| `carId` | string | `CP-1..CP-4` の固定枠 ID。device ID ではない |
| `entryId` | string | ピットへ入るたびに MADSYSTEM が新しく発行する |
| `tick` | integer | entry 内で 1 から連続して増やす |

識別子の文字列は 1 文字以上 128 bytes 以下とし、前後に空白を含めない。
未知の JSON field、複数の JSON object、不正な型は拒否する。

## MADSYSTEM の送信規則

1. 対象象限でピット用 marker ID が 1 枚以上見えたら滞在計測を開始する。
2. 同じ ID のマーカーが複数枚見えても presence は 1 として扱う。
3. 同じ ID の別マーカーへ視界が移っても、未検出猶予内なら滞在計測を継続する。
4. 連続滞在 2 秒で tick 1、その後 2 秒ごとに tick 2、3 と送る。
5. 未検出が MADSYSTEM の猶予を超えたら entry を終了する。
6. 次の検出では新しい `entryId` と tick 1 から始める。
7. timeout または 5xx では、同じ `commandId` と同じ JSON 本文で再送する。
8. `429 recovery_too_soon` では `retryAfterMs` 以降に同じ要求を再送する。
9. `409 race_control_unavailable`、`race_run_mismatch`、`phase_not_allowed` は、race と marker entry が有効な間、同一本文で再送する。
10. その他の 4xx は内容または設定の不一致として自動再送しない。
11. 車両ごとに送信順を維持し、前の tick が受理される前に次の tick を送らない。
12. marker exit、run 変更、race 終了では未送信・再送待ち tick を破棄する。
13. tick は永続 outbox へ保存せず、MADSYSTEM 再起動後に古い tick を復元しない。

MADSYSTEM のクラス分割、既存コードへの接続、テスト方針は
[MADSYSTEM Pit Recovery 実装指南書](PIT_RECOVERY_MADSYSTEM_IMPLEMENTATION_GUIDE.md) を参照する。
本番環境への配置、確認、rollback は
[PIT 回復機能 本番適用 Runbook](PIT_RECOVERY_PRODUCTION_ROLLOUT.md) に従う。

## Relay の受理条件

Relay は次の条件をすべて満たす要求だけを受理する。

- recovery mode が `pit-marker` または `hybrid`
- Race Control WebSocket が接続中
- `raceRunId` が active run と一致
- race phase が `green`
- `carId` が一意の Relay source に割り当て済み
- `commandId` が未処理、または同一本文の再送
- tick が entry 内で連続している
- 前回受理した回復 tick から 2 秒以上経過

受理時は `min(100, hp + 20)` と `min(100, fuel + 20)` を同じ vehicle health lock 内で計算し、
更新後の `VHS:1` と `VGS:1` を Pilot と Observer へ即時配信する。HP または Fuel が既に 100 の場合も
tick は受理し、対応する `recoveredAmount` または `fuelRecoveredAmount` は 0 になる。

新しい `raceRunId`、`ready` phase、Relay 再起動では、entry、tick、重複排除履歴を破棄する。
`pit-marker` mode では従来の安全走行による連続回復を行わない。
`hybrid` mode では安全走行による連続回復と PIT tick 回復の両方を行う。

## Success Response

初回の受理と同一要求の再送は、どちらも HTTP `200` を返す。

```json
{
  "schemaVersion": 1,
  "status": "applied",
  "commandId": "rr_123:CP-1:pit-entry-7:tick-2",
  "raceRunId": "rr_123",
  "carId": "CP-1",
  "entryId": "pit-entry-7",
  "tick": 2,
  "recoveredAmount": 20,
  "fuelRecoveredAmount": 20,
  "hp": 100,
  "fuel": 80,
  "speedCap": 1,
  "mode": "healthy"
}
```

同じ `commandId` と同じ内容を再送した場合は `status` が `duplicate` になり、最初に受理した結果を返す。
同じ `commandId` を異なる内容へ流用した場合は拒否する。

## Error Response

```json
{
  "schemaVersion": 1,
  "error": "race_run_mismatch",
  "message": "raceRunId does not match the active run"
}
```

| HTTP | `error` | Meaning |
| --- | --- | --- |
| `400` | `invalid_json` | JSON の形式、型、field が不正 |
| `400` | `invalid_command` | 必須値または固定値が不正 |
| `401` | `unauthorized` | Bearer token がない、または一致しない |
| `403` | - | 接続元 IP が許可 CIDR 外 |
| `404` | `unknown_car` | `carId` が一意の source に対応しない |
| `409` | `race_control_unavailable` | Race Control WebSocket が未接続 |
| `409` | `race_run_mismatch` | run が一致しない |
| `409` | `phase_not_allowed` | phase が `green` ではない |
| `409` | `recovery_mode_not_allowed` | recovery mode が `pit-marker` ではない |
| `409` | `tick_out_of_sequence` | entry の tick が連続していない |
| `409` | `entry_id_reused` | 同じ run で過去に使った entry ID が再利用された |
| `409` | `command_id_conflict` | 同じ ID が異なる内容に使われた |
| `429` | `recovery_too_soon` | 前回受理から 2 秒経っていない |
| `503` | `gameplay_api_disabled` | Relay に gameplay token が未設定 |

`429` は `retryAfterMs` を含む。

## PIT Presence Event

PIT IN / PIT OUT の表示状態は回復 tick から推測しない。MADSYSTEM は marker presence の確定と消失を、回復 tick とは別の event として送る。

```http
POST /api/v1/gameplay/pit-presence-events HTTP/1.1
Authorization: Bearer <GAMEPLAY_TOKEN>
Content-Type: application/json
```

```json
{
  "schemaVersion": 1,
  "event": "pit_presence",
  "eventId": "550e8400-e29b-41d4-a716-446655440000",
  "sourceId": "madsystem",
  "raceRunId": "rr_123",
  "carId": "CP-1",
  "entryId": "pit-entry-7",
  "transition": "entered",
  "occurredAtUnixMs": 1786348800123,
  "reason": "marker_confirmed"
}
```

`entered` の reason は `marker_confirmed`、`exited` は `marker_lost`、`observation_stale`、`video_invalid` のいずれかとする。同じ滞在では `entryId` を維持し、transition ごとに新しい `eventId` を発行する。

Relay は Race Control 接続、active run、`green` phase、car mapping、entered/exited の順序を検証する。同じ `eventId` と同じ本文の再送は `200 duplicate`、本文が異なる再利用は `409 event_conflict` とする。受理済み event は Relay process 内で直近 256 件を保持する。

```json
{
  "schemaVersion": 1,
  "status": "applied",
  "eventId": "550e8400-e29b-41d4-a716-446655440000",
  "raceRunId": "rr_123",
  "carId": "CP-1",
  "entryId": "pit-entry-7",
  "transition": "entered",
  "present": true,
  "serverTimeMs": 1786348800201
}
```

MADSYSTEM の `occurredAtUnixMs` は診断用である。Observer の PIT 滞在時刻には Relay が受理した `serverTimeMs` を使う。

presence event をまだ送らない旧 MADSYSTEM の回復 tick も引き続き受理する。この場合 HP / Fuel 回復と
`VHS:1` / `VGS:1` は動作するが、Relay は tick だけから PIT IN / PIT OUT を生成しない。

## Observer Telemetry

presence transition、active entry の回復 tick、run / phase / Race Control 接続の reset 時に、Relay は対象 source の `momo-telemetry` DataChannel へ `PIT:1` を送る。

```text
PIT:1,{"raceRunId":"rr_123","carId":"CP-1","present":true,"entryId":"pit-entry-7","enteredAtUnixMs":1786348800201,"lastAcceptedTick":1,"serviceState":"servicing","hp":92,"fuel":80}
```

`serviceState` は `outside`、`servicing`、`complete` のいずれかである。`complete` は active entry かつ
HP 100 / Fuel 100 を表す表示値であり、PIT OUTを禁止する条件ではない。途中でPIT OUTした場合はその時点の
HP / Fuelを保持し、以後のtickを停止する。`exited` 後は `exitedAtUnixMs` と `exitReason` を含める。

新しい Pilot / Observer の telemetry DataChannel が開いた時、Relay は現在の `VHS:1`、`VGS:1`、`PIT:1` を
各 1 回送る。新 run、`green` 以外の phase、Race Control 切断では active entry を解除し、`present: false` を配信する。

## Vehicle Gameplay State

`VHS:1` は旧 client 用に維持する。新しい Pilot / Observer は包括状態 `VGS:1` を正として表示する。

```text
VGS:1,{"hp":80,"speedCap":0.933,"mode":"healthy","fuel":42.5,"fuelState":"normal","boost":100,"boostState":"ready","boostRemainingMs":0,"gear":3,"normalGearMax":3,"position":2,"fieldSize":4,"fuelRatePerSecond":0.833,"requestedThrottle":1,"effectiveThrottle":0.56,"serverTimeMs":1786348800201}
```

- `fuelState`: `normal`、`low`、`empty`
- `boostState`: `charging`、`ready`、`active`
- `gear`: Relay が適用している実効gear。通常は1..3、Boost中だけ4
- `requestedThrottle`: Pilotが要求した前進量を0..1へ正規化した値
- `effectiveThrottle`: gear、HP、Fuel制限後の前進量を0..1へ正規化した値
- `fuelRatePerSecond`: 現在のFuel消費率。初期版はスロットル量に依存しない

FuelとBoostが進行する条件は、phaseが`green`、Race Control状態が5秒以内、Drive ON、PIT外、
前進指令が350 ms以内であること。Fuel 0ではBoostを解除し、前進PWMを1550へ制限する。
