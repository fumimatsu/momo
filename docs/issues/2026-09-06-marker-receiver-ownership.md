# Marker Receiver の接続解放時の所有権

- Status: doing

## Context

動的 Marker source の入れ替えで Source が receiver を破棄しても、RTCConnection の
PeerConnectionObserver が track 解放のために receiver を参照する期間が残り得る。
従来の raw pointer だけでは、その期間の生存を保証できない。

## Change

- Marker source の receiver を shared ownership にする。
- P2PReceiverClient -> RTCManager -> PeerConnectionObserver へ owner を渡す。
- PeerConnectionObserver の destructor で track を外し終わるまで owner を保持する。
- owner を渡さない既存接続の挙動は変更しない。

## Verification

Windows `python run.py build windows_x86_64 --disable-cuda` で変更箇所のコンパイルは成功。
稼働中 Marker が通常の `momo.exe` を使用していたため、通常リンクは LNK1104 になった。
停止させず、同じ build の MSBuild `TargetName=momo-20260906-verify` で別名リンクを行った。
検証用成果物と現在稼働中の EXE、Pi へ配布済みの ARM 成果物は区別する。

- 通常の `test` ディレクトリで `uv run pytest -v`: 40 件成功、Sora 環境未設定で 43 件スキップ。
  この実行は既存の通常名 EXE を対象にした基準確認である。
- 今回の別名 EXE をテストモジュールの探索先に明示して P2P / metrics を再実行: 8 件成功。
  複数プロセスの同時起動と動的生成・終了を含む。テスト用の探索先変更はプロセス内だけで、
  リポジトリや稼働中プロセスの設定は変更していない。
- 検証用 EXE の SHA-256:
  `444f72e244c7247b1f84d0d02a24bb6ec5493694bd6b50622d808c003eb011cc`。
- 稼働中 Marker の PID 72528 は維持した。この EXE の現場への差し替えは未実施。

## Remaining

実機で source の追加・削除・再接続を繰り返す長時間試験と、メモリ破壊検出付き試験は未完了。
この修正だけでレース運営の再開問題すべてが解決したとは扱わない。
