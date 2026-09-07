# Pilot 映像は動くが LAP 計測だけ停止する障害

## 確認した事実

- 2026-09-07 のローカル運用。Relay は `127.0.0.1:8090`、Coordinator は `8790`。
- Pilot の映像・操縦と Relay の STREAMING は正常でも、Marker 用の
  `p2p-marker-recv` は独立プロセスなので、停止すると LAP を計測できない。
- Receiver PID 132828 は 18:24、再起動した PID 89836 は 20:18 に異常終了した。
- 両方とも `momo.exe + 0xe7f6d1`、例外 `0xc0000005`。
- 対象 EXE の `--version` は `2025.1.3 (90230d7f)`。
  既存の video receiver 所有権修正は適用済みだった。
- ダンプは `%LOCALAPPDATA%\CrashDumps\momo.exe.<PID>.dmp` に保存されている。
  ダンプにはメモリ内容が含まれるため Git に追加しない。

## 原因と修正

ダンプの障害命令は null ポインターの読み取り。EXE の逆アセンブルと埋め込みの
ソース名・ログ文字列を照合し、`rtc_connection.cpp` の
`Created session description` に続く `SetLocalDescription` 呼び出しと特定した。

SDP 生成完了の非同期 callback は生の `this` を保持していた。
一方、`CloseDetached()` は `connection_` を空にし、呼び出し元は RTCConnection 自体も
解放できる。完了通知が遅れて届くと null または解放済みオブジェクトを参照する。

- Offer / Answer の callback は RTCConnection の `this` を参照せず、PeerConnection の
  参照を保持する。closed なら生成済み SDP を解放して終了する。
- SetLocalDescription に渡す SDP と callback 用 SDP の所有権を分け、失敗・終了時にも
  移譲済みポインターを読み出さない。
- MLY2 writer の再初期化は共有メモリの全消去中も topology seqlock を奇数に保つ。
  生き残った reader が中途半端な header を読むのを防ぐ。
- writer 再起動でも receiver generation を進め、古い slot / sequence を再利用しない。
- Python reader は未初期化のゼロ header を待機として扱い、整合性を確認してから
  contract / source ID を検証する。安定した不正 contract は引き続きエラーとする。
- Operations の警告は「映像が古い」から「計測用映像が停止」へ変更する。
  Pilot 映像とは別経路だと明記する。

## 試験と適用

Python は `test` ディレクトリから `uv run pytest -v` を使用する。
Marker GPU テストには numpy / OpenCV のある既存 GPU venv を指定する。

`test/test_marker_receiver_shutdown.py` は隔離した HTTP / WebSocket サーバーと一意の
共有メモリ名を使い、接続準備中の close と manifest 更新を繰り返す。
`TEST_MARKER_RECEIVER_EXECUTABLE` で候補 EXE を選択できる。
旧版でも 1 回の試験は通ったため、これは障害の決定的な再現試験ではない。

Windows の候補ビルド:

```powershell
uv run --no-project --python 3.13 python run.py build windows_x86_64 --disable-cuda --relwithdebinfo
```

Relay / Pilot / 車載 Momo はこの障害の修正対象ではない。
更新対象はローカル Marker receiver と Python 認識側。
適用後はプロセスの存在だけでなく、MLY2 フレーム時刻、認識出力時刻、確定通過イベント、
Race Control の LAP 増加を確認する。停止中の通過は復元できない。

2026-09-07 の検証結果:

- Python reader / GPU worker 関連: 22 件成功。
- 修正版 EXE の接続切断試験: 12 接続以上を処理し成功 (44 秒)。
- Coordinator `Invoke-Validation.ps1`: 成功 (Go test / vet、Node 21 件、運用 script 検証)。
- Windows RelWithDebInfo ビルド: 成功。次の障害解析に使える PDB も生成。
- 候補 EXE SHA256: `3433a3ea086441f5272080f1e6da200758dae74dbfd5cae352210558f7f9cd23`。

実運用 receiver の Start-Process は実行ツールから `blocked by policy` と拒否された。
その後、ユーザーの手動実行で 20:47 に receiver PID 134276、認識 PID 95160 の
起動と実機映像・認識出力の更新を確認した。
端末専用の `momo-race-timing/state/local-experience/Start-FixedMarker.ps1` に
候補 EXE の hash 確認、二重起動防止、計測用映像と認識出力の確認をまとめた。
この script は上記 SHA256 のクラッシュ対策版を起動する。下記の ABORT 対策版への
切り替えはまだ行っていない。

Operations の警告文言はソースと配布候補を更新済み。
実行中の Coordinator を再起動していないため、8790 の表示更新は未反映。

## ABORT 後の一時的な計測用映像停止

手動復旧後に ABORT / Prepare を操作すると、11.4 の「映像が古い」が再度表示された。
今回はプロセスの異常終了ではなく、その後に同じ receiver / 認識 PID のまま復帰した。
確認時点の 11.4 は frame age 58 ms、recognition age 36 ms、検出 40 Hz。

認識ログには、対象が 1 台のまま次の再初期化が残っていた。

```text
MLY2 generation=2 phase=idle sources=1
MLY2 generation=3 phase=countdown sources=1
MLY2 generation=4 phase=idle sources=1
MLY2 generation=5 phase=ready sources=1
```

