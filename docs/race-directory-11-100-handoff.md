# Race Directory の 11.100 引継ぎ

## 目的

Race Directory API、local cache、Relay の DRIVE 状態、Race Control roster lock を
11.100 で段階的に確認する。現在の MADSYSTEM 固定 4 台運用は維持し、この手順だけで
Coordinator をレースの正本へ切り替えない。

## データ境界

```text
legacy event KV / D1
        |
        v
MADSYSTEM Web private Race Directory API
        |  Bearer token / ETag
        v
momo-race-directory-cache on 11.100
        |  validated event-scoped cache
        +------------------------> future Coordinator
                                      |
Relay /api/v1/status -----------------+  manual JOIN only
                                      |
                                      v
                              Race Control draft roster
                                      |
                                      v
                              timing-run locked roster
```

Relay、Viewer、Marker Node、browser は Cloudflare KV、D1、private API を直接読まない。
`vehicleId` は物理車両、`sourceId` は Relay の実行時接続、`carId` は race run 内の計測枠、
`pilotId` は D1 の永続 pilot identity として分離する。

## 2026-08-18 検証済みの土台

| Repository | Branch / commit | 確認内容 |
| --- | --- | --- |
| `cloudflare-line-echo-bot` | `origin/master` / `cd26c62` | D1 migration、private API、ETag |
| `momo-race-timing` | `codex/marker-shadow` / `1d03f58` | strict validation、atomic cache |
| `momo-race-control` | `codex/blue-flag-advisory` / `1119c70` | draft roster、revision、run lock |
| `momo` | `codex/relay-startup-ayame-cidr` / `7cda79c3` | Relay Operations API、dynamic source |

この PC で次を確認した。

- Cloudflare API: TypeScript check と Race Directory を含む 196 tests が成功
- Timing/cache: `go test ./...` が成功
- Race Control: 16 tests が成功
- Relay: Go tests が成功
- local D1 migration `0001` から `0006` と 4 台 demo seed が成功
- cache 1 回目が `updated`、2 回目が ETag により `not_modified`
- cache は 4 pilots、4 vehicles、4 entries、4 roster candidates を保持し、token を含まない
- 隔離 race への roster PUT、同一 revision 再送、timing-run lock が成功
- locked roster は同じ `directoryRevision` と 4 台の identity mapping を保持
- production endpoint は未認証要求へ `401 Bearer realm="race-directory"` を返した

Cloudflare repository は `package.json` と `package-lock.json` が不整合で、clean checkout の
`npm ci` が失敗する。今回の検証では test worktree に限り lockfile を変更せず依存を展開した。
11.100 へ恒久配置する前に upstream で lockfile を修正する。

## 11.100 の準備

想定配置は次のとおり。

```text
C:\src\momo
C:\src\momo-race-timing
C:\src\momo-race-control
```

`momo-race-timing` は `main` ではなく、Race Directory client を含む branch を使う。

```powershell
Set-Location C:\src\momo-race-timing
git fetch origin
git switch codex/marker-shadow
git pull --ff-only
.\tools\Invoke-Validation.ps1
```

11.100 の Relay は管理 LAN を Operations API に許可する。

```powershell
.\tools\start-mads-observer.ps1 `
  -RebuildRelay `
  -OperationsAllowCidr '192.168.11.0/24'
```

この PC の `192.168.11.105` から `http://192.168.11.100:8090/api/v1/status` を取得した際は
`403 operations access denied` だった。Relay 再起動後、11.100 の loopback と管理 LAN の
両方から HTTP `200` を確認する。Internet 側の CIDR は許可しない。

## 別 PC の Coordinator へ cache を渡す

11.100 を Relay 専用 host、Race Operations Coordinator を別 PC で動かす場合、cloud read token は
11.100 にだけ保存する。Coordinator host は起動時に次を取得する。

```text
GET http://192.168.11.100:8090/api/v1/coordinator-directory-cache
```

この endpoint は `operations-allow-cidr` で管理 LAN に限定され、Relay が読み込んだ完全な strict cache
envelope を返す。`team-observer-directory` は表示用に entry status、roster candidates、organization、
source bindings を削っているため Coordinator 入力として使用しない。

Coordinator host の初回設定:

```powershell
Set-Location C:\src\momo-race-timing
.\tools\Initialize-RaceDirectoryCache.ps1 `
  -RelayCacheUrl http://192.168.11.100:8090/api/v1/coordinator-directory-cache `
  -Organization madsystem `
  -Event tokorozawa-2026-08 `
  -CachePath C:\src\momo-race-timing\state\race-directory-cache.json
