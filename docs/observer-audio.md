# Native Observer の M5 音声

`p2p-recv-multi` は映像と別に、選択した 1 台だけの `momo-telemetry` DataChannel を作成できる。Relay は既存の `AUD:` フレームをその DataChannel に転送するため、Relay のプロトコル変更は不要である。

Windows Native Observer をビルドする SDL3 は `SDL_AUDIO=ON` が必須である。無効な SDL3 をリンクすると、画面上で `INIT ERR` と `SDL not built with audio support` が出て再生できない。

```powershell
.\tools\start-mads-observer.ps1 -ObserverAudioSource 11.6 -RestartObserver
```

- `--audio-source` を指定しなければ音声を購読しない。
- 指定できるのは `--source` で登録した名前のいずれか 1 台だけ。
- `AUD:1` の 8 kHz IMA ADPCM を Native Observer が PCM に復元し、Windows の既定再生デバイスへ出力する。
- 4 台の常時ミックスは実装しない。音声が重なり、Relay からの追加転送も 4 倍になるためである。

## 運用中の切替設計

起動時の `--audio-source` は初期選択値として残す。運用中の音声切替は、Observer の `1` ～ `4` キーで行う予定である。映像 PeerConnection、共有メモリ、MADSYSTEM は切り替えない。

DataChannel は次の 2 本を各 Source の Observer 接続で事前に開く。

| Label | 信頼性 | 役割 |
| --- | --- | --- |
| `momo-observer-audio-control` | reliable / ordered | `AUDIO:1` と `AUDIO:0`、Relay からの確認応答 |
| `momo-observer-audio` | unreliable / unordered | 選択中の Source だけに送る `AUD:` フレーム |

切替時は、旧 Source に `AUDIO:0`、新 Source に `AUDIO:1` を送る。Relay は Source ごとの購読状態を保持し、`AUD:` のみを `momo-observer-audio` へ配信する。通常の `momo-telemetry` と Pilot Browser の挙動は変更しない。

この方式なら、未選択の 3 台には制御メッセージ以外を流さない。4 台の音声を常時転送してから Observer で破棄する設計は禁止する。

## 実装前の負荷検証

実機が Relay に接続している状態で、以下を同一条件の映像 4 台表示と比較する。

1. 音声なし。
2. 1 台を音声購読。
3. `1` ～ `4` の切替を 5 秒間隔で繰り返す。

確認対象は Relay の CPU 使用率、Relay の送受信量、4 枠の映像 FPS、音声切替後の再生開始時間、`TEL:` の欠落数である。合格条件は、音声を 1 台に限定した状態で映像の停止・FPS 低下・telemetry の連続的な欠落を起こさないこととする。11.6 を含む上流が現在 `CONNECTING` のため、この実測は接続復帰後に実施する。
