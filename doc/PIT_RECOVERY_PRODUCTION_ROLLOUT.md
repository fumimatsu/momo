# PIT 回復機能 本番適用 Runbook

## 目的

MADSYSTEM が PIT marker ID `49` を認識している間、2 秒ごとに Relay へ回復 tick を送り、
Relay が車体 HP を 20 回復して Pilot Viewer と Observer へ反映する構成を別環境へ適用する。

HP、回復量、速度上限の正本は Relay に置く。MADSYSTEM は marker presence と tick の順序だけを管理し、
Viewer は Relay が配信する `VHS:1` を表示する。Viewer から PIT API を呼ばない。

```text
MADSYSTEM
  marker ID 49 / 2 秒継続
      |
      | POST /api/v1/gameplay/pit-recovery-ticks
      v
Relay :8090 ---- Race Control WebSocket ---- Race Control
      |
      +---- VHS:1,<hp>,<speedCap>,<mode> ---- Pilot Viewer / Observer
```

## 2026-08-10 時点の実装基準

| Repository | Branch / commit | 必要な内容 |
| --- | --- | --- |
| `fumimatsu/momo` | `master` / `ad093ed` 以降 | PIT API、HP +20、`VHS:1` 配信 |
| `fumimatsu/MADSYSTEM` | `codex/pit-recovery-publisher` / `281dea044` 以降 | marker presence、2 秒 tick、再送、Stop 処理 |
| `fumimatsu/momo-race-control` | `main` | active `raceRunId` と race phase の配信 |

MADSYSTEM の PIT branch が main へ統合された後は、統合 commit を基準に読み替える。
commit hash だけでなく、後述のテストと実行時ログで機能を確認する。

## 配置条件

- Relay と MADSYSTEM は同じ private LAN に置く。
- Relay の `8090/tcp` を Internet へ公開しない。
- MADSYSTEM が別 PC の場合、`-gameplay-allow-cidr` には MADSYSTEM PC の固定 IP `/32` を指定する。
- Relay は Race Control へ WebSocket 接続できること。
- `device ID -> carId` の対応を Relay と MADSYSTEM で一致させる。
- PIT marker は MADSYSTEM 既存の `DICT_4X4_50`、marker ID `49` を使う。
- MADSYSTEM の UI は `useFPVRC=true`、`OptionMode=PitIn` を使う。

## Token の分離

| Token | 保持するプロセス | 用途 |
| --- | --- | --- |
| `MOMO_RELAY_GAMEPLAY_TOKEN` | Relay、MADSYSTEM | PIT tick 専用 |
| `VIEWER_TOKEN` | Race Control、Relay | race state の購読 |
| `TIMING_INGEST_TOKEN` | Race Control、MADSYSTEM | timing run と snapshot |
| `RACE_CONTROL_TOKEN` | Race Control、MADSYSTEM | lifecycle command |
| `ADMIN_TOKEN` | Race Control 管理者だけ | 管理 API |

`GAMEPLAY_TOKEN` を Viewer、車体、Git 管理ファイルへ渡さない。Relay の command line、log、画面にも出さない。
別 PC 間では HTTP で token が平文になるため、信頼できる private LAN または VPN 内だけで使う。

## 1. 変更前の記録

Relay PC と MADSYSTEM PC の両方で、未コミット差分と commit を保存する。

```powershell
git status --short --branch
git rev-parse HEAD
git rev-list --left-right --count '@{u}...HEAD'
```

Relay PC では次も保存する。

```powershell
Get-CimInstance Win32_Process |
  Where-Object Name -Like 'momo-local-relay*.exe' |
  Select-Object ProcessId, ExecutablePath, CommandLine

Invoke-RestMethod http://127.0.0.1:8090/api/v1/status |
  ConvertTo-Json -Depth 8
```

次を記録する。

- 現行 Relay exe の SHA256
- Relay の起動引数と source 順
- `device ID -> carId` の対応
- Race Control URL と race ID
- MADSYSTEM PC の固定 IP
- 現行 Viewer の URL
- rollback 用 Relay exe の保存先

token の値は記録へ含めない。

## 2. Relay の更新

実行中 Relay を止める前に candidate を build する。

```powershell
Set-Location C:\src\momo
git fetch --all --prune
git pull --ff-only origin master
git status --short --branch

Set-Location .\tools\momo-relay
go test ./...
go build -trimpath -o momo-local-relay-device-input-v15.candidate.exe .
Get-FileHash .\momo-local-relay-device-input-v15.candidate.exe -Algorithm SHA256
```

既存 exe を日時付きで退避し、candidate を運用名へ切り替える。起動中 exe を直接上書きしない。
`tools/start-mads-observer.ps1` を使う環境では運用名を
`momo-local-relay-device-input-v15.exe` にする。

Viewer の正本を別途更新した場合だけ、build 前に `tools/sync-relay-viewer.ps1` を実行する。
PIT 回復だけなら Viewer の追加変更は不要である。

## 3. Relay の本番設定