```

以後は Race Operations アプリケーション起動時に 1 回だけ mirror する。追加の Scheduled Task、常駐
同期、Coordinator host への cloud token 配布は行わない。

## 実データ cache の確認

専用 read token は 11.100 の process secret として配置する。command line、Git、log、
Viewer URL へ書かない。

```powershell
Set-Location C:\src\momo-race-timing
$env:RACE_DIRECTORY_READ_TOKEN = '<dedicated read token>'
$go = & .\tools\Resolve-GoExecutable.ps1

& $go run .\cmd\momo-race-directory-cache `
  -base-url https://madsystem.win `
  -organization <organization-slug> `
  -event <event-slug> `
  -cache .\state\race-directory-cache.json
```

同じ command を 2 回実行し、次を確認する。

- 1 回目: `status: updated`
- 2 回目: `status: not_modified`
- 2 回とも同じ `directoryRevision`
- event status と pilot / vehicle / entry 件数が運用台帳と一致
- cache に token、LINE ID、MAC address がない

確認後、interactive shell の token を削除する。

```powershell
Remove-Item Env:RACE_DIRECTORY_READ_TOKEN
```

## Team Observer projection

Relay は Coordinator cache を read-only で検証し、同一 origin の Team Observer へ非 secret projection
を返す。private read token は cache 更新プロセスだけに保持し、Relay の環境・引数へ渡さない。

```powershell
Set-Location C:\src\momo
.\tools\start-mads-observer.ps1 `
  -RebuildRelay `
  -OperationsAllowCidr '192.168.11.0/24' `
  -GarageAllowCidr '192.168.11.0/24' `
  -TeamObserverDirectoryCache 'C:\src\momo-race-timing\state\race-directory-cache.json' `
  -TeamObserverDirectoryOrganization '<organization-slug>' `
  -TeamObserverDirectoryEvent '<event-slug>' `
  -TeamObserverDirectoryMaxAge '24h' `
  -RaceDirectoryRefreshConfig "$env:LOCALAPPDATA\MomoFPV\race-directory\race-directory-cache.json" `
  -RaceDirectoryRefreshScript 'C:\src\momo-race-timing\tools\Invoke-RaceDirectoryCacheRefresh.ps1'
```

Relay 起動時に Directory を 1 回だけ更新する。別の updater service や Windows Scheduled Task は
起動しない。更新失敗時は既存の検証済み cache を維持して警告し、cache 自体がなければ Relay 起動を
止める。参加者、車両、event を D1 で変更した場合は明示的に refresh する。

確認する endpoint:

```powershell
Invoke-RestMethod 'http://127.0.0.1:8090/api/v1/team-observer-directory'
Invoke-RestMethod 'http://127.0.0.1:8090/api/v1/pilot-devices'
```

- projection は `vehicleId`、active `sourceId`、表示用 pilot/vehicle/event だけを含む。
- cache の ETag、organization ID、theme song、token は返さない。
- cache 未設定は `204`、invalid/missing/wrong-scope は `503`。
- max age 超過時は HTTP `200` のまま `stale: true` とし、画面に警告を出す。
- Race Directory だけで pilot と vehicle を結び付けず、run 中は locked roster を正とする。

## Relay との手動 JOIN

```powershell
$relay = Invoke-RestMethod `
  'http://127.0.0.1:8090/api/v1/status'
$relay.sources | Select-Object sourceId,state,drive
```

cache の active `sourceId` ごとに Relay source が存在し、実車の DRIVE ON/OFF と
`drive.sessionId` が対応することを確認する。`ownerViewerId` は通信診断値であり、
`pilotId` に使用しない。

隔離 race ID での手動 lock 試験は、MADSYSTEM が race を実行していない時間だけ行う。
`directoryRevision` を含む完全 roster を PUT し、同一 revision 再送が
`duplicate_roster_revision`、timing-run 作成後の roster が `locked: true` になることを確認する。
実レース ID では実行しない。

## 未実装と停止条件

次は未実装である。

- Relay DRIVE 状態と directory を自動 JOIN する Coordinator lifecycle
- read-only roster preview と operator diagnostics
- Coordinator から Race Control への自動 PUT
- Team Observer の選択外車両へ HP、PIT、確定 event を一括配信する fleet snapshot
- 5 台以上の Race Control timing 運用

次の場合は roster 作成へ進まない。

- Operations API が `403`
- cache が missing、wrong-event、invalid、または operator 未承認の stale 状態
- event が `closed` または `archived` で replay 指定がない
- vehicle または active `sourceId` が重複する
- confirmed pilot と DRIVE session の対応を確定できない
- MADSYSTEM が実レースを実行中
