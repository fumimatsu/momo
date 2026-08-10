# MADSYSTEM Pit Recovery 実装指南書

## Status and baseline

- Relay API: `momo` `ad093ed` で実装済み
- Relay verification: Go `1.26.5`、`go test ./...` と Windows amd64 build 成功
- MADSYSTEM client: `281dea044` で実装・ローカル結合確認済み
- 実装した MADSYSTEM branch: `codex/pit-recovery-publisher`
- 実装 baseline: `815e6533577b8d7ef0368dbdbaa2d301d8e6f30a`

baseline hash は参照値であり、既存の local 変更を破棄して checkout する指定ではない。
作業開始時は必ず MADSYSTEM の `git status --short --branch` と最新 commit を確認し、
`TimerScript.cs`、scene、ProjectSettings などの既存変更を PIT 実装へ混ぜない。

## 作業の固定境界

最初の完了条件は「marker group の presence から Relay API へ正しい tick を送れる」までとする。
Web Observer、sector history、collision attribution、HP 計算は同じ変更へ入れない。

```text
ArUcoWebCamMulti
  -> quadrant ごとの PitMarkerObservation
  -> PitMarkerPresenceTracker
  -> MomoPitRecoveryPublisher
  -> POST /api/v1/gameplay/pit-recovery-ticks
  -> Relay authoritative HP / speedCap
  -> VHS -> Pilot / Observer
```

MADSYSTEM が決める値:

- どの quadrant で PIT marker group が見えているか
- presence が同一 entry として継続しているか
- `entryId`、連番 `tick`、tick ごとの `commandId`
- active entry 中に同じ request body を再送するか

MADSYSTEM が決めてはならない値:

- 回復量
- 回復後 HP
- speed cap
- damage mode
- Race Control phase の代替判定

Relay response の `hp`、`speedCap`、`mode` は診断表示に使えるが、MADSYSTEM 側で再計算しない。

## 対象とゴール

この文書は `C:\src\MADSYSTEM` に Relay pit recovery client を実装する担当者向けである。
Relay 側の契約は実装済みであり、MADSYSTEM は次だけを担当する。

1. 4 分割映像からピット専用 ArUco marker の presence を象限ごとに求める
2. 同じ entry で 2 秒連続して presence が成立するたびに tick を 1 件送る
3. 象限を既存の Race Control `carId` へ変換する
4. 通信失敗時に、同じ tick の同じ本文だけを限定的に再送する

MADSYSTEM は HP、回復量、speed cap を計算しない。Relay の API 契約は
[Relay Pit Recovery Tick API](PIT_RECOVERY_API.md) を正本とする。

## 推奨 commit 分割

既存の timing / sector 作業と衝突させないため、次の順で commit を分ける。

1. contract DTO、pure tracker、fake transport、EditMode tests
2. machine-local settings と `MomoPitRecoveryPublisher`
3. `ArUcoWebCamMulti` の PIT detector / observation 接続
4. lifecycle reset、diagnostics、結合試験記録

1 と 2 は camera frame や scene を必要としない。最初にここを通せば、画像認識の調整と
HTTP idempotency の不具合を分離できる。scene / Prefab 変更が必要になった場合は 3 以降へ隔離する。

## 現行コードで利用する境界

### 映像と象限

`Assets/Script/race_manage/ArUcoWebCamMulti.cs` は Observer の 2x2 合成映像を処理し、検出結果を
象限 `0..3` へ戻している。既存の主な処理は次である。

- `ProcessFrameSinglePass`: 全フレームを縮小し、ArUco 検出を 1 回行う
- `MapDetectionsToQuadrants`: marker の中心から象限を求める
- `detectedIdsPerQuadrant`: 象限ごとの検出 ID
- `OnDataReceived`: checkpoint / bonus の通過処理
- `ReturnCheckPointFlag`: marker 消失後に checkpoint 通過を確定する
- `GetVideoStreamState`: 象限ごとの映像状態を返す

ピット presence を `ReturnCheckPointFlag` へ追加してはならない。checkpoint は「認識後に消えた時」を通過とするが、
pit は「見えている時間」を扱うため、状態遷移が異なる。

### carId

象限から `carId` への変換には `EventManager.ResolveRaceControlCarId(quadrant)` を使う。
`CP-1..CP-4` や device `11.x` を新しいコードへハードコードしない。

`raceControlCarIds` の並びが Observer の表示枠と Relay の `-race-car` に一致していることを運用前提とする。

