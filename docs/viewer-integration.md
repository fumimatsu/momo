# Viewer の正本と Relay 配布

## 責務

| リポジトリ | 責務 | Viewer の扱い |
| --- | --- | --- |
| `momo-fpv-viewer` | Viewer の正本 | Direct、Relay Pilot、Web Observer を管理する |
| `momo` | Momo 本体、Relay、Observer | Relay Pilot と Web Observer の配布先を持つ |
| `momo-fpv` | Pi 設定、ファームウェア、直結 Viewer の運用配布 | Relay Pilot の正本を持たない |

基本運用は Relay Pilot とし、Direct Viewer は単独走行、簡易検証、Relay 障害時の切り分けに限定する。

Relay Pilot の正本は `momo-fpv-viewer/variants/relay/` の `pilot.html`、`pilot.js`、`race-battle.js`、`garage.html`、`ffb-bridge.js` である。入力・FFB設定画面は root の `gamepad.html`、`gamepad.js`、`gamepad-profile.js` を正本として使う。`momo/tools/momo-relay/web/` は Relay binary に埋め込む配布コピーであり、直接編集しない。

Web Observer の正本は `momo-fpv-viewer/variants/observer/` の `observer.html`、
`observer.css`、`observer.js`、`observer-core.js`、`observer-config.json` である。
本番では Relay と同一 origin から配信し、ブラウザから Race Control へ直接接続しない。

## 更新手順

1. `momo-fpv-viewer/variants/relay/` を更新し、`npm test` と `npm run build:relay` を実行する。
2. `momo-fpv-viewer` をコミットして push する。
3. `momo` で `tools/sync-relay-viewer.ps1` を実行する。
4. `tools/momo-relay/web/viewer-source.json` の source commit を確認する。
5. `tools/start-mads-observer.ps1 -RebuildRelay` で Relay を再ビルドし、Pilot と Web Observer を強制再読み込みして確認する。

未コミットの Viewer を Relay へ配布しない。同期スクリプトは既定で未コミットの同期元を拒否する。

同期ファイルの正本は `momo-fpv-viewer/tools/distribution-targets.json` の `relay-web` である。
`sync-relay-viewer.ps1` や `tools/momo-relay/web/` へファイル一覧を手作業で追加しない。
同期スクリプトは直前の `viewer-source.json` に記録された管理対象のうち、新しい `relay-web` から外れた
stale file を削除する。記録にない運用ファイルは削除しない。

同期前に配布コピーが `viewer-source.json` の記録済み commit と異なる場合も、同期スクリプトは中断する。Relay client の変更は先に Viewer 正本へ移植してコミットする。移植済みの乖離を初回同期で置換する場合だけ、`-AllowDistributionDrift` を明示する。

`relay-web` 配布定義への初回移行では、`viewer.html` 互換エントリと PWA アイコンが新たに配布対象へ加わるため、既存コピーとの差分を確認した上で一度だけ `tools/sync-relay-viewer.ps1 -AllowDistributionDrift` を使う。以後の通常同期ではこのオプションを付けない。

FFB は Viewer PC のネイティブ bridge の責務である。Pi、Relay、ブラウザに DirectInput 実装を入れない。ブラウザ側は bridge が必要とする telemetry 契約だけを維持する。

本番 Web Observer は次の URL で開く。`relayHost` を省略するとページを配信した
`location.host` が接続先になる。

```text
http://<relay-host>:8090/observer.html
```

Relay の `web/` は `go:embed` されるため、同期後は Relay の再ビルドと再起動が必要である。
本番適用手順は `momo-fpv-viewer/docs/web-observer-production-deployment.md` を参照する。