Relay と Race Control が同じ PC、MADSYSTEM が別 PC の例を示す。

```powershell
$env:MOMO_RELAY_GAMEPLAY_TOKEN = '<新しい GAMEPLAY_TOKEN>'

Set-Location C:\src\momo
.\tools\start-mads-observer.ps1 `
  -HealthRecoveryMode 'pit-marker' `
  -RaceControlUrl 'ws://127.0.0.1:8787/ws/races/race-test' `
  -RaceControlViewerToken '<VIEWER_TOKEN>' `
  -GameplayAllowCidr '<MADSYSTEM-PC-IP>/32'
```

独自の起動スクリプトを使う場合も、次の引数を欠かさない。

```text
-health-recovery-mode pit-marker
-race-url ws://<race-control-host>:8787/ws/races/<raceId>
-race-viewer-token <VIEWER_TOKEN>
-gameplay-allow-cidr <MADSYSTEM-PC-IP>/32
-race-car <device>=CP-1
-race-car <device>=CP-2
-race-car <device>=CP-3
-race-car <device>=CP-4
```

`-source` と `-race-car` の対応がずれると、別車両の HP を回復する。表示順だけを見て判断せず、
`GET /api/v1/status` の各 source にある `id` と `raceCarId` を確認する。

現行 `tools/start-mads-observer.ps1` の固定対応は次である。

```text
11.3 -> CP-1
11.4 -> CP-2
11.5 -> CP-3
11.6 -> CP-4
```

当日の枠順が `11.5、11.6、11.3、11.4` などに変わる場合、このスクリプトを無変更で使ってはならない。
運用 profile または直接起動引数で `-source` と `-race-car` を同時に変更し、変更後の status を保存する。

## 4. MADSYSTEM の更新と設定

MADSYSTEM は PIT 実装を含む commit から Compile、EditMode test、Windows Build を実行する。
既存の TTS、scene、ProjectSettings などの未コミット差分を PIT branch へ混入させない。

main へ統合する前に別環境で確認する場合は、PIT branch を明示して取得する。

```powershell
Set-Location C:\src\MADSYSTEM
git fetch origin
git switch codex/pit-recovery-publisher
git pull --ff-only origin codex/pit-recovery-publisher
git rev-parse HEAD

.\tools\Invoke-UnityValidation.ps1 -Action Compile
.\tools\Invoke-UnityValidation.ps1 -Action EditMode
.\tools\Invoke-UnityValidation.ps1 -Action WindowsBuild
```

`git rev-parse HEAD` が少なくとも `281dea044` の内容を含むことを確認する。main 統合後は branch 名を
main に読み替え、統合 commit を記録する。

machine-local 設定は次にある。

```text
%USERPROFILE%\AppData\LocalLow\MADX\MADSYSTEM\MomoRaceControl\settings.json
```

変更前に backup を作成し、既存 field を消さずに次を追加する。

```json
{
  "pitRecoveryEnabled": true,
  "relayGameplayBaseUrl": "http://<relay-host>:8090",
  "relayGameplayToken": "<Relay と同じ GAMEPLAY_TOKEN>"
}
```

同じファイルの次の値も本番配置と一致させる。

```json
{
  "raceControlBaseUrl": "http://<race-control-host>:8787",
  "raceId": "race-test",
  "sourceId": "MADSYSTEM-01",
  "carIds": "CP-1,CP-2,CP-3,CP-4"
}
```

`raceControlTimingToken` と `raceControlCommandToken` も Race Control と一致させる。
設定値や token を repository へコピーしない。

`carIds` は MADSYSTEM の 4 象限を左上、右上、左下、右下の順に Relay の `carId` へ対応させる。
Observer の合成順、Relay の source 順、`-race-car` の 3 つが同じ車両を指すことを、実車を 1 台ずつ
映して確認する。文字列が `CP-1,CP-2,CP-3,CP-4` でも、映像の並びが違えば誤回復する。

## 5. 起動順

1. Race Control を起動する。
2. Relay を `pit-marker` mode で起動する。
3. Relay の Operations API で source と `raceCarId` を確認する。
4. Observer と Pilot Viewer を接続する。
5. MADSYSTEM を起動する。
6. MADSYSTEM で `useFPVRC` を有効にする。
7. `Practice`、`Qualy Rounds`、`Final` のいずれかを選ぶ。
8. `OptionMode` を `PitIn` にする。これは内部の `OptionType=2` である。
9. MADSYSTEM からレースを開始する。

Relay の Race Control WebSocket が未接続、active run が不一致、phase が `green` 以外の場合、
PIT tick は回復へ進まない。

## 6. 段階確認

### A. 起動確認

Relay PC で次を確認する。

```powershell
$status = Invoke-RestMethod http://127.0.0.1:8090/api/v1/status
$status.sources | Select-Object id, raceCarId, state,
  @{Name='recoveryMode'; Expression={$_.vehicleHealth.recoveryMode}},
  @{Name='hp'; Expression={$_.vehicleHealth.hp}}
```

合格条件:

