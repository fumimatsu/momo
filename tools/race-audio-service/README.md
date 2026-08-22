# Race Audio Service

Relay 内部専用の TTS / Opus packet service である。公開 Viewer からこの service へは接続しない。
Relay と別 PC で動かす場合は、LAN 内で Relay から到達できるアドレスへ bind し、Bearer token を必須にする。
非 loopback address へ token なしで bind しようとすると起動を拒否する。

## Production setup

初期本番構成は英語だけを配信し、Kokoro `am_michael` を使う。
`11.100` へ TTS service と Relay を同居させる手順は
[`../../docs/relay-race-audio-operations.md`](../../docs/relay-race-audio-operations.md) を参照する。

```powershell
cd C:\src\momo\tools\race-audio-service
.\download-kokoro-models.ps1
$env:MOMO_RACE_AUDIO_SERVICE_TOKEN = '<random-token>'
uv sync --group dev
uv run python .\race_audio_service.py --listen 0.0.0.0:18090 --engine kokoro
```

Kokoro は起動時に英語`am_michael`と日本語`jf_alpha`をウォームアップする。`listening`表示後の
初回LAPにONNX初期化や日本語辞書初期化を混ぜないため、起動完了まで数秒かかる。
Relayの既定voiceは英語`am_michael`である。

### VOICEVOX Japanese event profile

日本語中心のローカルイベントでは、MADSYSTEMの通常設定に合わせて
`†聖騎士 紅桜†`のノーマル（speaker ID `51`）と速度`1.0`を使う。
MADSYSTEMは`audio_query_from_preset?preset_id=2`を呼ぶが、VOICEVOXのカスタムpresetは
端末ローカルであり、このserviceの必須条件にはしない。pitch `0.0`、intonation `1.0`、volume `1.0`は
VOICEVOXのspeaker既定値を使う。

```powershell
$env:MOMO_RACE_AUDIO_SERVICE_TOKEN = '<random-token>'
uv run python .\race_audio_service.py `
  --listen 0.0.0.0:18090 `
  --engine voicevox `
  --voicevox-url http://127.0.0.1:50021 `
  --voicevox-speaker 51
```

Relayは`-race-audio-speed 1.0 -race-audio-browser-kokoro=false`で起動する。Pilot Viewerの
`Race Voice`は`日本語`を選ぶ。`Off`はローカル生成だけでなく中央VOICEVOX音声も停止する。

## Comparison engines

### Browser Pilot Kokoro

Pilot固有のLAP通知を各Viewerで生成する検証では、日本語`jf_alpha + Misaki JAG2P`と英語
`am_michael + espeak-ng`がKokoroへ渡す整形後音素とmodel input IDを基準fixtureとして出力する。
ブラウザ側は同じIDを`generate_from_ids()`へ渡し、G2Pとtokenizerのruntime差を除外する。

```powershell
cd E:\src\momo\tools\race-audio-service
& .\.venv\Scripts\python.exe .\export_browser_kokoro_fixture.py `
  E:\src\momo-fpv-viewer\tools\browser-kokoro-lab\fixtures
```

fixtureにはmodel/voices SHA-256、日英の音素とmodel input ID、Python基準WAV、生成時間を含む。生成物はGit管理しない。
日本語fixtureではMisakiの生音素も保持し、末尾の韻律制御列を除去して`.`を発話境界として付ける。
固有語の置換は`japanese_pronunciation_dictionary.json`へ分離し、聴取確認済みの項目だけを登録する。
Viewer側の実行手順と測定結果は`momo-fpv-viewer/docs/browser-kokoro-pilot-evaluation.md`を参照する。
公開Viewerからこのserviceを直接呼ばせない。RelayがBearer token付きで`POST /v1/prepare`を呼び、短い
音素payloadとmodel input IDだけを`momo-race-audio` Reliable DataChannelへ中継する。対応Pilotは
WebGPU/FP32 modelのload完了後、LAP通知だけをローカル生成する。GOALと全体実況は`/v1/synthesize`で
中央生成する。

