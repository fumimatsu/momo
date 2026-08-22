# Relay 配信型レース音声サービス

本番適用、再起動、Viewer 再接続、Web Observer の対応範囲は
[`relay-race-audio-operations.md`](relay-race-audio-operations.md) を参照する。

## 状態

Relay PilotへLAP完了とGOALの日英TTSを配信する実装である。Pilot固有のLAPは対応ブラウザへKokoroの
音素とmodel input IDを送り、WebGPUで生成できる。GOALと将来の全体実況はRelayが取得したOpus packetを
既存PeerConnectionの専用audio trackへ送る。音声assetの公開HTTPS配信は採用しない。

実装済み範囲:

- `race_state v2` の新規 `lapHistory` から `lap_complete` を 1 回だけ確定する。
- `lapHistory[].achievement` が示す自己ベスト・全体ベスト更新を LAP 読み上げへ追加する。Relay はベストを再計算しない。
- `phase` の `finished` 遷移から `race_finish` を 1 回だけ確定する。
- 英語はKokoro `am_michael`、日本語は`jf_alpha + Misaki JAG2P`を使う。Viewerの既定選択は`Off`とする。
- WebGPU/FP32 model準備完了後の`lap_complete`だけをBrowser Kokoroへ分散する。
- Battle Meterと同じ確定gapから、前方車と後方車の車番・0.1秒単位の差をPilot固有通知にする。
- Viewerは固定schemaの`race_audio_callout_request`だけを送り、Relayが文言を組み立てる。
- RelayはPilotごとに2秒のhard limit、重複排除、512 byte上限を適用する。
- `race_finish`は中央生成のOpus trackを維持し、旧Viewerはmode未指定のまま中央生成を使う。
- `queued` 受信時に約 200 ms の固定 radio cue を即時再生し、TTS 生成中であることを Pilot へ知らせる。
- TTS 失敗時は既存 Web Speech API へフォールバックする。
- TTS 再生中は M5Audio を 40% へ下げ、終了時に復元する。
- LAN Relay と Ayame Relay transport の両方で同じ WebRTC audio track を使う。

追加実装済み:

- STOP 相当の `race_paused`、race 再開、race start、blue flag、順位変動を event 化する。
- PIT は servicing 中に繰り返さず、service 完了時だけ event 化する。
- Fuel low / critical / empty、critical damage、final lap を event 化する。

未実装:

- Boost の server-side TTS event 化。
- Yellow / Red flag を `flag` 遷移として区別した TTS event 化。Red は現状 `phase: paused` の `race_paused` として扱う。
- 逆走状態の契約、判定、TTS event 化。
- race開始前など全員向けeventを1回生成し、全Pilotへ同じpacketを配るserver-level audio bus。
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
          |                         +--> Kokoro JA / jf_alpha + Misaki
          |                         +--> /v1/prepare: phonemes + input IDs
          |                         +--> /v1/synthesize: Opus packet cache
          |
          +-- WebRTC video track --------------------+
          +-- WebRTC race audio track (Opus) --------+--> Pilot Viewer
          +-- momo-race-audio DataChannel -----------+
                       mode / queued / prompt / playing / ended / failed
```

外部 Ayame Pilot も Relay との PeerConnection で Opus track を受ける。ブラウザから TTS host へ
直接接続しないため、Cloudflare Tunnel、公開 audio hostname、CORS、mixed content 対策は不要である。
Ayame signaling server と TURN server は SDP / ICE / DTLS の確立だけを仲介し、TTS API は公開しない。

Pilot固有のLAP通知は中央TTSで全件を生成せず、各Pilot ViewerのWebGPU Kokoroへ分散する。中央で日本語は
Misaki JAG2P、英語はespeak-ngを使って固定model版のinput IDまで生成し、
Relayが言語、voice、model revision、input IDをReliable channelで中継する。
ViewerへRace Audio Service tokenや辞書を配布しない。全体実況は中央生成の独立audio layerとして残す。

前後差通知は既存のReliableな`momo-race-audio` DataChannelを双方向で使う。ブラウザは任意文言を送れず、
`gap_ahead | gap_behind`、車番、100 ms単位のgap、64文字以内のrequest IDだけを送る。Relayは
`前、11号車、差0.8`または`Car 11 ahead. Gap zero point eight seconds`相当の固定文言に変換し、
`browser-kokoro`では`/v1/prepare`結果を、`remote`では中央生成したOpusを同じPilotだけへ返す。
正式な`carNumber`がTiming/Directoryから届くまでは、
`CP-2`や`FPV-02`の末尾番号を移行用fallbackとして使う。

2026-08-19の20文比較では、ブラウザWebGPU/FP32がgeneration P50 388 ms、P95 524 ms、Python基準との
minimum waveform cosine 0.9961だった。WASM/Q8はP50 7,651 msのためフォールバックに使わず、WebGPU
利用不能時は中央生成を維持する。ローカル生成に失敗したLAPだけOS Speechへ戻す。実WebRTC映像との
同時試験は未完了であり、Race Voiceは既定`Off`のままとする。

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
POST /v1/prepare
POST /v1/synthesize
```