- 4 source の `id` と `raceCarId` が当日の枠割りと一致する。
- `vehicleHealth.recoveryMode` が `pit-marker` である。
- 使用車両が `STREAMING` である。
- Race Control log に WebSocket `101 Switching Protocols` がある。
- Viewer に HP bar が表示される。

### B. lifecycle 確認

MADSYSTEM で Practice を開始する。Race Control で次を確認する。

- 準備時に `ready`
- カウントダウン中に `countdown`
- 実計測開始時に `green`
- 新しい `raceRunId`
- Stop 後に `finished`

### C. 実ダメージと回復

1. 対象車両へ IMPACT または HEAVY IMPACT を発生させる。
2. Viewer の HP が 100 未満へ減り、速度上限も下がることを確認する。
3. 対象象限で marker ID `49` を連続して 2 秒以上認識させる。
4. 2 秒ごとに HP が最大 20 ずつ増えることを確認する。
5. HP が 100 を超えないことを確認する。
6. HP 回復に合わせて Viewer の HP bar と速度上限が戻ることを確認する。

MADSYSTEM の期待ログ:

```text
[MomoPitRecovery] carId=CP-3 tick=1 queued
[MomoPitRecovery] carId=CP-3 tick=1 applied recovered=20 hp=...
```

Relay の期待ログ:

```text
source "<device>" car "CP-3": applied pit recovery entry="..." tick=1 recovered=20.0 hp=...
```

HP が 100 の場合も `200 applied` になるが、`recoveredAmount=0` になる。これは異常ではない。

### D. Stop と再開

1. marker を映したまま Stop を押す。
2. MADSYSTEM に `entry ended reason=RaceStopped` が出ることを確認する。
3. 5 秒以上待ち、追加 tick がないことを確認する。
4. Race Control が `finished` になることを確認する。
5. 次のレースを開始し、新しい `raceRunId` と新しい `entryId`、tick 1 で再開することを確認する。

### E. 通信遅延時の確認

次の `409` だけは、marker entry と race が有効な間、同じ command ID と同じ JSON 本文で再送する。

- `race_control_unavailable`
- `race_run_mismatch`
- `phase_not_allowed`

`tick_out_of_sequence`、`entry_id_reused`、`command_id_conflict`、`recovery_mode_not_allowed` は
設定または実装不整合として再送を停止する。無限再送で隠さず、Relay と MADSYSTEM の設定を直す。

## 7. Viewer の確認

Viewer は PIT API を認識する必要がない。Relay が回復後に配信する
`VHS:1,<hp>,<speedCap>,<mode>` を受け、既存の HP bar と走行制限へ反映する。

PIT tick が Relay で `applied` なのに Viewer が変化しない場合は、次を順に調べる。

1. Relay status の対象 source で HP が増えたか。
2. `carId` と Viewer の対象車両が一致しているか。
3. Viewer の telemetry DataChannel が open か。
4. Viewer が `VHS:1` 対応版か。
5. Relay 更新後に Viewer を再読み込みしたか。

Relay status の HP も増えていなければ Viewer の問題ではない。MADSYSTEM の request、Race Control phase、
Relay の受理条件を調べる。

## 8. Rollback

本番試験に失敗した場合は次の順で戻す。

1. MADSYSTEM のレースを Stop する。
2. MADSYSTEM を終了する。
3. Relay を終了する。
4. 退避した Relay exe を運用名へ戻す。
5. MADSYSTEM の machine-local settings を backup から戻す。
6. Relay を従来の `legacy` mode で起動する。
7. Operations API と Viewer で従来動作を確認する。

`legacy` は安全走行による従来回復を有効にする。回復自体を止める場合は `disabled` を使う。
問題解析が終わるまで、新旧 binary、hash、Relay log、MADSYSTEM `Player.log` を保持する。

## 9. 本番適用記録

次を日付付きで残す。

- 各 repository の commit
- Relay exe と MADSYSTEM exe の SHA256
- Relay host、Race Control host、MADSYSTEM host
- source 順と `device ID -> carId`
- recovery mode と許可 CIDR
- 試験した race ID と raceRunId
- damage 前 HP、damage 後 HP、各 tick 後 HP
- Stop 後に追加 tick がなかった確認時間
- rollback の有無

token、秘密鍵、password は記録しない。

## 2026-08-10 ローカル結合実績

ローカル Race Control、ダミー source の Relay、別 Relay から映像を受ける Observer、MADSYSTEM を使い、
次を確認した。

- Practice / `OptionMode=PitIn`
- `CP-3` で marker ID `49` を連続認識
- tick 1 から 38 まで 2 秒周期で `200 applied`
- HP 100 のため各 `recoveredAmount` は 0
- marker を映したまま Stop し、`entry ended reason=RaceStopped`
- Stop 後 10 秒以上、追加 tick なし
- Race Control は `finished`

この試験は MADSYSTEM から Relay までの送信と Stop 処理を確認した。実ダメージ後の HP +20、
速度上限回復、Viewer 表示は本番 Relay で確認する。
