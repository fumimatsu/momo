# Relay 配信型レース音声サービス設計

## 状態

この文書は実装前の契約と本番適用手順を固定する。対象は Relay Pilot の遠隔利用者へ配信する
LAP、GOAL、PIT、接近警告などの英語・日本語アナウンスである。M5Audio の車載音声経路、
操縦 command、Telemetry の契約は変更しない。

初期構成は英語を既定、日本語を選択可能な副言語とする。

- 英語: Kokoro-82M を第一候補にする。
- 日本語: Kokoro-82M と VOICEVOX を実機文言で比較し、VOICEVOX をフォールバックに残す。
- Viewer の Web Speech API は、生成音声が期限内に届かない場合のフォールバックとして維持する。
- LLM に自由文実況を生成させない。最初は Relay が確定した事実だけをテンプレートへ埋め込む。

Kokoro のモデルと voice は [Kokoro-82M](https://huggingface.co/hexgrad/Kokoro-82M)、
VOICEVOX の内部 API は [VOICEVOX ENGINE API](https://voicevox.github.io/voicevox_engine/api/)
を参照する。モデル、voice、話者ごとの利用条件は本番配布前に確認する。

## 採用する経路

```text
Race Control race_state v2
          |
          v
Relay event detector
          |
          +--> race-audio-service (loopback only)
          |       +--> Kokoro EN / JA
          |       +--> VOICEVOX JA fallback
          |       +--> Ogg Opus cache
          |
          +--> momo-race DataChannel: event metadata and asset URL
                                      |
                                      v
                              Relay Pilot Viewer
                                      |
                                      +--> HTTPS GET audio asset
                                      +--> Web Audio playback
                                      +--> Web Speech fallback
```

音声バイナリを DataChannel へ載せない。DataChannel は再生対象、優先度、期限、音声 URL、
フォールバック文だけを送る。生成済み音声は HTTP / HTTPS で取得する。

この分離には次の理由がある。

- 大きい reliable DataChannel message が操縦、Telemetry、race state と同じ SCTP association を
  詰まらせることを避ける。
- ブラウザと CDN の cache を利用し、同じ定型音声を複数 Pilot へ再送しない。
- TTS 停止時も Relay の映像、操縦、Race Control 中継を継続する。
- 言語ごとの音声を同じ event へ紐付け、Viewer 側で 1 言語だけ選べる。

## コンポーネント境界

### race-audio-service

Relay とは別プロセスで動かす。初期実装は Python の小さい HTTP service とし、TTS engine の
起動、生成、変換、disk cache だけを担当する。Relay の Go process 内で model inference をしない。

内部 API は `127.0.0.1` のみに bind する。

```text
POST /v1/synthesize
GET  /healthz
```

`POST /v1/synthesize` の要求例:

```json
{
  "eventKey": "race-test/run-42/CP-1/lap-4",
  "language": "en-US",
  "engine": "kokoro",
  "voice": "configured-english-voice",
  "text": "Lap four complete. Thirteen point four four four seconds. Personal best. Position two.",
  "speed": 1.0,
  "format": "ogg-opus"
}
```

応答は cache key、SHA-256、content type、duration、内部保存先を返す。cache key には engine、
model version、voice、language、text、speed、codec をすべて含める。同じ key の同時生成は
1 request にまとめる。

出力は Chrome で直接再生できる mono Ogg Opus に統一する。TTS engine の WAV や異なる
sample rate は service 内で正規化し、Viewer に engine 固有形式を見せない。

### Relay

Relay は `race_state v2` の sequence と車両別状態から、各イベントを 1 回だけ確定する。
TTS へ渡す文は server 側の固定テンプレートから作り、Viewer から任意文字列を受理しない。

実装時に次の設定を追加する。

```text
-race-audio-service-url http://127.0.0.1:18090
-race-audio-public-base-url https://<audio-public-host>/fpv-audio
-race-audio-cache-dir C:\fpv-race-audio-cache
-race-audio-default-language en-US
```

環境変数を併用する場合も token、voice license 情報、外部 service credential を command line や
Git 管理ファイルへ入れない。

Relay が公開する read-only API:

```text
GET  /api/v1/race-audio/assets/<sha256>.ogg
HEAD /api/v1/race-audio/assets/<sha256>.ogg
```

- `<sha256>` 以外の path を拒否する。
- 応答は `Content-Type: audio/ogg` と immutable cache header を付ける。
- synthesize、cache 削除、任意ファイル取得 API は外部公開しない。
- disk cache に上限と最終参照時刻ベースの削除を設ける。
- TTS timeout や cache miss は race state 配信を失敗させない。

### DataChannel 契約

既存の reliable `momo-race` DataChannel に、`RACE:` と区別できる prefix を追加する。

```text
RACE_AUDIO:{json}
```

version 1 の例:

```json
{
  "type": "race_audio",
  "version": 1,
  "eventId": "run-42:CP-1:lap:4",
  "raceId": "race-test",
  "raceRunId": "run-42",
  "sourceId": "11.3",
  "carId": "CP-1",
  "kind": "lap_complete",
  "priority": 40,
  "expiresInMs": 3000,
  "interrupt": false,
  "fallbackText": {
    "en-US": "Lap four complete. Thirteen point four four four seconds. Personal best. Position two.",
    "ja-JP": "4周目、13秒444。自己ベスト。現在2位。"
  },
  "assets": {
    "en-US": {
      "url": "https://<audio-public-host>/fpv-audio/<sha256-en>.ogg",
      "contentType": "audio/ogg",
      "sha256": "<sha256-en>"
    },
    "ja-JP": {
      "url": "https://<audio-public-host>/fpv-audio/<sha256-ja>.ogg",
      "contentType": "audio/ogg",
      "sha256": "<sha256-ja>"
    }
  },
  "ducking": {
    "m5AudioGain": 0.4,
    "attackMs": 80,
    "releaseMs": 250
  }
}
```

- `eventId` は `(raceRunId, carId, kind, event sequence)` から決定し、再接続後も同一イベントを
  二重再生しない。
- `expiresInMs` は Viewer 受信時点からの相対期限とし、Relay と Viewer の wall clock 差に依存しない。
- `assets` は 0 件を許可する。その場合は `fallbackText` を Web Speech API で再生する。
- Viewer は `en-US`、`ja-JP`、`off` のいずれか 1 つを選び、英語と日本語を同時再生しない。
- 未知の `version`、`kind`、不正 URL、SHA-256 不一致は再生しない。

## イベントと優先順位

初期イベントは次に限定する。

| kind | 内容 | 既定優先度 |
| --- | --- | ---: |
| `safety_stop` | STOP、安全停止 | 100 |
| `blue_flag` | 周回遅れ警告 | 90 |
| `rear_approach` | 後続車接近 | 80 |
| `race_finish` | GOAL、順位 | 70 |
| `pit_enter` / `pit_exit` | PIT 状態 | 60 |
| `fuel_low` / `fuel_empty` | Fuel 警告 | 55 |
| `final_lap` | 最終 LAP | 50 |
| `lap_complete` | LAP、time、best、順位 | 40 |
| `boost_ready` | Boost 満了 | 20 |

Viewer の Notification Controller と同じ優先順位へ集約する。高優先度は低優先度を中断できるが、
同じ kind の連打は cooldown と `eventId` で抑止する。LAP 読み上げ中でも STOP と安全警告を待たせない。

## 文言生成

初期版は決定的な template だけを使う。

```text
Lap four complete. Thirteen point four four four seconds. Personal best. Position two.
Final lap. Position three. Gap ahead, zero point eight seconds.
Race finished. Position two.
Blue flag. Faster car approaching.
Fuel empty. Return to the pit.
```

日本語も同じ事実から別 template で作る。翻訳 service や LLM を走行中の必須依存にしない。
pilot name、順位、time は Race Control / Relay の authoritative value だけを使う。

Kokoro は極端に短い発話で品質が落ちる可能性があるため、単語 fragment を大量に連結せず、
1 イベントを自然な 1～2 文として生成する。start signal の Red / Green は遅延を避けるため、
引き続き Viewer の Web Audio tone を使い、TTS 対象にしない。

## 外部 Ayame Pilot の HTTPS 経路

公開 Pages から開いた Viewer は、LAN の `http://192.168.11.100:8090` を直接取得できない。
HTTPS page から HTTP asset を読む mixed content も許可しない。そのため外部 Ayame 運用では、
`-race-audio-public-base-url` が指す HTTPS endpoint が必要である。

推奨構成は 11.100 から外向き tunnel を張り、音声 asset 専用の read-only port だけを公開する方法である。

```text
Public Viewer
    |
    v
https://audio.<domain>/fpv-audio/<sha256>.ogg
    |
Cloudflare Tunnel または同等の outbound tunnel
    |
11.100 read-only asset endpoint
```

- Relay の `/ws`、operations、gameplay、source admin を同じ public hostname へ出さない。
- TTS 内部 API `POST /v1/synthesize` を tunnel 対象にしない。
- CORS は公開 Viewer origin と本番 Relay origin に限定する。
- asset 名は内容 hash とし、directory listing を無効にする。
- tunnel が停止した場合は Viewer の Web Speech fallback へ移行する。

既存 VPS を使う場合は、VPS から 11.100 へ到達できる VPN / tunnel がある時だけ reverse proxy する。
VPS が LAN へ到達できない状態で Caddy の proxy 設定だけを追加しても動作しない。

## 実装順

### 1. `momo` repository

1. `tools/race-audio-service/` に service、engine adapter、cache、health check を追加する。
2. Kokoro EN / JA と VOICEVOX JA adapter を同じ synthesize interface に合わせる。
3. Relay に event detector、template renderer、TTS client、asset handler を追加する。
4. `momo-race` DataChannel の `RACE_AUDIO:` 送信と再接続時の現在イベント同期を追加する。
5. `tools/start-mads-observer.ps1` に service 起動、health 待ち、Relay 設定を追加する。
6. TTS process の CPU、memory、生成時間、cache hit、error を Operations status へ追加する。

### 2. `momo-fpv-viewer` repository

1. `RaceAudioPlayer` を追加し、Notification Controller と同じ priority queue を使う。
2. 言語設定 `en-US` / `ja-JP` / `off` を local storage へ保存する。
3. asset を fetch して SHA-256 を確認し、Web Audio で再生する。
4. CONNECT の user gesture で AudioContext を解除する。
5. 再生中だけ M5Audio gain を下げ、終了・中断・error 時に必ず復元する。
6. asset timeout、HTTP error、decode error は `fallbackText` の Web Speech へ移行する。
7. Direct Viewer は当面 Web Speech を維持し、Relay 専用契約へ依存させない。

### 3. `momo-fpv` repository

1. 11.100 の local config、service install、tunnel、cache directory を運用文書へ追加する。
2. 一括起動ツールへ race-audio-service の health check を追加する。
3. fleet status に TTS engine、model version、cache、直近生成時間を追加する。

Race Control と MADSYSTEM は初期版では変更しない。Relay が既存 `race_state v2` から必要な
イベントを確定できない項目が見つかった場合だけ、別途 schema を拡張する。

## 11.100 への適用手順

実装完了後は次の順で適用する。

1. `momo`、`momo-fpv-viewer`、`momo-fpv` の指定 commit を取得する。
2. 既存 Relay config、環境変数、実行ファイル、cache を backup する。token 値は記録へ残さない。
3. TTS 専用環境を作り、Kokoro model、英語 voice、日本語 voice、VOICEVOX Engine を配置する。
4. race-audio-service を loopback で起動し、`/healthz` と英日 sample 生成を確認する。
5. 定型文を prewarm し、英語・日本語の生成時間、duration、音割れ、固有名詞を確認する。
6. Relay の Go test と build、Viewer の Node test と relay distribution build を実行する。
7. LAN 内の 1 Pilot だけで `lap_complete`、`final_lap`、`race_finish` を確認する。
8. 音声専用 HTTPS endpoint を公開し、外部 Ayame Pilot 1 接続で asset 取得を確認する。
9. 4 Pilot 同時接続で cache hit、DataChannel latency、映像 FPS、command delay を確認する。
10. 問題がなければ service の自動起動を有効にする。

## 受け入れ条件

- TTS service 停止中も映像、操縦、Telemetry、Race Control が継続する。
- 同じ `eventId` を再接続後に二重再生しない。
- 生成済み asset は複数 Viewer で cache hit する。
- Viewer の選択言語以外を再生しない。
- asset が取得できない時は期限内なら Web Speech へ移行し、期限後は読み上げない。
- STOP / safety warning は LAP 音声を中断できる。
- TTS 中断後に M5Audio gain が必ず元へ戻る。
- 外部公開 endpoint から Relay の操作 API と TTS synthesize API へ到達できない。
- 4 台走行中に Relay の command delay、映像 FPS、DataChannel queue が基準値から悪化しない。

## ロールバック

Relay の race audio 設定を無効にし、race-audio-service と tunnel を停止する。Viewer は
`RACE_AUDIO:` が届かなければ既存の Web Speech / Web Audio 実装を使う。M5Audio、Race Control、
FFB、車体 firmware の rollback は不要である。
