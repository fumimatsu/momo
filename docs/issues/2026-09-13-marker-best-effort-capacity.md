# Marker 過負荷時の継続優先モード

- Type: enhancement
- Status: implementation verified for handoff; full-event qualification pending

## 現象と期待動作

会場の2台走行中、50 Hz固定のWorkerが `downgrade_locked` により停止し、
OperationsがMarker入力待ちになった。終了時の処理p95は26.36 ms、期限超過率は約13%。
利用者は認識頻度を下げて継続する運用を希望した。

## 変更

`Run-GpuMarkerObserverLumaV2.py` に `--capacity-policy stop|continue` を追加。
既定は従来どおり `stop`。`continue` は `--adaptive` が必須。
既存の50/40/33/25 Hz制御を使い、下限でも負荷が高ければ5秒の制御窓ごとに
`marker_worker_status` の `state=degraded`、`publication=continuing_best_effort` を出して
最新フレームの処理を継続する。無制限のフレームキューや自動再起動ループは追加しない。
過負荷検出は消さず、有限時間の性能検証も成功扱いにしない。
報告には `capacityPolicy` と `capacityStopped` を追加する。

今回の会場は次のオプションで起動する。映像受信そのものは50 fpsのまま。

```text
--initial-detection-hz 25 --adaptive --capacity-policy continue
```

十分に健全な期間があった後、既存制御によりPrepare時だけ上位のHzへ戻ることがある。
Green中は負荷による降格だけで、積極的な昇格はしない。

## 維持する境界と注意

- 古い映像、無効映像、重複フレームを有効な通過として補完しない。
- GPU/IPC例外、計時権限・認証・資格情報・レース状態の失敗は隠さない。
- コースは必須の周回1、任意3/4、PIT8、最短周回5秒を維持。
- 現行の退出4フレームを変えないため、正常に25 Hz供給される場合でも確認は約160 ms。
  50 Hzの約80 msより遅れる。候補の観測時刻と通過確定時刻は別々に保存されるが、
  今回の Coordinator の `timestampBasis=timing` は `confirmedAtTimingNs` を使う。
  「候補フレームの時刻で計時する」という初期メモは誤りであり、2026-09-15に訂正した。
  検出間隔・確定待ちのばらつきは、高速通過の捕捉率と区間時間の両方に影響し得る。
- `continue` は精度や25 Hz実効性能の保証ではない。過負荷や入力欠落を残して運用判断する。
- Coordinator、Relay、Race Controlの再起動は不要。停止済みWorkerだけを起動する。

## 検証

- 既定の停止、明示的な継続、固定Hzと継続指定の拒否を単体テスト。
- 偽Reader/Writerのメインループで、停止は1回の書き込み後に終了、継続は複数回の書き込みを確認。
- 既存の段階的降格・入力鮮度・IPCテストを維持。
- 実車での高速周回・長時間負荷変動・精度比較は別途必要。

### 2026-09-15 引き継ぎ前の再検証

Python 3.12.14 と既存の ArUco 用仮想環境で、次の52件が成功した。
GPU実画像による精度試験ではなく、Worker制御・鮮度・IPCの回帰検証である。

```powershell
Set-Location <momo-repository>/tools
& <aruco-python.exe> -m unittest test_run_gpu_marker_observer_luma_v2 test_marker_detection_rate_controller test_marker_frame_sampler test_marker_runtime_metrics test_marker_luma_v2 test_marker_observation_ipc
```

継続モードの適用条件は会場で明示的に選ぶ。既定の停止方針、コース条件、計時の時刻基準は変更しない。
別環境では起動スクリプトにもオプションを明示し、`capacityPolicy` と実効Hzを確認する。
利用者も、手前から見え始める候補検出より通過確定を基準とする方針を確認した。
今後の調査は候補が確定へ進まない理由と確定待ちのばらつきを扱い、検出時刻基準への変更を前提にしない。
