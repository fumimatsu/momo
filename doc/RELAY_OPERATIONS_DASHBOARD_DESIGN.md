# Relay Operations Dashboard 設計

## 状態

`implemented-runtime-validation-pending`。設計レビューを通過し、Relay の status API と Operations
画面を実装済みである。実機 source を使った負荷・watchdog・運用表示の確認は未完了である。

## 目的

Local Momo Relay が source ごとに受信・転送している状態を、会場 LAN の運営端末から確認できる
ようにする。映像を追加受信・デコードせず、Relay がすでに保持する WebRTC / RTP / DataChannel
の状態と軽量なカウンタだけを表示する。

この画面は Multi Observer の代替ではない。Relay の transport 状態と、Observer 側の実際の復号／
表示状態を分離して運用する。

## 非目標

- source 映像のプレビュー、録画、サムネイル生成をしない。
- Pi、Observer、Unity、Race Control の操縦または設定を操作しない。初期版は読み取り専用である。
- インターネットへ公開しない。Operations endpoint は既定で loopback だけを許可し、会場 LAN から
  開く時だけ `-operations-allow-cidr` で管理用サブネットを明示許可する。
- 外部 Ayame Pilot の実装を含めない。将来の Relay 下流状態として追加できる余地だけ残す。

## 利用者と画面

運営者は Relay の HTTP server から次を開く。

```text
http://<relay-host>:8090/operations.html
```

- 既定は **Grid**。source 番号を大きく表示するカード型で、4 台程度を即読する。
- **List** は source 数が増えた時の比較・障害切り分け用である。
- 右上の Grid / List アイコンで切り替える。選択はブラウザの local storage に保存する。
- 両ビューは同じ status API を 1 秒ごとに poll する。status WebSocket は初期版に入れない。
- 正常、待機、警告、異常／復旧中は、緑、シアン、黄、赤を使う。ただし色だけに依存せず、
  `STREAMING`、`WAITING`、`STALE`、`RECOVERING` のテキストを必ず表示する。

## 表示項目と根拠

| 区分 | 項目 | Relay 側の根拠 | 初期版の定義 |
| --- | --- | --- | --- |
| 識別 | source、`carId` | `-source`、`-race-car` 設定 | source 設定順で表示。4 台固定にしない。 |
| 状態 | `WAITING` / `CONNECTING` / `STREAMING` / `STALE` / `RECOVERING` / `DISCONNECTED` | 新設する source lifecycle と video health | lifecycle と映像進行を分けて排他的に導出する。 |
| 上流 | WebRTC connection state、serial DataChannel state | 明示的に保持する upstream `PeerConnection` state、`serial` DataChannel | `connected` のみで映像正常と判断しない。 |
| 映像入力 | 最終 RTP age、ingress access-unit FPS | RTP timestamp が進んだ時刻、RTP marker bit の受信数 | Pi から Relay へ届く映像の進行を示す。未受信の age は `null`。 |
| Relay 転送 | relay-write access-unit FPS、直近転送エラーコード | marker bit の RTP を `TrackLocalStaticRTP.WriteRTP` が受理した数 | 下流 peer の送信成功やブラウザの復号完了を保証しない。 |
| 下流 | Pilot lease、negotiating / connected Pilot・Observer 数 | Pilot 予約、Viewer role、各 viewer の明示的な PeerConnection state | SDP answer 後と WebRTC connected 後を区別する。 |
| Channel | telemetry / race の open 数 | Viewer の DataChannel open 状態 | 映像受信成功や telemetry payload freshness を意味しない。 |
| 復旧 | PLI reason 別回数、RTP stall 回数、retry attempt 数、固定 error code | watchdog、source 接続ループ、明示カウンタ | connection generation と retry attempt は別値として出す。 |

`ingressAccessUnitFps` と `relayWriteAccessUnitFps` は、1 秒の rolling window で access unit 数を数える。
H.264 RTP の marker bit を frame の近似境界に使う。前者は marker bit の RTP を受信した数、後者は
その RTP を `WriteRTP` が受理した数である。これは映像解析やデコードを行わないため軽量だが、
下流 WebRTC の送信成功、packet loss、Viewer が実際に画面へ表示した FPS を意味しない。

## 状態判定

Relay は source ごとに次の二つを明示的に保持する。

- `lifecycle`: `waiting`、`connecting`、`connected`、`retry_wait`、`recovering`
- `videoHealth`: `not_started`、`receiving`、`stalled`

`lifecycle` の更新契機は、Relay 起動時=`waiting`、各 upstream dial 開始=`connecting`、
PeerConnection connected=`connected`、signaling / PeerConnection の通常失敗=`retry_wait`、
watchdog の PLI grace 開始=`connected` + `videoHealth=stalled`、watchdog が source を close=`recovering`、
3 秒 retry wait 後の次回 dial=`connecting` とする。`lastRtpAgeMs` は映像未受信時に `null` とする。

画面の state は次の優先順位で一意に決める。

| 状態 | 条件 | 運営上の意味 |
| --- | --- | --- |
| `RECOVERING` | `lifecycle=recovering` | watchdog が source を閉じて復旧処理中。 |
| `DISCONNECTED` | `lifecycle=retry_wait` | signaling / PeerConnection 失敗後の retry 待機。 |
| `WAITING` | `lifecycle=waiting` | Relay 起動直後で、source の初回 dial 前。 |
| `STALE` | `lifecycle=connected` かつ `videoHealth=stalled` | RTP timeout 後、PLI grace 中。 |
| `STREAMING` | `lifecycle=connected` かつ `videoHealth=receiving` | 最新の RTP timestamp が進んでいる。 |
| `CONNECTING` | それ以外の `lifecycle=connecting` または `connected` + `not_started` | signaling / ICE / 最初の映像を待機中。 |