### raceRunId

Relay は active `raceRunId` と一致しない tick を拒否する。現在の active run は
`MomoRaceControlPublisher` の `outbox.raceRunId` に保存されているが private である。

次の読み取り専用 API を `MomoRaceControlPublisher` に追加する。

```csharp
public static bool TryGetActiveRaceRunId(out string raceRunId)
{
    raceRunId = instance?.outbox?.raceRunId?.Trim() ?? string.Empty;
    return !string.IsNullOrEmpty(raceRunId);
}
```

setter や outbox 自体は公開しない。run が空、変更、または race 終了になった場合、pit entry と pending request を
全車分破棄する。

`TryGetActiveRaceRunId` は run の存在だけを公開する。PIT publisher が Race Control phase を推測する API にしない。
MADSYSTEM の実スタート時に local lifecycle を active にし、Stop、countdown cancel、integration OFF では response 待ちを
含めて即時 inactive にする。Relay は別途 Race Control WSS の `green` phase を検証するため、両方が成立した時だけ
回復が適用される。

推奨する lifecycle entry point:

```csharp
MomoPitRecoveryPublisher.NotifyRaceStarted();
MomoPitRecoveryPublisher.NotifyRaceStopped(PitMarkerExitReason.RaceStopped);
MomoPitRecoveryPublisher.NotifyIntegrationDisabled();
```

呼び出し位置は既存の `MomoRaceControlPublisher.NotifyRaceStarted()`、`FinalizeRace()`、
`NotifyRaceReady()`、`NotifyIntegrationDisabled()` と同じ MADSYSTEM lifecycle event に合わせる。
PIT publisher から Race Control command を送らない。

## 追加するクラス

既存の画像クラスへ HTTP、token、再送処理を混在させない。次の責務で分ける。

| File | Responsibility |
| --- | --- |
| `PitMarkerPresenceTracker.cs` | 1 象限分の presence 状態、未検出猶予、entry、2 秒境界を管理する純粋 C# class |
| `MomoPitRecoveryContract.cs` | request / response / error DTO と JSON serialization |
| `MomoPitRecoveryPublisher.cs` | 4 tracker の統合、active run、carId、1 車両 1 in-flight、HTTP と retry |
| `MomoPitRecoveryLocalSettings.cs` または既存 local settings 拡張 | Relay URL、gameplay token、機能 ON/OFF を machine-local に読む |
| EditMode tests | tracker、contract、retry policy、run / exit reset を検証する |

配置先は既存 Race Control 実装に合わせて `Assets/Script/race_manage/` と
`Assets/Script/race_manage/RefactorSupport/` を使う。

## ArUco 検出の追加

### Dictionary

通常 checkpoint と同じ `DICT_4X4_50` を使い、marker ID は
`EventManager.BonusCheckPointNo` の現行値 `49` とする。同じ dictionary / ID の marker を
ピットエリアへ複数置く。

同じ ID が 1 フレームで複数検出されても presence は 1 回だけ成立する。

### Detector

PIT 専用 detector は追加しない。既存の 1 回の ArUco 検出結果と `detectedIdsPerQuadrant` から、
4 象限それぞれの ID `49` の presence を取得する。既存 checkpoint 検出と PIT API 用 presence tracker は
同じ検出結果を使うが、消失後の PIT IN 確定処理と 2 秒周期の回復 tick は別の状態として管理する。

画像処理から Publisher へ渡す値は、フレームごとの次の観測だけにする。

```csharp
public readonly struct PitMarkerObservation
{
    public PitMarkerObservation(int quadrant, bool present, bool videoValid, double observedAtSeconds)
    {
        Quadrant = quadrant;
        Present = present;
        VideoValid = videoValid;
        ObservedAtSeconds = observedAtSeconds;
    }

    public int Quadrant { get; }
    public bool Present { get; }
    public bool VideoValid { get; }
    public double ObservedAtSeconds { get; }
}
```

`ObservedAtSeconds` は `Time.realtimeSinceStartupAsDouble` に相当する単調時間を使う。PC の壁時計や
`Time.time` に依存させない。

### Fail closed

次では対象象限を即時または設定した短い猶予後に inactive とする。

- ArUco 機能 OFF
- ArUco 検出未初期化
- Observer frame の停止
- 象限の映像状態が有効ではない
- scene disable / component disable
- active `raceRunId` の変更または消失
- race の停止・終了

