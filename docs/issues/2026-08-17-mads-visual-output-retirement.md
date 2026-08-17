# MADSYSTEM visual output retirement

Status: done

## Context

Native ObserverはMarker Observer向けY-planeと、MADSYSTEM映像演出向けBGRA compositeを同時に
公開していた。Marker検出はBGRAを使わないため、MADSYSTEMの映像演出を外す移行段階では変換と共有出力が
不要になる。一方、現行運用へ影響させないため既定動作は維持する必要がある。

## Goal

Relay/WebRTC receive、decode、Y-plane、MMO1 marker observationを維持したまま、MADSYSTEM向け
`Local\MomoObserverFrameV1`を明示的に停止できるようにする。

## Acceptance Criteria

- `start-mads-observer.ps1`と`start-mads-stack.ps1`が`legacy|off`を受け付ける。
- 既定`legacy`はBGRA compositeとY-planeを従来どおり公開する。
- `off`は`--shared-frame-name`を渡さず、Y-planeだけを公開し、Native previewも停止する。
- モードが異なる既存Observer processを誤って再利用しない。
- Program ObserverはBGRA mappingを入力またはfallbackとして使用しない。

## Verification

- 2026-08-17: PowerShell parserで両起動scriptの構文を確認した。
- 2026-08-17: Visual Studio 2022構成でNative Observer Release buildに成功した。
- 2026-08-17: `legacy`と`off`のprocess argument判定をdiff reviewした。

## Notes

- `-ObserverHeadless`だけではlegacy BGRA出力は止まらない。
- MADSYSTEMを完全廃止する段階ではBGRA shared frameの実装自体を別変更で削除する。
- FHD俯瞰カメラとProgram Directorはこの変更に依存せず、Relay `venue` sourceから構築する。