両POST APIは`Authorization: Bearer <token>`を要求し、Viewerから直接呼ばない。`/v1/prepare`は
Kokoroの固定model ID、voice、整形後phonemes、model input IDsを返す。Relayは内容と境界tokenを検証して
Reliable DataChannelへ中継する。`/v1/synthesize`の要求例:

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

`ThreadingHTTPServer`の既定accept queueでは16件以上の同時`/v1/prepare`が約0.5秒刻みで待ったため、
serviceのqueueを64へ拡張した。2026-08-19のloopback再測定では32件同時要求が日英ともエラー0で、
日本語P95 18.19 ms、英語P95 14.82 msだった。`benchmark_prepare_burst.py`で同じ試験を再現できる。
これはG2Pとtoken生成だけの値であり、Browser側のWebGPU推論時間を含まない。

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

### Qwen3-TTS feasibility result

2026-08-19 に RTX 5070 12 GB、Python 3.12、PyTorch 2.11.0 + CUDA 12.8、
`Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice`、BF16、SDPA で日英各 20 文を生成した。
比較対象は Kokoro `am_michael` / `jf_alpha` とし、同じ race announcement corpus を使った。

| engine / language | generation P50 | generation P95 | average audio | RTF P50 | GPU peak |
| --- | ---: | ---: | ---: | ---: | ---: |
| Kokoro English | 646 ms | 1,048 ms | 4,458 ms | 0.149 | CPU |
| Kokoro Japanese / `jf_alpha` + Misaki | 708 ms | 946 ms | 5,058 ms | 0.141 | CPU |
| Qwen3-TTS 0.6B English | 10,084 ms | 12,724 ms | 6,184 ms | 1.630 | 2,387 MB |
| Qwen3-TTS 0.6B Japanese | 11,384 ms | 16,790 ms | 7,280 ms | 1.634 | 2,385 MB |

Kokoro `jf_alpha`へ`lang=ja`で日本語文字列を直接渡した旧比較は、非対応言語の警告文を音声化した
無効な出力だった。Kokoro v1 model自体は日本語対応であり、正しい経路は`misaki[ja]`の
`JAG2P(version="pyopenjtalk")`で音素化し、Kokoro ONNXへ`is_phonemes=True`で渡す方式である。
日本語の短い通知ではMisaki終端韻律が余分な語尾として発声されるため、本番serviceは末尾の
`^ _ - j`制御列を除去し、音素境界`.`を付けてから生成する。ポリシーversionと発音辞書hashを
engine identityへ含めるため、変更前のcacheは自動的に再利用されない。

Qwen model の cache 済み load は約 8.3 秒、warmup は英語約 3.8 秒、日本語約 7.7 秒だった。
VRAM 容量には余裕があるが、現在の公式 Python CustomVoice API は full WAV を返す batch 経路であり、
この Windows SDPA 構成では生成完了まで待つ必要がある。現行の live race notification の 2.5 秒上限を
満たさないため、本番 engine は Kokoro のままとする。

単一 model instance に4件を同時到着させ、推論を直列化した結果:

| language | wall time | client P50 | client P95 |
| --- | ---: | ---: | ---: |
| English | 80,080 ms | 28,320 ms | 73,109 ms |
| Japanese | 43,853 ms | 27,614 ms | 42,270 ms |

英語 burst の `pilot_name` は、逐次生成時 7,280 ms の音声が 28,400 ms まで伸び、生成にも
46,420 msかかった。同一文・別 seed のため単純な性能分散だけとは断定できず、聞感で反復や脱落を
確認する必要がある。ただし live 採用時には生成 timeout、最大音声長、1回だけの再生成、Kokoro fallback、
同一文に対する音声長比の監視が必要である。

同じ PC で `faster-qwen3-tts 0.3.2` の CUDA Graph / streaming backend と
`Qwen3-TTS-12Hz-1.7B-CustomVoice` を追加検証した。ここでの RTF は `生成時間 / 音声時間`、
speed factor はその逆数である。起動時に CUDA Graph 捕捉と CustomVoice 1文の生成を完了させてから
race corpus を測定した。

| language | TTFA P50 | TTFA P95 | generation P50 | generation P95 | speed P50 | GPU peak |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| English | 296 ms | 313 ms | 1,984 ms | 3,124 ms | 2.474x | 4,337 MB |
| Japanese | 295 ms | 309 ms | 2,181 ms | 3,333 ms | 2.503x | 4,341 MB |