checkpoint 検出の noise prefilter によって象限の検出自体を skip したフレームを、単純な `present=false` と
数えるかは明示する。推奨は `VideoValid=false` とし、短い未検出猶予内だけ ACTIVE を維持することである。

## Presence tracker

### 状態

各象限に独立した tracker を 1 個持つ。

```text
OUTSIDE
  -> marker detected
ENTER_CANDIDATE
  -> enter threshold satisfied
ACTIVE
  -> 2 seconds elapsed and no request is pending
TICK_PENDING
  -> HTTP 200 applied / duplicate
ACTIVE
  -> marker missing beyond grace
OUTSIDE
```

`entryId` は `ENTER_CANDIDATE -> ACTIVE` で `Guid.NewGuid().ToString("D")` により 1 回だけ生成する。
`tick` は 1 から開始する。`commandId` も tick ごとに新しい GUID とする。

### 2 秒の数え方

- 最初の tick は ACTIVE が 2 秒継続した後に作る
- tick が Relay に受理された時点から、次の 2 秒を数える
- 1 車両につき HTTP request は 1 件だけ in-flight にする
- request が未確定の間に tick 2、3 を先行生成しない
- 同じ marker ID が別の物理 marker へ切り替わっても、未検出猶予内なら entry を継続する
- 同一フレームで同じ ID が複数見えても elapsed を重複加算しない

この方式では通信停止中の回復量を後からまとめて取り戻さない。通信が不安定なら回復が遅くなるが、
ピットを出た後の遅延回復を防ぐ方を優先する。

### 推奨する純粋 API

tracker 自体は UnityWebRequest を知らない形にする。

```csharp
public sealed class PitMarkerPresenceTracker
{
    public PitMarkerTransition Observe(
        bool present,
        bool videoValid,
        double nowSeconds,
        bool requestPending);

    public void MarkTickAccepted(double nowSeconds);
    public void Reset(PitMarkerExitReason reason);
}
```

`Observe` は state、entryId、次に送る tick、exit reason を返すだけとし、通信は Publisher が行う。

## HTTP Publisher

### 設定

Race Control の token と Relay gameplay token は別物である。次を machine-local settings へ追加する。

```json
{
  "pitRecoveryEnabled": true,
  "relayGameplayBaseUrl": "http://127.0.0.1:8090",
  "relayGameplayToken": "<GAMEPLAY_TOKEN>"
}
```

token を scene、Prefab、ScriptableObject、Git 管理ファイル、ログへ保存しない。
`sourceId` は Relay 契約の `madsystem` 固定であり、Race Control の `MADSYSTEM-01` を流用しない。

現行 `MomoRaceControlLocalSettings` は
`%USERPROFILE%\AppData\LocalLow\MADX\MADSYSTEM\MomoRaceControl\settings.json` を読み込む。
同じ JSON へ上記 field を追加してよいが、`relayGameplayToken` を `EventManager` の `[SerializeField]` へ追加してはならない。
local loader から `MomoPitRecoverySettings` の読み取り専用 snapshot を返し、PIT publisher だけが保持する。

推奨 API:

```csharp
public static bool TryLoadPitRecovery(out MomoPitRecoverySettings settings);
```

設定 file がない、token が空、URL が HTTP(S) absolute URL ではない場合は disabled とする。
default file を生成する場合も token は空文字にし、実値を source control へ入れない。

### Request の生成

```csharp
var payload = new MomoPitRecoveryTickRequest
{
    schemaVersion = 1,
    command = "pit_recovery_tick",
    commandId = pending.CommandId,
    sourceId = "madsystem",
    raceRunId = activeRaceRunId,
    carId = EventManager.Instance.ResolveRaceControlCarId(quadrant),
    entryId = tracker.EntryId,
    tick = tracker.NextTick
};
```

URL は `relayGameplayBaseUrl.TrimEnd('/') + "/api/v1/gameplay/pit-recovery-ticks"` とする。
header は `Content-Type: application/json` と `Authorization: Bearer <token>` を付け、timeout は 5 秒を初期値とする。

### 再送

Race Control の timing outbox を流用しない。pit tick は非永続の車両別 pending request とする。

