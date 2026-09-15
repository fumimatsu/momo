# Marker 自動降格・継続を標準化する

- Type: enhancement
- Status: local regression verified; live driving qualification pending
- Related: [会場の停止と25 Hz回復](2026-09-13-marker-best-effort-capacity.md)

## 目的と原因

2026-09-13はRelayに4台を設定し、そのうち2台をMarker認識対象にして運用した。
初回は `--initial-detection-hz 50 --no-adaptive` だったため、自動降格せず停止した。
停止窓の処理p95は26.36 ms、期限超過率は約13%。平均の検出epochが約50 Hzでも、
5秒窓のp95と期限超過率による負荷判定とは矛盾しない。
復旧後は25 Hz開始・自動降格・継続に変更したが、旧実装は25 Hzが下限だった。
さらに昇格の試行がMLY2のgeneration変更時に限られ、同じ車両で次のレースへ
進むとPrepare時の昇格機会がなかった。

利用者の方針は、可能な限り高いHzを維持しつつ、25 Hz未満でも計測を継続すること。
この変更は認識周期と容量超過時の運用方針を扱う。正式計時の条件は緩和しない。

## 標準設定（2026-09-15以降）

- 50 Hzから開始し、50 → 40 → 33 → 25 → 20 → 15 → 10 Hzの順に降格する。
- 5秒窓の処理p95が周期の80%を超える、または期限超過率が5%を超える状態が
  3窓連続すると1段階降格。変更後の10秒ホールドを維持する。
- 10 Hzでも過負荷なら `degraded / continuing_best_effort` を記録して処理を継続。
  10 Hzは今回の設定下限であり、実効10 Hzや正確な通過捕捉の保証ではない。
- `--capacity-policy auto` が既定。自動降格有効なら継続、固定Hzなら従来の停止。
  明示的な `stop` は自動降格時も指定可能。固定Hzと明示的な `continue` は拒否する。
- 有限時間の容量検証は、過負荷を継続しても合格扱いにしない。
  `Invoke-DynamicMarkerCapacityValidation.ps1` は明示的な `stop` で検証する。

標準の運用例（設定を明示した場合）：

```powershell
.\tools\Run-GpuMarkerObserverLumaV2.ps1 -InitialDetectionHz 50 -CapacityPolicy auto
```

固定25 Hzの失敗判定付き検証：

```powershell
.\tools\Run-GpuMarkerObserverLumaV2.ps1 -InitialDetectionHz 25 -NoAdaptive -CapacityPolicy stop -DurationSeconds 120
```

## 昇格と入力欠落

同じ車両・同じgenerationでも、別phaseから `ready` へ入ったときに1回だけ昇格を試す。
条件は連続60秒、処理p95が現在周期の55%未満かつ期限超過率1%未満。
従来の余裕判定を維持して頻繁な上下動を避ける。これは最大Hzを常時探索する制御ではない。
Green中の昇格はせず、固定Hz指定時も昇格しない。

映像未接続・欠落・遅いepochを「処理余力」と誤認しないため、健全時間の加算には
全対象車両の新規フレーム処理率と実効epoch Hzが設定比95%以上であることも必要。
generationが変わる場合は、旧構成の健全時間・連続過負荷回数・途中の制御窓を破棄する。

## 継続して記録する指標

強制終了でも判断材料が残るよう、5秒ごとにstdoutへ `marker_detection_window` JSONを出す。

- `atUnixNs`、phase、generation、対象台数
- `measuredDetectionHz`（評価した窓の設定Hz）、`detectionHz`（判定後の設定Hz）
- 処理p95、期限超過率、実効epoch Hz、入力充足、判定理由
- 車両別の新規検出フレーム数・実効検出Hz・freshTickRatio

昇格試行は `marker_rate_preparation`、下限過負荷はstderrの `marker_worker_status` に残す。
epoch Hzと各車両の実効検出Hzは別指標。処理p95は映像待ち時間を除いた値であり、
レースの通過確定遅延そのものではない。無制限の履歴配列・フレームキューは追加しない。

## 維持する境界

- 映像受信は50 fpsのまま。認識時は最新フレームを選び、重複・古い映像を通過に補完しない。
- 計時は `confirmedAtTimingNs` を維持。候補検出時刻への変更はしない。
- 現行の退出4フレームは変更しない。4周期の目安は50 Hzで80 ms、25 Hzで160 ms、
  20 Hzで200 ms、15 Hzで約267 ms、10 Hzで400 ms。
  実際の退出確定時間は映像・位置・フレーム欠落にも左右され、これらの値を保証しない。
- 低Hzでは高速通過の取りこぼしや確定時刻のばらつきが増え得る。入力鮮度、資格判定、
  Clock、authority lease、認証などの既存拒否条件は緩めない。
- GPU/IPC例外や受信停止まで自動復旧する変更ではない。

## 検証と残る確認

制御・Worker・鮮度・IPCの回帰テストで、10 Hzまでの全段階、下限警告継続、
固定Hz停止、同じ車両のReady遷移、欠落時の昇格抑制、窓別ログを確認する。
Windowsの実共有メモリでも20/15/10 Hzへの変更後に観測を書き込めることを確認する。

2026-09-15の実行結果：Python回帰62件成功、変更対象PowerShell 4本の構文検証成功。
本番とは別名の共有メモリにPythonから50→10 Hzへ変更しながら6件を書き込み、
Timing側の既存Go probeで `configuredHz=10 / sequence=6 / batchPresent=true / sources=1`
を確認した。本番サービス・車両・正式計時APIは使用していない。

```powershell
Set-Location <momo-repository>/tools
& <aruco-python.exe> -m unittest test_run_gpu_marker_observer_luma_v2 test_marker_detection_rate_controller test_marker_frame_sampler test_marker_runtime_metrics test_marker_luma_v2 test_marker_observation_ipc
```

次の走行では4台待機・2台対象と4台対象を分け、処理p95だけでなく車両別実効Hzと
通過漏れを確認する。25 Hz未満での実走行精度、長時間負荷変動、レース中の降格を
含む時刻のばらつきは別途実測が必要。ローカル回帰試験を本番精度保証とはしない。