閾値は既存 `-rtp-stall-timeout` と `-upstream-start-timeout` を status response に含め、
画面へ明示する。画面だけに別の停止閾値を持たせない。

## API 契約

初期版は Relay 本体が次の JSON を返す。

```text
GET /api/v1/status
Cache-Control: no-store
```

```json
{
  "version": 1,
  "serverTime": "2026-07-22T12:00:00Z",
  "sources": [
    {
      "id": "11.3",
      "raceCarId": "CP-1",
      "state": "STREAMING",
      "lifecycle": "connected",
      "videoHealth": "receiving",
      "upstream": {
        "peerState": "connected",
        "serialOpen": true,
        "lastRtpAgeMs": 42,
        "ingressAccessUnitFps": 49.8,
        "relayWriteAccessUnitFps": 49.8,
        "generation": 3
      },
      "downstream": {
        "pilotLeaseReserved": true,
        "negotiatingPeers": 0,
        "connectedPilots": 1,
        "connectedObservers": 1,
        "telemetryChannelsOpen": 2,
        "raceChannelsOpen": 2
      },
      "recovery": {
        "pliRequests": { "newTrack": 1, "viewerConnect": 1, "watchdog": 1 },
        "rtpStalls": 1,
        "retryAttempts": 2,
        "lastErrorCode": null
      }
    }
  ]
}
```

API に Viewer の氏名、remote IP、認証情報、DataChannel 本文、Race token を含めない。
`lastErrorCode` は `upstream_signaling_failed`、`upstream_peer_failed`、`upstream_rtp_stalled` のような固定値だけにし、
URL・IP・token を含み得る生の `err.Error()` を返さない。

## 実装境界

### Relay Go

- source state と監視カウンタは `atomic` または既存 mutex 配下で更新する。
- 上流 `PeerConnection` state、source lifecycle、video health、last error code は接続 callback と watchdog の
  更新契機で明示的に保持する。`upstreamPC` を serial DataChannel open の有無だけで判断しない。
- `sourceOrder []string` を追加し、`-source` 指定順で status response と Grid を安定表示する。`map` の
  iteration 順へ依存しない。
- connection generation は PeerConnection 作成世代、retry attempt は接続ループの再試行回数として別に保持する。
- PLI は new-track、viewer-connect、watchdog の起因別に count する。
- status snapshot は短時間の read lock で値をコピーし、JSON encode は lock の外で行う。
- watchdog、RTP forwarding、Viewer command の goroutine に blocking I/O や UI 処理を追加しない。
- `GET /api/v1/status` と `operations.html` を既存 8090 HTTP server へ追加する。
- `/api/v1/status` と `/operations.html` は `-operations-allow-cidr` を通る source IP だけへ応答する。
  値を指定しない既定は loopback のみとし、会場では管理 LAN CIDR を起動引数へ明示する。Windows Firewall も
  同じ管理サブネットに制限する。Pilot WebSocket の既存公開範囲はこの変更で拡張しない。

### Operations HTML

- Relay の `web/` に embedded asset として置く。別プロセスや別 server は作らない。
- API 失敗時は最後の有効 snapshot を残し、`STATUS API UNAVAILABLE` と取得時刻を表示する。
- Grid/List の切替は CSS class と client-side rendering だけで行う。
- 操縦ボタン、Relay restart、Pi restart は置かない。

### Observer の実表示状態

初期版では Operations 画面に含めない。Observer の SDL 表示、共有メモリ sequence、Unity Texture
更新は別の責務である。将来必要なら、Observer が source ごとの最終 decoded frame 時刻だけを
Relay へ heartbeat する別 API を追加する。その時も映像フレーム自体は送らない。

## 検証と受け入れ条件

1. source 3 台、次に source 5 台以上で、設定順に Grid と List が表示される。
2. Viewer / Observer を増減しても、Relay の映像 RTP 転送と command forwarding に回帰がない。
3. Pi 停止、RTP 無音、復帰を意図的に起こし、`STALE`、PLI、`RECOVERING`、`STREAMING` の順を
   status API と Relay log で一致確認する。
4. Pilot lease、negotiating peer、connected Pilot / Observer、telemetry / race DataChannel の open 数が
   実接続段階と一致する。
5. status を 1 秒 poll しても、4 source の Relay CPU、ingress FPS、forward FPS、RTP age に
   有意な悪化がないことを実測する。
6. 状態遷移、marker bit の FPS window、Viewer state と DataChannel open / close、source order、HTTP method、
   `Cache-Control`、機密値非露出、CIDR access deny / allow を table-driven test で確認する。
7. `go test ./...` と `go test -race ./...`、status snapshot の単体テスト、ブラウザで Grid/List の
   静的・縮小幅表示を確認する。

## 実装順

1. Go の source status snapshot と `/api/v1/status`、単体テスト。
2. 最小の `operations.html` と polling、Grid/List 切替。
3. watchdog 意図停止試験、Pilot / Observer 接続試験。
4. 見た目の調整と、必要なら Observer heartbeat の別設計。