| Result | Action |
| --- | --- |
| HTTP `200`, `status=applied` | tick 受理。次の 2 秒計測を開始 |
| HTTP `200`, `status=duplicate` | 受理済みとして同じ処理 |
| timeout / connection error / `5xx` | entry が ACTIVE の間だけ、同じ ID・同じ本文を再送 |
| `429 recovery_too_soon` | `retryAfterMs` 後、ACTIVE なら同じ要求を再送 |
| `409 race_control_unavailable` / `race_run_mismatch` / `phase_not_allowed` | race と entry が有効な間、同じ要求を再送 |
| その他の `4xx` | permanent failure として pending を破棄し、その entry の送信を停止 |
| marker exit / run change / race end | response 待ちを含めて pending を破棄し、再送しない |

retry は指数 backoff とし、上限を 2 秒程度に抑える。再送のたびに JSON を作り直さず、最初に作った本文を保持する。
Unity coroutine を物理的に cancel できない場合は、response 処理時に `runGeneration` と `entryId` を照合し、古い response を無視する。

永続 outbox に入れない理由は、アプリ再起動後や pit exit 後に古い tick が送信されると、marker が見えていないのに
HP が回復するためである。

### active 条件

Publisher は少なくとも次を満たす時だけ新しい tick を作る。

- `pitRecoveryEnabled`
- `EventManager.Instance.useFPVRC`
- Practice / Qualy Rounds / Final の対象 mode
- `MomoRaceControlPublisher.TryGetActiveRaceRunId` が成功
- MADSYSTEM 上で実レース開始済み
- 対象象限の marker tracker が ACTIVE
- 対象象限に有効な `carId` がある
- Relay URL と gameplay token が設定済み

`raceRunId` 取得前に marker が見えていても、その滞在時間を run 取得後へ引き継がない。
run 取得後に新しく enter threshold を満たした entry だけを送信対象にする。

Relay も Race Control WebSocket、run、`green` phase を独立して検証する。MADSYSTEM の active 判定だけを
セキュリティ境界にしない。

## 既存 outbox との関係

`MomoRaceControlPublisher.PrepareRace()` は `set_ready` 成功後に timing run を作り、response の
`raceRunId` を保持する。この順序は変更しない。

pit publisher は active run がまだない間は marker を検出しても tick を生成しない。新しい run ID を受け取った時点で
全 tracker を reset し、その後の新しい entry だけを対象にする。

Race Control command / timing queue が詰まっている状態で pit API を同じ queue へ入れると、片方の障害がもう片方を
停止させる。通信 loop と retry state は分離する。

## ログと診断

通常ログは状態遷移と request 結果に限定する。

```text
[MomoPitRecovery] quadrant=2 carId=CP-3 entry started
[MomoPitRecovery] carId=CP-3 tick=1 queued
[MomoPitRecovery] carId=CP-3 tick=1 applied recovered=20 hp=72
[MomoPitRecovery] quadrant=2 entry ended reason=marker_lost
```

毎フレームの detection と token はログへ出さない。診断 UI または development log では次を確認できるようにする。

- quadrant / carId
- OUTSIDE / ENTER_CANDIDATE / ACTIVE / TICK_PENDING
- current entryId の短縮表示
- accepted tick
- presence elapsed
- missing grace elapsed
- pending age / retry count / last HTTP status
- active raceRunId の短縮表示

## 必須 EditMode tests

### Presence tracker

- 1 frame の検出だけでは ACTIVE にならない
- enter threshold 後に ACTIVE になる
- ACTIVE から 2 秒未満では tick を作らない
- 2 秒で tick 1 を 1 回だけ作る
- request pending 中は次 tick を作らない
- tick 1 accepted 後、さらに 2 秒で tick 2 を作る
- 同じ ID が複数検出されても 1 回として扱う
- 短い未検出では entry を維持する
- grace 超過で entry を終了する
- video invalid、ArUco OFF、run change で reset する

### Contract / publisher policy

- JSON が API version 1 の field 名と完全一致する
- `carId` は `ResolveRaceControlCarId(quadrant)` の結果になる
- success と duplicate を同じ accepted として扱う
- timeout / 5xx / 429 と限定 3 種類の transient 409 では同じ commandId と本文を使う
- その他の permanent 4xx では自動再送しない
- exit / run change 後は pending response を適用しない
- 車両 A の retry が車両 B の送信を止めない
- active run が空なら送信しない
- gameplay token をログへ含めない
- local lifecycle が start 前、Stop 後、countdown cancel 後なら送信しない
- run ID 取得前の presence を新 run の entry へ引き継がない
- settings file がない、token が空、URL が不正なら fail closed にする

