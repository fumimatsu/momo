# Relay 配信型レース音声サービス

## 状態

Relay Pilot へ LAP 完了と GOAL の英語 TTS を配信する初期実装である。
音声 asset の公開 HTTPS 配信は採用しない。TTS service は内部 API として動かし、Relay が取得した
Opus packet を既存 PeerConnection の専用 audio track へ送る。

実装済み範囲:

- `race_state v2` の新規 `lapHistory` から `lap_complete` を 1 回だけ確定する。
- `phase` の `finished` 遷移から `race_finish` を 1 回だけ確定する。
- 初期本番 voice は Kokoro `am_michael` に固定する。`raceAnnounce=0` で無効化する。
- `queued` 受信時に約 200 ms の固定 radio cue を即時再生し、TTS 生成中であることを Pilot へ知らせる。
- TTS 失敗時は既存 Web Speech API へフォールバックする。
- TTS 再生中は M5Audio を 40% へ下げ、終了時に復元する。
- LAN Relay と Ayame Relay transport の両方で同じ WebRTC audio track を使う。

未実装:

- STOP、blue flag、接近警告、PIT、Fuel、final lap、Boost の server-side TTS event 化。
- 高優先度音声による再生中 clip の中断と priority queue。
- Operations 画面の生成時間、cache hit、失敗数。
- 4 Pilot 同時実走試験と `11.100` への本番適用。

## 経路

```text
Race Control race_state v2
          |
          v
11.100 Relay event detector
          |
          +-- internal HTTP --> 192.168.11.105 race-audio-service
          |                         +--> Kokoro EN / am_michael
          |                         +--> JA engines retained for comparison only
          |                         +--> Opus packet cache
          |
          +-- WebRTC video track --------------------+
          +-- WebRTC race audio track (Opus) --------+--> Pilot Viewer
          +-- momo-race-audio DataChannel -----------+
                       language / queued / playing / ended / failed
```

外部 Ayame Pilot も Relay との PeerConnection で Opus track を受ける。ブラウザから TTS host へ
直接接続しないため、Cloudflare Tunnel、公開 audio hostname、CORS、mixed content 対策は不要である。
Ayame signaling server と TURN server は SDP / ICE / DTLS の確立だけを仲介し、TTS API は公開しない。

## 配置

TTS service と Relay は同じ PC である必要はない。現在の試験構成は次を使う。

| role | host | endpoint |
| --- | --- | --- |
| Relay | `192.168.11.100` | `:8090` |
| TTS service | `192.168.11.105` | `:18090` |
| Race Control | `192.168.11.100` | 既存設定 |

`11.100` から `192.168.11.105:18090/tcp` へ到達できれば動作する。Windows Firewall は
送信元 `192.168.11.100` に限定する。この PC の停止、sleep、IP 変更で TTS は失敗するが、Relay の
映像、操縦、telemetry、race state は継続し、Viewer は Web Speech へフォールバックする。

本番常用時にこの PC の稼働を保証できないなら、TTS service を `11.100` または常時稼働する別 host へ
移す。service URL だけを変えれば Viewer と Race Control の変更は不要である。

## TTS 内部 API

実装は `tools/race-audio-service/` にある。Python 3.13、Kokoro ONNX、PyAV の libopus encoder を使う。

```text
GET  /healthz
POST /v1/synthesize
```

`POST /v1/synthesize` は `Authorization: Bearer <token>` を要求する。要求例:

```json
{
  "eventKey": "run-42:CP-1:lap:4",
  "language": "en-US",
  "voice": "am_michael",
  "text": "Lap four. Thirteen point four four four. P two.",
  "speed": 1.04,
  "codec": "opus",
  "frameDurationMs": 20
}
```

応答は `48 kHz`、mono source、20 ms ごとの raw Opus packet を Base64 配列で返す。
HTTP response は最大 4 MiB、各 packet は最大 1500 bytes とし、Relay が全項目を検証する。
JSON は内部 API に限定される。音声 packet を DataChannel へ載せない。

cache key には engine、model、voice、language、text、speed、codec を含める。同じ内容は disk cache から返す。
2026-08-16 のこの PC では、ウォームアップ後の `Lap 4. 13.715. P 2.` が Relay の最初の RTP まで
1,566 ms、cache hit が 18 ms、音声長が 3,521 ms だった。service は `listening` 表示前に
`am_michael` を 1 回推論し、最初の実イベントへ約 4.7 秒の cold inference を持ち込まない。
文章量で変動するため、11.100 適用後も生成時間を測る。
INT8 model は英語 5～6 秒、日本語 11～12 秒で、容量は小さいが race notification には遅いため既定にしない。

### Piper Plus CSS10 実測

`piper-plus 1.13.0` と `ayousanz/piper-plus-css10-ja-6lang` の FP16 model をこの PC の CPU で検証した。
Python runtime は model と英語 G2P を常駐させ、起動中に英語・日本語を 1 回ずつウォームアップする。
cold start から service ready までは約 5～7 秒、warmup 後の process working set は約 370 MB、
private memory は約 1.0 GB だった。race 開始前に service ready を確認し、レース中は process を常駐させる。

