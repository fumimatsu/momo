# Relay 責務境界

更新日: 2026-08-19

## 方針

Relay は、レース管理が存在しない単独走行でも必要になる接続、操縦、安全制御、
telemetry 配信を担当する。LAP、順位、gap、レース進行、HP、FUEL、BOOST など、
レースまたは gameplay のルールで決まる状態は外部 authority が決定し、Relay は
検証済みの結果を配信または車体出力へ適用する。

ここでいう authority は、同じ入力から最終値を決定し、その値を公式状態として
publish できる component を指す。Relay 内で表示用に補間または整形した値は
projection であり、Race Control や gameplay authority へ書き戻してはならない。

## 責務表

| 領域 | 正本 | Relay の責務 | 判定 |
| --- | --- | --- | --- |
| source registry、room、signaling、WebRTC、DataChannel | Relay | 接続 lifecycle、認証、帯域制御、再接続、fan-out | 維持 |
| Pilot ownership、DRIVE、watchdog | Relay | 操縦権の排他、切断時 neutral、stale command の拒否 | 維持 |
| 物理的な PWM / gear 安全上限 | Relay | 外部状態で許可された範囲を超えないよう最終出力を制限 | 維持 |
| raw telemetry | 車載 S3 / Momo | 構文・鮮度検証、bounded log、Viewer / Observer への配信 | 維持 |
| race lifecycle、roster、flag | Race Operations Coordinator / Race Control | accepted state の cache と配信 | 外部 authority |
| LAP、sector、順位、gap、finish | Timing Engine | accepted `race_state v2` を改変せず配信 | 外部 authority |
| personal / overall best | Timing Engine | `lapHistory[].achievement` を読み上げと表示へ使う | 外部 authority |
| impact sensor event | Relay の telemetry adapter | sensor 値の検証、event ID、raw impact candidate の生成 | Relay に残せる |
| HP、damage、FUEL、BOOST | Gameplay Engine | authoritative envelope の cache、配信、車体出力への強制 | 移動対象 |
| PIT presence / recovery | Marker Event + Gameplay Engine | 認証・冪等化 gateway、結果の配信 | 移動対象 |
| race audio | Race event producer + presentation layer | 固定文言の生成、TTS transport、rate limit、ducking | 一部移動 |
| map 補間、race clock 補間、接近警告 | Viewer / Observer | 表示専用 projection | 維持 |

## 現在の実装

### 分離できている箇所

- `raceMessageForCar` は Race Control payload へ `viewerCarId` を追加するだけで、
  standings、LAP、sector、gap を再計算しない。
- `courseProgressTracker` は accepted race state を telemetry log 用の canonical
  projection にする。公式計時へは書き戻さない。
- Web Observer と Pilot の race clock、course map、rear attention、blue flag は
  表示用の補間または通知判定である。公式結果には使わない。
- LAP の自己ベスト・全体ベスト判定は Timing Engine の
  `lapHistory[].achievement` を正とする。Race Control は保持し、Relay は再計算せず
  音声文言へ変換する。

### 責務が混在している箇所

`tools/momo-relay/vehicle_health.go` は、次の異なる責務を同時に持つ。

- impact class から damage 量を決めて HP を更新する。
- 走行回復、FUEL 消費、PIT 回復量を計算する。
- 順位と前車 gap から BOOST 充填時間を決める。
- BOOST 発動時間、gear 4、ガス欠、HP による speed cap を決める。
- 決定した制限を実際の throttle command へ適用する。

最後の「車体出力へ適用する」は Relay に必要だが、それ以前は gameplay rule である。
同じ型と mutex の中にあるため、ルール変更と操縦安全性の変更を分離して試験できない。

`pit_recovery.go` と `pit_presence.go` も、API の認証・冪等化だけでなく、tick 順序、
回復量、HP / FUEL、service complete を Relay 内で決定している。PIT marker の検出元が
外部であるのに、gameplay authority だけ Relay に残っている。

`boost_regen_probe.go` は ESC / drive telemetry から回生候補を検出した直後、Relay 内の
BOOST へ直接加算する。検出 evidence の生成と gameplay rule の適用を分ける必要がある。

`raceAudioDetector` は accepted snapshot から LAP と finish の遷移を検出する。
表示機能としては許容できるが、全 Pilot 共通音声を exactly-once で扱う段階では、
Timing Engine または Race Control が発行する明示的な race event を受け取る形へ変える。

## 仕様判断が必要な境界

Race Control が未接続または phase が green ではない場合、damage と FUEL 消費は停止する。
一方、現在の BOOST passive charge と `activateBoost` には race active 条件がなく、DRIVE 中なら
fallback の 30 秒で充填して発動できる。この挙動は
`TestVehicleGameplayStandaloneBoostUsesFallbackAndKeepsFuel` で固定されており、偶発的な不具合ではない。
単独走行でも BOOST を許可するなら standalone gameplay profile として明文化する。レース専用へ
変更するなら既存仕様の変更になるため、移行前に profile と rollback 条件を決める。

## 目標構成

```text
S3 / Momo telemetry -----> Relay telemetry adapter -----> immutable vehicle events
Pilot command -----------> Relay ownership / safety ----+                |
                                                                  Gameplay Engine
Timing Engine -----------> Race Control --------------------------+      |
                                                                         v
Relay <---------------- versioned vehicle_gameplay_state / envelope -----+
  |
  +-- enforce speedCap / gearMax / limp / neutral
  +-- distribute telemetry, race state, gameplay state and audio
```

Gameplay Engine は Timing Engine の計時 package へ混ぜない。別 service または明確に分離した
package とし、race lifecycle、authoritative gap、impact event、drive sample、PIT event を入力にする。
出力には少なくとも `schemaVersion`、`raceRunId`、`sequence`、`carId`、policy revision、HP、
FUEL、BOOST、gear max、speed cap、PIT state、生成時刻を含める。

Relay は envelope の run、sequence、鮮度、car mapping を検証してから出力へ適用する。
gameplay 無効の単独走行では物理安全上限だけを適用する。gameplay 必須の race profile で
state が stale または authority が切断された場合は、古い強化状態を維持せず neutral または
明示した保守的上限へ fail closed する。

## 移行順序

1. 現行挙動を fixture 化し、HP、FUEL、BOOST、PIT、impact、command limit の入出力を固定する。
2. Gameplay Engine の event と state envelope を versioned schema として追加する。
3. 現行 Relay gameplay を shadow producer と比較し、1 台、2 台、4 台で一致を確認する。
4. Relay の command limiter を外部 envelope 駆動へ切り替える。rollback profile を残す。
5. PIT API と boost regen は compatibility gateway に縮小し、event を変更せず Gameplay Engine へ渡す。
6. LAP、finish、PIT complete の明示 event を追加し、race audio の snapshot 推定を段階的に外す。
7. parity と stale-state fail-closed を確認してから Relay 内の rule calculation を削除する。

現時点で `vehicle_health.go` を分割するだけの refactor は行わない。外部契約と replay がないまま
移動すると、障害時にどちらが authority だったか追跡できなくなるためである。
