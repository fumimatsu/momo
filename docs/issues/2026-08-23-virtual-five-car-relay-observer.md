# 2026-08-23 仮想 Momo 5 台 Relay / Team Observer 検証

## 目的

実車を 5 台用意せず、現在の 4 台運用を超えた場合の媒体経路を確認する。

検証範囲は次とした。

```text
録画ループ x 5
  -> Momo P2P WebSocket 互換の仮想上流 x 5
  -> 隔離 Relay
  -> Team Observer
```

Race Control、Coordinator、Marker Observer、S3 telemetry、実車 control はこの検証に含めない。

## 構成

| 項目 | 値 |
| --- | --- |
| 入力 | `cpu-shadow-20260731T093643811Z-9ee91411.webm` |
| 前処理 | 180 度回転、960x528、30 fps、H.264 Baseline、IDR 1 秒間隔 |
| 仮想上流 | `virtual-01` から `virtual-05` |
| carId | `CP-1` から `CP-5` |
| 仮想 Momo endpoint | `ws://127.0.0.1:18880/ws/<source-id>` |
| 隔離 Relay | `http://127.0.0.1:18190` |
| Team Observer | Relay 同一 origin の `/observer.html` |

仮想上流は Relay が使用する Momo P2P signaling と同じ `offer`、`answer`、trickle ICE を受理し、
H.264 RTP と `serial` DataChannel を個別の PeerConnection で確立する。各 source は同じ録画を使うが、
WebRTC session、RTP sequence、timestamp は独立している。

## 結果

5 source すべてで次を確認した。

- Relay state: `STREAMING`
- upstream peer: `connected`
- video health: `receiving`
- ingress access unit: 30 fps
- Relay write access unit: 30 fps
- upstream `serial` DataChannel: open
- RTP stall: 0
- last error: なし

Team Observer では次を確認した。

- RACE ORDER は 5 台を列挙する。
- detail video は既存契約どおり最大 4 枠を維持する。
- `CP-1`、`CP-2`、`CP-3`、`CP-5` へ選択を切り替えられる。
- 選択した 4 映像はすべて 960x528、再生可能状態、約 30 fps となる。
- ブラウザの console error / warning は発生しない。

これにより、Relay と Team Observer の媒体経路は 4 台固定ではなく、5 台目を fleet 一覧へ追加し、
選択した最大 4 台を監視する運用が成立した。

## 再現手順

```powershell
cd C:\src\momo
.\tools\Start-VirtualFiveCarDemo.ps1
```

別の入力を使う場合は `-InputPath`、台数を変える場合は `-CarCount` を指定する。

```powershell
.\tools\Start-VirtualFiveCarDemo.ps1 -InputPath C:\recordings\race.webm -CarCount 20
```

停止する。

```powershell
.\tools\Stop-VirtualFiveCarDemo.ps1
```

変換済み H.264、実行ファイル、PID、ログは `tools/.artifacts/virtual-five-car` に置き、Git には含めない。
起動時に Race Audio、Ayame、Race Control、dynamic source registry の親 process environment を遮断し、
通常運用の Relay へ接続しない。

## 解釈上の制限

この試験で確認できるのは、複数 Momo 相当の H.264 upstream、Relay の source 管理、WebRTC 再配信、
Team Observer の fleet 一覧と 4 映像選択である。

次は再現していない。

- 車両ごとに異なる映像をエンコードする CPU / GPU 負荷
- Wi-Fi、LAN、Internet の独立した損失、jitter、帯域競合
- S3 telemetry、M5 audio、Pilot command、FFB
- Marker Observer の複数映像推論負荷
- Race Control / Coordinator の 5 台 roster と timing snapshot

Race Control と Coordinator には現在も 4 台上限が残る。正式な 5 台レースでは、carId の固定 allow-list、
roster、standings、position、Coordinator draft validation を同じ上限へ更新し、schema と再送契約を含む
結合試験が必要である。媒体試験の成立を理由に、計時経路まで 5 台対応済みとは扱わない。

## 検証

```text
go test ./...                                               OK
Start-VirtualFiveCarDemo.ps1 PowerShell parse              OK
Stop-VirtualFiveCarDemo.ps1 PowerShell parse               OK
Relay 5 sources connected / receiving / 30 fps             OK
Team Observer 5 leaderboard rows / 4 playable video tiles  OK
Browser console error / warning                            0
```