6 種類のレース文言の平均:

| language | warm generation | audio duration | RTF |
| --- | ---: | ---: | ---: |
| English | 114 ms | 2,442 ms | 0.048 |
| Japanese | 139 ms | 2,862 ms | 0.049 |

2026-08-16 の聞感比較では、CSS10 は Kokoro より明確に音質が低く、本番既定候補には採用しない。
速度と Piper Plus 経路を確認する benchmark model としてのみ残す。

TTS API の未キャッシュ生成から Relay 内の WebRTC track を通し、Pion 受信側で最初の
Opus RTP payload を受けるまでの実測:

| language | synthesize HTTP | Relay track | total | cached HTTP |
| --- | ---: | ---: | ---: | ---: |
| English | 226 ms | 16 ms | 241 ms | 25 ms |
| Japanese | 263 ms | 3 ms | 266 ms | 16 ms |

英語 2 件、日本語 2 件を同時に未キャッシュ生成した場合:

| request | client total | service elapsed | audio duration |
| --- | ---: | ---: | ---: |
| English 1 | 812 ms | 662 ms | 3,221 ms |
| Japanese 1 | 332 ms | 188 ms | 2,541 ms |
| English 2 | 502 ms | 357 ms | 3,441 ms |
| Japanese 2 | 686 ms | 540 ms | 4,381 ms |

4 件全体の wall time は 818 ms だった。model inference は 1 process 内で直列化しているため、
同時生成数が増えると待ち時間も増える。4 Pilot 規模では 1 秒未満だったが、8 台以上へ拡張する場合は
高優先度文言の事前生成、cache 利用、または worker process の分離を再評価する。

これは同一 PC の loopback 検証である。`11.100` Relay からこの PC の TTS service までの LAN 実測と、
Ayame Pilot のブラウザ再生時刻は未検証である。一方、生成と Relay 内部配信は 2.5 秒の受け入れ上限を
大きく下回った。

公開済み Windows C++ runtime `v1.13.0` は、CSS10 の公開 `config.json` に含まれる複数コードポイント音素
`œ̃` を読み込めず終了した。現時点では Python runtime を使う。合成時に空白と一部句読点の
`Missing phoneme` warning が出るため、本番既定へ切り替える前にレース文言の聞感を確認する。
数字は英語の単語または日本語の漢数字へ正規化してから音素化し、正規化 version を cache identity に含める。

### Piper Plus つくよみちゃん実測

声優系の比較候補として `ayousanz/piper-plus-tsukuyomi-chan` の MB-iSTFT FP16 model を、公式推奨の
`length_scale=1.5` で測定した。model は約 38 MB で、同じ Piper Plus Python runtime と数字正規化を使う。

6 種類のレース文言の平均:

| language | warm generation | audio duration | RTF |
| --- | ---: | ---: | ---: |
| English | 91 ms | 2,988 ms | 0.031 |
| Japanese | 151 ms | 4,342 ms | 0.035 |

TTS API から Relay の WebRTC track を通した実測:

| language | synthesize HTTP | Relay track | total | cached HTTP |
| --- | ---: | ---: | ---: | ---: |
| English | 249 ms | 11 ms | 260 ms | 50 ms |
| Japanese | 546 ms | 15 ms | 561 ms | 23 ms |

model load と英語・日本語の warmup は約 4.7～6.0 秒だった。生成速度は race notification に十分だが、
本番には採用しない。比較を継続する場合はモデルカードと
つくよみちゃんコーパス利用規約に従ってクレジットを掲載する。

最初の比較サンプルは `piper-plus 1.13.0` の `PiperVoice` runtime をそのまま使っており、ONNX の
`prosody_features` へ日本語の A1 / A2 / A3 を渡さず、全要素を 0 にしていた。この音声は日本語の
聞感評価には使えない。service は公式デモと同じ `piper-plus-g2p` の OpenJTalk 解析結果を渡す実装へ
変更し、cache identity を更新した。上表と比較ページの Tsukuyomi 音声は修正後に再生成した値である。
修正後にも残るイントネーション差をモデル候補の評価対象とする。

比較用ファイルは `compare_tts_engines.py` で生成し、`build_tts_comparison_report.py` で Kokoro、Pocket TTS、
VOICEVOX と同じ表へ追加する。

## WebRTC / DataChannel 契約

Relay transport の Viewer は接続開始時から audio recvonly transceiver を offer へ含める。Relay は Pilot ごとに
Opus track を answer へ含め、idle 中も 20 ms Opus DTX silence を送る。最初の TTS event で SDP renegotiation は
行わない。

音声状態と設定は reliable `momo-race-audio` DataChannel を使う。初期本番は `en-US` だけを使う。
Viewer から Relay への設定:

```json
{
  "type": "race_audio_preference",
  "version": 1,
  "language": "en-US"
}
```