### Qwen3-TTS PoC

Qwen3-TTS は現行 service へ組み込まず、比較専用の Python 3.12 venv で実行する。
現行 service は Python 3.13 を前提としている一方、検証済みの Qwen runtime は Python 3.12 を使うためである。
`faster-qwen3-tts` は MIT、Qwen3-TTS のcodeと使用modelは Apache-2.0 である。配布物へ組み込む場合は
それぞれのlicense noticeとmodel provenanceを残す。

```powershell
py -3.12 -m venv E:\tmp\momo-race-audio-qwen-venv
$python = 'E:\tmp\momo-race-audio-qwen-venv\Scripts\python.exe'
& $python -m pip install --upgrade pip
& $python -m pip install torch==2.11.0+cu128 torchaudio==2.11.0+cu128 `
  --index-url https://download.pytorch.org/whl/cu128
& $python -m pip install qwen-tts==0.1.1 faster-qwen3-tts==0.3.2 `
  av==18.1.0 kokoro-onnx==0.4.9 'misaki[ja]==0.9.4' pytest
```

Kokoro の model は `download-kokoro-models.ps1` で準備する。Qwen model の cache を一時領域へ
分離する場合は `HF_HOME` を設定する。

```powershell
cd C:\src\momo\tools\race-audio-service
$python = 'E:\tmp\momo-race-audio-qwen-venv\Scripts\python.exe'
$output = 'E:\tmp\momo-qwen3-tts-comparison'
$env:HF_HOME = 'E:\tmp\huggingface-cache'

& $python .\compare_tts_engines.py --engine kokoro --language en-US `
  --voice am_michael --output-dir $output
& $python .\compare_tts_engines.py --engine kokoro --language en-US `
  --voice jf_alpha --output-dir $output
& $python .\compare_tts_engines.py --engine kokoro --language ja-JP `
  --voice jf_alpha --output-dir $output
& $python .\compare_tts_engines.py --engine qwen3 --language en-US `
  --voice Ryan --output-dir $output --burst-size 4
& $python .\compare_tts_engines.py --engine qwen3 --language ja-JP `
  --voice Ono_Anna --output-dir $output --burst-size 4 --qwen-local-files-only
& $python .\compare_tts_engines.py --engine qwen3 --qwen-backend faster `
  --qwen-streaming --qwen-model Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice `
  --qwen-max-new-tokens 256 --language en-US --voice Ryan `
  --output-dir $output --burst-size 4
& $python .\compare_tts_engines.py --engine qwen3 --qwen-backend faster `
  --qwen-streaming --qwen-model Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice `
  --qwen-max-new-tokens 256 --language ja-JP --voice Ono_Anna `
  --output-dir $output --burst-size 4 --qwen-local-files-only
& $python .\build_tts_comparison_report.py $output
```

`--limit` は各言語の先頭 N 文だけを試す。`--qwen-local-files-only` は model download 完了後の
再現試験で使う。`faster` backend は CUDA Graph を起動時に捕捉し、さらに CustomVoice を1回生成して
codecを含めてウォームアップする。Windows では FlashAttention を前提にせず `sdpa` を既定にする。CustomVoice の
比較経路では SoX の warning が出ても WAV は生成できるが、Qwen の別機能には SoX が必要になり得る。

比較 corpus は日英各 20 文で、パイロット名、車両番号、小数ラップタイム、PIT、BOOST、接触、
最終周、略語混在を含む。manifest には P50 / P95、RTF、GPU peak、4件同時到着時の待ち時間を記録する。
HTML report は同一文の逐次生成に対して音声長が 2 倍以上になった burst 出力を、反復・生成不安定の
確認対象として強調する。

RTF は `生成時間 / 音声時間` の一般的な定義で記録し、小さいほど速い。`speedFactor` はその逆数で、
`2.5` なら音声の再生時間に対して約2.5倍速で生成できたことを示す。

Piper Plus CSS10 6-language model を使う場合:

```powershell
cd C:\src\momo\tools\race-audio-service
uv sync --group dev
.\download-piper-plus-models.ps1

