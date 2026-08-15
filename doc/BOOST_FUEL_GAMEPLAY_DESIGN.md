# Boost / Fuel Gameplay 設計

## 責務

HP、Fuel、Boost、実効gear、前進制限の正本は Local Relay とする。Pilot と Web Observer は表示と入力要求だけを行う。
Race Control は既存の phase、run、standings を配信し、MADSYSTEM は既存の PIT presence / recovery tick 契約を維持する。

## 初期ルール

| 項目 | ルール |
| --- | --- |
| 通常gear | G1..G3。`GEAR:4` / `GEAR:5` は拒否する |
| Boost | G3かつ100の時、右パドルで2.5秒だけG4へ入る |
| G4上限 | 前進PWM 1900。終了時はRelayがG3へ戻す |
| Boost充填 | green中は前走車とのタイム差・周回差に連動し、順位情報なし・レース外は30秒 |
| Fuel | 既定では合計120秒の有効前進で100から0まで定率消費する |
| Practice | `raceInfo.sessionType=practice` の間はFuelを消費しない |
| Fuel 0 | Boostを解除し、前進PWMを1速上限より10低い1590へ制限する。PITへ戻るため完全停止にはしない |
| PIT tick | 1秒ごとにHP +10とFuel +10を同じlock内で適用する。回復速度は10/秒を維持する |
| PIT完了表示 | HP 100かつFuel 100。途中退出は常に許可し、回復済みの値を保持する |
| severe damage | HP -20。HP 100から1回で80となり、PIT 2 tickで戻せる |
| damage有効期間 | Practiceを含む有効な`green`セッション中だけHPを減算する |
| run reset | 新runまたはreadyでHP 100、Fuel 100、Boost 0、G1へ戻す |

Fuel消費は、次をすべて満たす間だけ進行する。

- Race Control接続中
- phaseが`green`
- Drive ON
- PIT外
- 350 ms以内に有効な前進指令を受信
- Fuelが0より大きい

Fuel消費は `raceInfo.sessionType=practice` のとき停止する。Boost充填はRace Control状態に依存せず、
Drive ON、PIT外、350 ms以内の有効な前進指令、Fuelが0より大きい条件で進行する。
`green`中は前走車とのタイム差・周回差を使い、それ以外は30秒の基準時間で充填する。
`sessionType` がない旧Race Controlでは後方互換のためFuelを消費する。

HPダメージと走行回復はRace Control接続中、raceRunIdあり、phaseが`green`の条件でだけ進行する。
Practiceも対象に含む。条件外の衝撃は`race_inactive`として配信・記録し、Boost有効中は
`boost_active`として抑制する。phaseが`green`以外へ変化した時、またはRace Control切断時は
残っているHPダメージを即時回復する。

## Fuel拡張境界

初期版はスロットル量、gear、Boost状態で燃費を変えない。Relayは次の値を状態へ保持・配信する。

- Pilot要求前進量 `requestedThrottle`
- 制限後前進量 `effectiveThrottle`
- 実効gear
- Boost状態
- 現在の `fuelRatePerSecond`

将来の燃費差は `fuelRateLocked()` の計算だけを置き換える。外部APIや別managerは、実際のルールが確定するまで追加しない。

## 配信

旧client向けに `VHS:1,<hp>,<speedCap>,<mode>` を維持する。新clientはJSONの `VGS:1` を使用する。
DataChannel再接続時は `VHS:1`、`VGS:1`、`PIT:1` の現在値を送る。

Pilotは画面中央下部へDAMAGE、FUEL、BOOSTの3段HUDを表示する。既存のGメーター、Throttle、Brakeは移動しない。
Web Observerはleaderboard行へ小型3ゲージを表示し、映像枠へ重複表示しない。

## 実車確認

1. G1..G3の既存上限が変わらないことを確認する。
2. G3以外、Boost未充填、PIT中、Fuel 0でG4へ入れないことを確認する。
3. 順位別の充填時間と2.5秒後のG3復帰を確認する。
4. Fuelが有効前進中だけ減り、PIT、停止、Race Control切断中に減らないことを確認する。
5. Fuel 0でPWM 1590へ制限され、PITへ戻れることを確認する。
6. 1 tickでHP/Fuelがそれぞれ最大20回復し、再送で二重回復しないことを確認する。
7. Pilot再読み込みとObserver複数接続で同じ状態を復元することを確認する。
