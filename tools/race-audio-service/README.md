# Race Audio Service

Relay 内部専用の TTS / Opus packet service である。公開 Viewer からこの service へは接続しない。
Relay と別 PC で動かす場合は、LAN 内で Relay から到達できるアドレスへ bind し、Bearer token を必須にする。
非 loopback address へ token なしで bind しようとすると起動を拒否する。

## Production setup

初期本番構成は英語だけを配信し、Kokoro `am_michael` を使う。

```powershell
cd C:\src\momo\tools\race-audio-service
.\download-kokoro-models.ps1
$env:MOMO_RACE_AUDIO_SERVICE_TOKEN = '<random-token>'
uv sync --group dev
uv run python .\race_audio_service.py --listen 0.0.0.0:18090 --engine kokoro
```

Kokoro は起動時に `am_michael` でウォームアップする。`listening` 表示後の初回 LAP に
ONNX 初期化を混ぜないため、起動完了まで数秒かかる。Relay の既定 voice も `am_michael` である。

## Comparison engines

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

日本語を VOICEVOX で比較する場合は、VOICEVOX Engine を先に起動して `--engine voicevox` を使う。
現行既定は英語 Kokoro の `am_michael` である。`jf_alpha`、VOICEVOX、Piper Plus は比較経路として残すが、
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
