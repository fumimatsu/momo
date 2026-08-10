# Relay Pit Recovery Tick API

## Status

- API version: 1
- Relay: 実装済み
- MADSYSTEM client: `codex/pit-recovery-publisher` の `281dea044` で実装済み
- Endpoint: `POST /api/v1/gameplay/pit-recovery-ticks`

MADSYSTEM が同一のピットマーカーを連続認識した時間を管理し、2 秒継続するたびに 1 tick を送る。
Relay は tick の正当性を検証し、受理した tick ごとに対象車両の HP を 20 回復する。

MADSYSTEM は回復量を指定しない。HP、回復量、速度上限の正本は Relay に置く。

## Relay 設定

ピット回復を有効にするには、Relay を次の条件で起動する。

- `-health-recovery-mode=pit-marker`
- `-race-url` を設定する
- `MOMO_RELAY_GAMEPLAY_TOKEN` を設定する
- 各 source に一意の `-race-car DEVICE=CAR_ID` を設定する

既定の API 接続元は loopback のみである。MADSYSTEM と Relay を同じ PC で動かす構成を初期仕様とする。

```powershell
$env:MOMO_RELAY_GAMEPLAY_TOKEN = '<GAMEPLAY_TOKEN>'
.\tools\start-mads-observer.ps1 `
  -HealthRecoveryMode 'pit-marker' `
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

- recovery mode が `pit-marker`
- Race Control WebSocket が接続中
- `raceRunId` が active run と一致
- race phase が `green`
- `carId` が一意の Relay source に割り当て済み
- `commandId` が未処理、または同一本文の再送
- tick が entry 内で連続している
- 前回受理した回復 tick から 2 秒以上経過

受理時は `min(100, hp + 20)` を同じ vehicle health lock 内で計算し、更新後の `VHS:1` を Pilot と
Observer へ即時配信する。HP が既に 100 の場合も tick は受理し、`recoveredAmount` は 0 になる。

新しい `raceRunId`、`ready` phase、Relay 再起動では、entry、tick、重複排除履歴を破棄する。
`pit-marker` mode では従来の安全走行による連続回復を行わない。

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
  "hp": 72,
  "speedCap": 0.86,
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