model load は約 10.2～10.4 秒、完全 warmup は約 5.5～6.7 秒だった。RTX 5070では公開されている
RTX 4090 の4倍台には届かなかったが、最初の音声を約0.3秒で供給し、その後は再生より約2.5倍速で
生成できる。full WAV 完了時間が2.5秒を超える長文もあるが、streaming再生は継続できるため、
upstream batch runtimeとは採用判断を分ける。

4件同時到着時の単一 model直列処理:

| language | wall time | client P50 | client P95 | request TTFA after dequeue |
| --- | ---: | ---: | ---: | ---: |
| English | 8,430 ms | 5,256 ms | 8,148 ms | 308～344 ms |
| Japanese | 9,194 ms | 5,583 ms | 8,788 ms | 311～325 ms |

各 request の推論開始後は速いが、後順位の event はmodel lock待ちになる。実際の実況音声も同時再生できないため、
workerを4つへ増やす前に、priority queue、同種の古いeventの置換、race終了などの割り込み、生成済み音声cacheを
設計する。今回のfaster 1.7B burstでは、逐次生成比2倍以上の音声長outlierは発生しなかった。

現段階の判断:

1. 日英両言語の生成、true streaming、既存比較レポートへの統合は成立する。
2. faster 1.7B はlive実況backend候補として次段階へ進める。upstream 0.6B batch経路は候補から外す。
3. 1.7B の日英全サンプルを人が確認し、名前、数字、略語、反復、自然さを評価する。ASR back-checkは補助に留める。
4. Race Audio Serviceへの組み込みは、queue / timeout / duration guard / Kokoro fallbackと、実機E2Eを同時に実装する。
   聞感とE2Eが通るまでKokoroの本番既定値は変更しない。
5. Qwen を採用する場合も、ChatGPT 等が作る実況文と TTS engine は分離し、生成文を cache / priority queue /
   fallbackへ渡す現在の責務分離を維持する。

## WebRTC / DataChannel 契約

Relay transport の Viewer は接続開始時から audio recvonly transceiver を offer へ含める。Relay は Pilot ごとに
Opus track を answer へ含め、idle 中も 20 ms Opus DTX silence を送る。最初の TTS event で SDP renegotiation は
行わない。

音声状態と設定はreliable `momo-race-audio` DataChannelを使う。
Viewer から Relay への設定:

```json
{
  "type": "race_audio_preference",
  "version": 1,
  "language": "ja-JP",
  "mode": "browser-kokoro"
}
```

`mode`は`remote`または`browser-kokoro`である。未指定は後方互換のため`remote`とする。Viewerは
WebGPU/FP32 modelのload完了前に`browser-kokoro`を送らない。Relayを
`-race-audio-browser-kokoro=false`で起動した場合はcapabilityに`remote`だけを載せ、Viewerから届いた
`browser-kokoro` preferenceも受理しない。

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
    "en-US": "Lap four. Thirteen point four four four seconds",
    "ja-JP": "4周目、13.444"
  },
  "ducking": {
    "m5AudioGain": 0.4,
    "attackMs": 80,
    "releaseMs": 250
  }
}
```

`state`は`queued`、`prompt`、`ready`、`playing`、`ended`、`failed`を使う。`prompt`は
`lap_complete`のBrowser Kokoro用で、phonemesとmodel input IDsを含む。Viewerは生成中に次のLAPが来たら
最新1件だけを残し、古い生成結果を再生しない。`failed`ではViewerが選択言語の`fallbackText`をWeb Speech
APIで読む。capabilityは同じprefixで`type: race_audio_capabilities`、`state: enabled`と利用可能な
`modes`を送る。中央VOICEVOX運用では`modes: ["remote"]`、Browser Kokoro併用時は
`modes: ["remote", "browser-kokoro"]`となる。

`eventId` は `(raceRunId, carId, kind, lap)` から決める。timing correction で lap time が変わっても同じ LAP を
再度読まない。Relay 再起動または途中接続時は、その時点の既存 `lapHistory` を baseline とし、過去 LAP を
まとめて再生しない。

`achievement` は timing authority が全車の完了時刻順で確定する任意項目であり、値は
`personal_best` または `overall_best` とする。Race Control は検証・保存・配信だけを行い、Relay は値に応じて
`New personal best` または `New overall best` を文末へ付ける。現在の production authority である MADSYSTEM が
この項目を省略する間は、通常の LAP とタイムだけを読み上げる。Timing Engine への authority 切替前に、
MADSYSTEM 側も同じ項目を送るか、切替までベスト更新音声を無効のまま受け入れる必要がある。

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