Relay から Viewer への message は `RACE_AUDIO:` prefix の後ろに JSON を置く。

```json
{
  "type": "race_audio",
  "version": 1,
  "state": "playing",
  "eventId": "run-42:CP-1:lap:4",
  "kind": "lap_complete",
  "priority": 40,
  "language": "en-US",
  "durationMs": 4380,
  "fallbackText": {
    "en-US": "Lap four. Thirteen point four four four. P two.",
    "ja-JP": "4 周目。13.444 秒。現在 2 位。"
  },
  "ducking": {
    "m5AudioGain": 0.4,
    "attackMs": 80,
    "releaseMs": 250
  }
}
```

`state` は `queued`、`ready`、`playing`、`ended`、`failed` を使う。`failed` では Viewer が選択言語の
`fallbackText` を Web Speech API で読む。capability は同じ prefix で
`type: race_audio_capabilities`、`state: enabled` を送る。

`eventId` は `(raceRunId, carId, kind, lap)` から決める。timing correction で lap time が変わっても同じ LAP を
再度読まない。Relay 再起動または途中接続時は、その時点の既存 `lapHistory` を baseline とし、過去 LAP を
まとめて再生しない。

## この PC での起動

```powershell
cd C:\src\momo\tools\race-audio-service
uv sync --group dev

# Piper Plus CSS10
.\download-piper-plus-models.ps1

$env:MOMO_RACE_AUDIO_SERVICE_TOKEN = '<random-token>'
uv run python .\race_audio_service.py `
  --listen 0.0.0.0:18090 `
  --engine piper-plus
```

model は Git 管理しない。download script は固定 SHA-256 を検証する。Piper Plus は次の model と config を使う。

```text
models/css10-ja-6lang-fp16.onnx
models/css10-ja-6lang-config.json
models/nltk_data/
```

経路だけを確認する場合は `--engine fixture` を使う。Kokoro は `download-kokoro-models.ps1` と
`--engine kokoro` で比較できる。日本語を VOICEVOX で比較する場合は VOICEVOX Engine を別途起動し、
`--engine voicevox --voicevox-url http://127.0.0.1:50021` を使う。

## `11.100` Relay への適用

`11.100` では token を Git 管理外の process environment に設定する。command line へ token を入れない。

```powershell
$env:MOMO_RACE_AUDIO_SERVICE_URL = 'http://192.168.11.105:18090'
$env:MOMO_RACE_AUDIO_SERVICE_TOKEN = '<same-token>'
```

`tools/start-mads-observer.ps1` は `MOMO_RACE_AUDIO_SERVICE_URL` がある時だけ次を Relay へ渡す。

```text
-race-audio-service-url http://192.168.11.105:18090
-race-audio-default-language en-US
-race-audio-en-voice am_michael
-race-audio-ja-voice jf_alpha
-race-audio-speed 1.04
```

適用順:

1. この PC で TTS service を起動し、`GET /healthz` を確認する。
2. `11.100` から同じ URL の `/healthz` を確認する。
3. token なしの `POST /v1/synthesize` が `401` になることを確認する。
4. 更新済み Relay binary と Viewer web assets を `11.100` へ配置する。
5. 上記環境変数を設定して Relay を再起動する。
6. LAN Pilot で audio track、`momo-race-audio`、LAP、GOAL、M5Audio ducking を確認する。
7. Ayame Pilot で同じ確認を行う。ブラウザの Network に TTS host への HTTP request がないことを確認する。
8. 4 台同時接続で command delay、映像 FPS、DataChannel buffered amount、TTS 生成待ちを比較する。

## 検証

```powershell
cd C:\src\momo\tools\race-audio-service
uv run pytest -v

cd C:\src\momo\tools\momo-relay
go test ./...

cd C:\src\momo-fpv-viewer
npm test
npm run build:relay
```

Go test は race event の冪等化、内部 API の Bearer token、Opus response 検証、SDP renegotiation なしの
Opus RTP 到達を含む。Python test は 20 ms Opus encode、cache、入力範囲を確認する。

## 受け入れ条件

- 外部 Ayame Pilot が公開 audio endpoint なしで TTS を聞ける。
- ブラウザへ TTS service URL と token を渡さない。
- 同じ LAP と GOAL を二重再生しない。
- TTS service 停止中も映像、操縦、telemetry、race state が継続する。
- TTS 失敗時は Web Speech へ移行する。
- TTS 再生終了、失敗、切断後に M5Audio gain が 100% へ戻る。
- Viewer の選択言語以外を再生しない。
- Direct Viewer はこの Relay 専用契約に依存しない。

## ロールバック

`MOMO_RACE_AUDIO_SERVICE_URL` を外して Relay を再起動する。Relay は audio track と
`momo-race-audio` capability を作らず、Viewer は既存 Web Speech を使う。TTS service を停止してもよい。
M5Audio、Race Control、FFB、車体 firmware の rollback は不要である。
