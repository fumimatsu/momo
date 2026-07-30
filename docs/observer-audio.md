# Native Observer の M5 音声

`p2p-recv-multi` は映像と別に `momo-telemetry` DataChannel を作成する。選択した 1 台だけを Windows の既定再生デバイスへ出力し、各 Source の加速度 telemetry は Observer 上の診断表示に使う。Relay のプロトコル変更は不要である。

Windows Native Observer をビルドする SDL3 は `SDL_AUDIO=ON` が必須である。無効な SDL3 をリンクすると、画面上で `INIT ERR` と `SDL not built with audio support` が出て再生できない。

```powershell
.\tools\start-mads-observer.ps1 -ObserverAudioSource 11.6 -RestartObserver
```

- `--audio-source` を指定しなければ Windows への音声再生を行わない。
- 指定できるのは `--source` で登録した名前のいずれか 1 台だけ。
- `AUD:1` の 8 kHz IMA ADPCM を Native Observer が PCM に復元し、Windows の既定再生デバイスへ出力する。
- 4 台の常時ミックスは実装しない。音声が重なり、Relay からの追加転送も 4 倍になるためである。

## DataChannel と音声の範囲

Observer は各 Source に既存の `momo-telemetry` DataChannel を 1 本作る。Relay は upstream の `TEL:` と `AUD:` をそのまま各 Observer へ転送する。

- 加速度グラフは全 Source の state `TEL:` を読む。
- Windows へ PCM を出力するのは `--audio-source` で選んだ 1 台だけ。
- 未選択 Source の `AUD:` は受信しても復元・再生しない。
- 4 台の音声をミックスして再生する機能は持たない。

Relay は現在、Observer ごとに音声フレームを選別していない。このため同時接続台数を増やす前に、音声あり・なしで Relay の送受信量と映像 FPS を測る必要がある。

## telemetry の 3 軸グラフ

該当映像枠の左下に、state `TEL:` の加速度グラフを重ねる。`TELEMETRY:RAW` の V1 と、`TELEMETRY:BINARY` が Momo で復元した V2 の両方を表示する。通常運用では V2 の 30 Hz を使い、診断が必要なときだけ V1 RAW に切り替える。

- 表示範囲は最大 180 サンプル。V2 30 Hz では直近約 6 秒、V1 RAW 15 Hz では直近約 12 秒である。縦軸は固定で `-15` ～ `+15 m/s²`。
- 赤が X、緑が Y、青が Z。V1 RAW は生センサー軸、V2 は FLU 正規化軸である。
- M5 が送った `impact_candidate` は回数と最大加速度を同じ枠に表示する。
- 750 ms 以上 telemetry sample が来なければグラフを消す。停止した映像に古い値を残さないためである。
- この描画は Native Observer 専用で、共有メモリと MADSYSTEM には渡さない。

運用時は `TELEMETRY:BINARY` のグラフを見ながら衝撃候補の閾値を決める。V1 RAW は生軸の確認専用である。

## 実装前の負荷検証

実機が Relay に接続している状態で、以下を同一条件の映像 4 台表示と比較する。

1. 音声なし。
2. 1 台を音声購読。
3. `1` ～ `4` の切替を 5 秒間隔で繰り返す。

確認対象は Relay の CPU 使用率、Relay の送受信量、4 枠の映像 FPS、音声切替後の再生開始時間、`TEL:` の欠落数である。合格条件は、音声を 1 台に限定した状態で映像の停止・FPS 低下・telemetry の連続的な欠落を起こさないこととする。11.6 を含む上流が現在 `CONNECTING` のため、この実測は接続復帰後に実施する。
