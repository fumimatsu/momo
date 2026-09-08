# Observer画面のFPSを走行ログに残す

- Status: done

## Context

2026-09-07の記録には画面FPSがなく、Observerで30fps程度まで低下した観測を後から照合できなかった。

## Goal

既存のRelayログ有効化だけで、選択車体ごとの画面表示と同じFPSを保存する。
追加のWebRTC統計取得は行わず、Relay側も既存のFPS計測を使う。

## Acceptance Criteria

- 約1秒窓の画面FPS、タブ表示状態、最終フレームからの経過時間をNDJSONへ保存する。
- 表示とログで計測を共通化し、WebRTC統計取得、合成フレーム差分、テレメトリ受信集計を除去する。
- 車体、Viewer接続、Relay受信時刻、受信時のレース文脈を付ける。
- 0と未取得を分離し、映像停止・タブ休止・再接続を正常値で埋めない。
- 既存の操縦・計測・停止開始契約を変えず、診断の送信待ちをためない。
- Viewer正本からRelay配布を生成し、配布元commitを検証する。

## Verification

- Viewerの `node --test`: 273件成功。
- `tools/Invoke-RelayTests.ps1`: 同期後も全Go package成功、Observer/Marker launcher 20ケース成功。
- 画面とログが同一サンプル・単一タイマーを使うこと、60→30→0fpsの反映、getStats呼び出し0回、
  未対応null、タブ休止、送信待ち混雑、再接続時の破棄を動作テストで確認。
- 実WebSocketでSDP offer → schema 2開始通知 → observer_stats送信 → NDJSON永続化を確認。
  廃止したschema 1・RTC/telemetryフィールド、不正値・連番・roleも検証。
- Viewer `6e2e6f49286e9062fc2636495019420fade9e4bd` からRelay配布を同期。
  `-CheckOnly` 成功、2回目の同期でも全配布ファイルのSHA256不変。
- Windows amd64 Relayビルド成功。更新用 `artifacts/momo-relay.exe` を再作成。
- 実車でのFPS低下再現・実端末の追加前後負荷比較は未実施。
  稼働Relayへの適用は別作業。公開対象はFPSログの最終変更とその配布に限定する。

## Notes

- 公開先: Viewer `main` / Momo `master`。公開準備ブランチは `codex/publish-observer-screen-logging`。
- 公開の起点: Viewer `e04d28a` / Momo `338d3c14`。元の作業ブランチからFPSログの変更だけを切り出した。
- 未公開のfused V4更新は元の作業ブランチに保持し、今回の公開には含めない。
- 実装契約: Viewer正本の `docs/observer-screen-logging.md` と本リポジトリのRelay README。
- 適用にはViewer配布を含むRelayの更新とObserverの再読み込みが必要。
- ロールバックは対応するViewer配布とRelayをこの変更前へ戻す。既存NDJSONは削除しない。

- WebRTC統計の記録は今後の検討事項。必要性と負荷を評価するまでは実装・flag・互換経路を残さない。