Relay manifest の revision は raceRunId、selectionMode、carId などでも変わる。
receiver は revision が変わると、同じ sourceId / observerPath でも全 PeerConnection を
閉じ、共有メモリと GPU 側の構成を作り直していた。警告を隠すのではなく、不要な
映像再接続をなくす。

- 同じ順序の sourceId / observerPath なら接続、映像、generation を保持する。
  phase と manifest revision hash、診断用 carId のみ更新する。
- 参加車両の追加・削除・順序変更や observerPath 変更では従来どおり再構成する。
- countdown / green 中の構成変更は保留する。ただし解除時に以前の保留内容を再生せず、
  最新の manifest を正とする。ABORT で取り消された古い参加車両を復活させない。
- 本当にフレームが停止した場合の鮮度チェックは変更しない。

`test/test_marker_receiver_manifest.py` は本番 Relay と別のローカル signaling server と
共有メモリ名を使う。実映像は流さず、接続数・generation・revision hash・phase を検証する。
修正前 EXE では ABORT 相当の idle 更新だけで generation が 1 から 2 となることを再現。
修正版は状態遷移、保留の取り消し、追加・削除、接続先変更、0 台を含め成功 (約 46 秒)。
追加・削除の試験は実際の PeerConnection 構築を伴うため、非同期処理の待機は 40 秒を上限とする。

今回の候補で接続切断試験 1 件、manifest 試験 1 件、Python reader / GPU worker 22 件が成功。
Windows RelWithDebInfo ビルド成功。通常の build 出力先には稼働中の EXE があるため、
今回だけ CMake の出力先を別ディレクトリへ向けてから `run.py build` を実行し、
ビルド後に CMake 設定を元へ戻した。

- 候補: `_build/windows_x86_64/release/momo/marker-lifecycle-candidate/momo.exe`
- SHA256: `a2c2e4a5f06bad022bbcb1cfe17481bb7c0ccf679dd2340481354920b58a817b`
- 同じディレクトリに `momo.pdb` を保存。
- 本番 receiver PID 134276 は前段のクラッシュ対策版で稼働継続。今回の ABORT 対策版は未適用。
- Relay、Pilot、車載 Momo、レース状態の変更は行っていない。

次の適用では計測用 receiver の起動先と hash を候補に切り替える。
Relay 自体は再起動不要。同一車両で Prepare / ABORT を繰り返してフレームと generation を
確認し、実車の確定通過と LAP 増加も確認する。机上試験は実映像での連続性確認の代替ではない。

### 適用依頼後の状況

ユーザーからレース終了後の適用依頼を受け、Coordinator が prepared、送信待ち 0 の状態を
確認した。検証済み EXE / PDB は端末内の
`momo-race-timing/state/local-experience/bin/marker-receiver-20260907-v2/` に配置済み。
ビルドフォルダーから切り離し、`Start-FixedMarker.ps1` の起動先と hash を更新した。
旧 launcher は `Start-FixedMarker.before-lifecycle.ps1` に保存。

切り替え用の Stop-Process / Start-Process コマンドは実行前に再び `blocked by policy` と
拒否された。旧 receiver PID 134276、認識 PID 95160、Relay PID 78348、Coordinator PID 3120 が
継続稼働していることを確認した。ファイルの配置完了とプロセスへの適用完了を区別する。

手動適用は端末専用の `state/local-experience/Apply-FixedMarker.ps1` を実行する。
この script は非走行状態、対象モード・共有メモリ名・旧 EXE パス、候補と rollback の hash を
照合してから receiver だけを切り替える。映像・認識の復帰を確認し、失敗時は旧 receiver を
起動し直す。構文チェック済みだが、適用自体はまだ実行されていない。

### 手動適用後の実機確認

2026-09-07 21:24:42 にユーザーが適用 script を実行した後、次を確認した。

- 新 receiver PID 792 は運用フォルダー `bin/marker-receiver-20260907-v2/momo.exe` で稼働。
- 認識 PID 95160 は再起動せず継続し、新 receiver の映像を受信。
- 11.4 の videoValid は true、確認時点の frame age 59 ms、recognition age 29 ms。
- 認識出力は 40 Hz。起動時の 50 Hz 上限と adaptive 設定は変更していない。
- Coordinator は prepared のまま。Relay / Pilot / Race Control の再起動は実施していない。

ABORT 対策版の実機適用と映像・認識の復帰確認は完了。
適用後の ABORT / Prepare 反復と、走行による LAP 加算は別途確認する。

### 2026-09-08 のコミット前検証

`test` で `uv run pytest -v` を実行し、41 件成功、43 件スキップ、1 件失敗。
失敗は manifest の generation 保持テストで、既定の build 出力に残る修正前 EXE を
選択したため。上記 SHA256 の `marker-lifecycle-candidate/momo.exe` を
`TEST_MARKER_RECEIVER_EXECUTABLE` に指定すると、manifest / shutdown の 2 件とも成功した。
旧 EXE に対する全体テスト成功とは扱わない。Sora / GPU などのスキップ条件も残る。
今回の検証では運用中プロセスの再起動・入れ替えは行っていない。