$env:MOMO_RACE_AUDIO_SERVICE_TOKEN = '<random-token>'
uv run python .\race_audio_service.py `
  --listen 0.0.0.0:18090 `
  --engine piper-plus
```

起動時に英語 G2P と日本語推論をウォームアップする。初回の LAP 通知に辞書初期化を
混ぜないため、`listening` の表示まで数秒かかる。推論中の model はメモリに常駐する。
Piper Plus では LAP、time、position の数字を英語の単語または日本語の漢数字へ正規化してから合成する。
日本語は `piper-plus-g2p` の OpenJTalk 解析で得た A1 / A2 / A3 を ONNX の
`prosody_features` へ渡す。`piper-plus` の `PiperVoice` を直接使うと、この入力がすべて 0 になるため
本番の日本語経路には使わない。
download script は CSS10 に加えて、つくよみちゃん 6 言語 FP16 model も取得する。つくよみちゃんを比較する場合:

```powershell
uv run python .\race_audio_service.py `
  --listen 127.0.0.1:18090 `
  --engine piper-plus `
  --piper-model models\tsukuyomi-chan-6lang-fp16.onnx `
  --piper-config models\tsukuyomi-chan-6lang-config.json `
  --piper-length-scale 1.5
```

download script は model と voices の SHA-256 を固定値で検証し、不一致なら停止する。

この PC で動かし、`192.168.11.100` の Relay から利用する場合の Relay 設定は次の通り。

```powershell
$env:MOMO_RACE_AUDIO_SERVICE_URL = 'http://192.168.11.105:18090'
$env:MOMO_RACE_AUDIO_SERVICE_TOKEN = '<same-token>'
```

Windows Firewall は `192.168.11.100` からの TCP 18090 だけを許可する。Internet へ公開しない。

経路だけを検証する場合は model を使わずに起動できる。

```powershell
uv run python .\race_audio_service.py --listen 127.0.0.1:18090 --engine fixture
```

`/v1/prepare`の同時要求を1、4、8、16、32件で再現し、中央G2P処理の
エラー率とP50/P95を確認する場合:

```powershell
uv run python .\benchmark_prepare_burst.py --url http://127.0.0.1:18090 --language ja-JP
```

これは音声推論ではなく、Browser Kokoroへ渡す音素とtokenの準備処理を測る。
Pilot数を増やす前に、実運用と同じengine、辞書、token設定で実行する。

日本語を VOICEVOX で比較する場合は、VOICEVOX Engine を先に起動して `--engine voicevox` を使う。
現行既定は英語 Kokoro の `am_michael` である。日本語は`misaki[ja]`の`JAG2P`で音素化してから、
末尾のMisaki韻律制御列を除去し、発話境界`.`を付けてKokoro ONNXへ`is_phonemes=True`で渡す。
このポリシーと`japanese_pronunciation_dictionary.json`の内容はengine identityへ含め、旧音声cacheを再利用しない。
`jf_alpha`は日本語女性話者であり、英語G2Pと組み合わせれば
英語も生成できるが、日本語話者らしい発音になる。VOICEVOX、Piper Plusは比較経路として残すが、
初期本番構成では使用しない。
この PC では FP32 model が INT8 model より大幅に速かったため、`kokoro-v1.0.onnx` を既定にする。

## Sample-based radio cue comparison

`build_sampled_radio_cue_candidates.py` は、利用許諾を別途確認した短い無線音源から比較用 WAV を生成する。
入力ディレクトリには次のファイル名が必要である。

- `radio-click.mp3`
- `radio-buzz-squelch.mp3`
- `radio-signoff-squelch.mp3`
- `walkie-talkie-beep.mp3`

音源ファイル自体はリポジトリへ含めない。ライセンスと再配布条件を確認してから使用する。
処理は既存依存の PyAV と NumPy だけを使う。

```powershell
uv run python .\build_sampled_radio_cue_candidates.py C:\path\to\sources C:\temp\race-radio-cues
```