既存の `MomoRaceControlTimingContractEditModeTests.cs` と同じく、DTO serialization と policy を Unity lifecycle から
切り離してテストする。HTTP 自体は fake transport interface を注入し、EditMode test で status と response body を返せる形にする。

本番配置と end-to-end の確認は
[PIT 回復機能 本番適用 Runbook](PIT_RECOVERY_PRODUCTION_ROLLOUT.md) に従う。

## 結合試験

### Preflight

MADSYSTEM を変更する前に Relay 単体を確認する。

```powershell
Set-Location <src-root>\momo\tools\momo-relay
go test ./...
go build -trimpath -o "$env:TEMP\momo-local-relay.exe" .
```

Relay 起動後は `/api/v1/status` で次を確認する。

- Race Control が接続済み
- active `raceRunId` が存在する
- 対象 source の `raceCarId` が一意
- `vehicleHealth.recoveryMode` が `pit-marker`
- gameplay API の bind address が意図した interface だけ

token や Authorization header を screenshot、issue、Unity log へ残さない。

1. Relay を `-health-recovery-mode=pit-marker` と gameplay token 付きで起動する
2. Race Control、Relay、Observer、MADSYSTEM を起動する
3. `/api/v1/status` で対象 source の `vehicleHealth.recoveryMode` が `pit-marker` であることを確認する
4. damage を発生させ、HP が 100 未満になることを確認する
5. 1 秒だけ marker を見せ、回復しないことを確認する
6. 2 秒継続して見せ、20 HP 回復することを確認する
7. 4 秒継続して見せ、合計 40 HP 回復することを確認する
8. 同じ ID の marker 間を移動しても未検出猶予内なら entry が継続することを確認する
9. marker を外した後に通信を復旧しても、古い tick が送られないことを確認する
10. 4 象限それぞれが正しい `carId` だけを回復することを確認する
11. Race Control 切断、run 変更、finished では API が回復を受理しないことを確認する
12. 2 台目の監視専用 Observer から pit API が送信されていないことを確認する

## 実装順序

1. 作業開始時の MADSYSTEM dirty files と baseline commit を記録する
2. API contract DTO と fake transport interface を追加する
3. 純粋な `PitMarkerPresenceTracker` と EditMode tests を作る
4. fake transport を使う `MomoPitRecoveryPublisher` policy tests を作る
5. `TryGetActiveRaceRunId` と local lifecycle reset を接続する
6. machine-local settings と UnityWebRequest transport を接続する
7. `ArUcoWebCamMulti` の既存検出結果へ象限別 PIT observation 出力だけを追加する
8. replay / shared memory 入力で 4 象限を確認する
9. Relay と結合し、1 秒 / 2 秒 / 4 秒の回復を確認する
10. Stop、run change、Race Control disconnect、marker exit の fail-closed を確認する

画像認識、状態機械、HTTP を一度に `ArUcoWebCamMulti` へ書き込まない。最初に tracker と contract をテスト可能な
純粋 C# として固定し、その後で既存フレーム処理へ接続する。

## Definition of done

MADSYSTEM root で次を実行する。

```powershell
.\tools\Invoke-UnityValidation.ps1 -Action Compile
.\tools\Invoke-UnityValidation.ps1 -Action EditMode
.\tools\Invoke-UnityValidation.ps1 -Action WindowsBuild
```

全 EditMode に既存 failure がある場合は、作業開始前の baseline と比較し、PIT 関連 fixture が成功していることと
failure が増えていないことを分けて記録する。既存 failure を PIT 実装の成功扱いにして無視せず、逆に unrelated
failure を直す変更も PIT commit へ混ぜない。

- 新規 PIT 関連 EditMode tests がすべて成功する
- Unity Compile と Windows Build が成功する
- 既存 timing / command / sector tests に regression がない
- 1 秒では回復せず、2 秒で `+20 HP`、4 秒で合計 `+40 HP` になる
- `duplicate` response で HP が二重回復しない
- Stop / exit / run change 後に古い request が送られない
- 4 quadrant が Relay の正しい `carId` と一致する
- `VHS` の HP / speed cap が Pilot と Web Observer の authoritative 表示になる
- token、local settings、生成 binary を commit しない
- MADSYSTEM の既存 unrelated dirty changes を PIT commit に含めない

MADSYSTEM 側の実装完了だけでは end-to-end 完了ではない。最後に Race Control、Relay、Native Observer、
MADSYSTEM、最低 1 台の実車を同時起動し、phase、run、marker presence、HP、speed cap の順で確認する。
