# Viewer の正本と Relay 配布

## 責務

| リポジトリ | 責務 | Viewer の扱い |
| --- | --- | --- |
| `momo-fpv-viewer` | Viewer の正本 | Direct と Relay Pilot Variant を管理する |
| `momo` | Momo 本体、Relay、Observer | Relay Pilot の配布先を持つ |
| `momo-fpv` | Pi 設定、ファームウェア、直結 Viewer の運用配布 | Relay Pilot の正本を持たない |

Relay Pilot の正本は `momo-fpv-viewer/variants/relay/` である。`momo/tools/momo-relay/web/` は Relay binary に埋め込む配布コピーであり、直接編集しない。

## 更新手順

1. `momo-fpv-viewer/variants/relay/` を更新してテストする。
2. `momo-fpv-viewer` をコミットして push する。
3. `momo` で `tools/sync-relay-viewer.ps1` を実行する。
4. `tools/momo-relay/web/viewer-source.json` の source commit を確認する。
5. `tools/start-mads-observer.ps1 -RebuildRelay` で Relay を再ビルドし、Pilot 画面を強制再読み込みして確認する。

未コミットの Viewer を Relay へ配布しない。同期スクリプトは既定で未コミットの同期元を拒否する。

FFB は Viewer PC のネイティブ bridge の責務である。Pi、Relay、ブラウザに DirectInput 実装を入れない。ブラウザ側は bridge が必要とする telemetry 契約だけを維持する。
