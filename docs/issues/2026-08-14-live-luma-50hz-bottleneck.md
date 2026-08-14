# Live luma 50 Hz bottleneck

Status: doing

## Context

The live Marker Observer no longer duplicates Relay/WebRTC input. Native Observer owns one
connection and decoder per source, publishes a fixed four-source I420 Y-plane batch through
`Local\MomoObserverLumaV1`, and keeps `Local\MomoObserverFrameV1` for MADSYSTEM visuals.
The Python/CuPy worker reads only the Y-plane mapping and publishes marker observations.

On the i7-8700 / Intel UHD 630 / RTX 3060 validation PC, four Relay inputs remained at
50 FPS while the complete Relay, Native Observer, MADSYSTEM External, and GPU worker stack
published about 17.5 marker batches per second with processing p95 around 89 ms. The same
warmed four-source replay reached 49.55 Hz in isolation and 17.716 Hz while the live stack
was active. Input decode and GPU compute capability alone therefore do not establish the
live 50 Hz capacity.

The current `processingMs` starts after `SharedLumaReader.read_latest` and stops after GPU
results return. It includes valid-source selection, pageable H2D copy, GPU work, D2H result
copies, and CUDA synchronization. It excludes shared-memory read/copy and observation IPC
publication, so it cannot identify one dominant stage.

## Goal

Measure the complete live-luma path by stage, remove avoidable allocation, copy, and
synchronization costs without changing marker semantics, and establish whether four-source
50 Hz operation with at least 20% operating headroom is achievable on RTX 3060 and RTX 5070
class validation PCs.

## Acceptance Criteria

- Report shared-memory read/host-copy, source selection, H2D, GPU kernel, D2H, result
  formatting, and observation IPC durations separately.
- Use CUDA Events for device time and a wall clock for host scheduling and transfer time.
- Record Native Y-plane publication rate, per-source sequence age, invalid-source intervals,
  Sink lock wait, and shared-memory write time.
- Reuse fixed-resolution NumPy and CuPy input buffers; do not allocate a new full frame batch
  during every detection cycle.
- Remove the advanced-indexing copy before `cp.asarray`, or measure and justify it if retained.
- Compare pageable copy, pinned staging, and registered shared-memory transfer. Use bounded
  latest-frame buffers and never queue stale frames to improve throughput.
- Combine GPU-to-host results into the minimum practical number of synchronizing transfers.
- Preserve duplicate physical markers with the same ID, source IDs and slot indices, reserved
  IDs 17/34/37, and the existing `Local\MomoMarkerObservationsV1` contract.
- Preserve the independent BGRA visual mapping until MADSYSTEM display ownership changes.
- Pass the existing replay and shared-memory unit tests and the current marker parity suite.
- For a claimed 50 Hz profile, sustain at least 47.5 published batches per second for four
  live sources, keep processing p95 at or below 20 ms, and then pass a ten-minute soak with
  at least 20% measured CPU/GPU headroom.

## Verification

- Run `python -m unittest tools.test_run_gpu_marker_observer_luma` in the repository GPU
  environment.
- Run a bounded `Run-GpuMarkerObserverLuma.ps1` report for one through four live sources.
- Repeat the four-source replay both alone and while Relay, Native Observer, and MADSYSTEM
  External are active.
- Compare per-stage p50/p95/p99 values before and after each optimization; change one
  transfer or synchronization condition per run.
- Complete checkpoint order, PIT ID, lap advancement, and Race Control snapshot E2E testing
  after the throughput gate passes.

## Notes

Source inspection found the following avoidable work:

- `Sink::OnFrame` scales Y to 960x528 and also converts the source to BGRA while holding the
  per-Sink frame lock.
- `WriteSharedLuma` clears the complete fixed buffer, copies every active source, and then
  copies all four planes into the triple-buffer mapping every 20 ms.
- `SharedLumaReader.read_latest` allocates a NumPy batch and copies selected planes every cycle.
- `batch.y_planes[valid_indices]` creates another host array before `cp.asarray` performs H2D.
- `GpuArucoDetector.detect_batch` returns candidate counts, IDs, valid masks, and corners with
  separate `cp.asnumpy` calls and launches marker decode per source.

The first implementation step is instrumentation. Optimizing only the Python loop, moving
all detection into Native code, or changing the detection rate before stage measurements
would hide whether the actual constraint is host scheduling, transfer, kernel work, or
full-stack resource contention. Native CUDA integration remains a later option. With Intel
decode and NVIDIA detection on separate adapters, it does not automatically create a
zero-copy path.

### Source-level implementation order

P0 makes the capacity result trustworthy before changing throughput:

1. Split the live report into `inputReady`, `throughputPassed`, and the final capacity
   result. The current `run_passed` accepts any one active source and does not enforce
   95% of the requested detection rate, although the runbook defines that gate.
2. Add explicit detector warm-up to replay measurement. A cold ten-second RTX 5070 replay
   on 2026-08-14 reported 46.978 Hz and failed despite 9.781ms processing p95 because one
   609.084ms startup cycle was included. Live luma already warms the detector before timing.
3. Add the stage timings and source-age counters described in Acceptance Criteria.

P1 removes bounded work without changing marker recognition:

1. In Native `Sink::OnFrame`, stop after generating the shared BGRA source and luma plane
   when `--shared-output-headless` is active. The second preview-oriented I420 scale and
   BGRA conversion still run today even though the SDL preview is not presented.
2. Replace the per-pixel horizontal-plus-vertical copy used by all four operational `HV`
   source flips with an existing SIMD-capable libyuv rotate/mirror operation, after parity
   tests confirm the exact orientation.
3. Let `SharedLumaReader` fill a reusable host batch. Avoid `batch.y_planes[valid_indices]`
   when all selected sources are valid, and use a reusable compact staging buffer otherwise.
4. Compare a reusable pinned host batch plus preallocated device input and asynchronous H2D
   against the current pageable `cp.asarray` path.

P2 changes the detector internals and therefore requires full marker parity validation:

1. Cache workspaces by `(batch, height, width)`. For four 960x528 inputs, `horizontal`,
   `labels`, `counts`, and eight uint64 component-key arrays cover approximately 154 MB per
   cycle before temporary corner arrays. CuPy's pool may recycle allocations, but the large
   zero/full initialization and object construction still occur for each detection.
2. Add a batch decode kernel that receives `candidate_sources`. The current implementation
   copies candidate counts to the host and launches decode once per source.
3. Return source index, decoded ID, validity, and corners with one packed D2H transfer instead
   of separate synchronizing `cp.asnumpy` calls.

P3 is architectural and should be attempted only if measured P0-P2 work cannot satisfy the
gate. Options are a native C++/CUDA detector boundary or a dedicated Marker node that does
not share its RTX and CPU scheduler with MADSYSTEM rendering. Either option must preserve
latest-frame dropping, source isolation, and the existing marker-observation contract during
migration.

## RTX 4060 Laptop GPU での 4 source live 結果

2026-08-14 に `192.168.11.100:8090` の Relay から `11.3`、`11.4`、`11.5`、`11.6` を
Native Observer で受信し、960x528 Y planeを4台batchとして60秒測定した。GPUは
NVIDIA GeForce RTX 4060 Laptop GPU、Native Observerはshared luma対応commit
`add0eb25`のWindows CI artifactを使用した。

| 条件 | publication | cycle p95 | 4 source coverage | 結果 |
| --- | ---: | ---: | --- | --- |
| 計測修正後のbaseline | 44.783 Hz | 20.997 ms | 100% | 不合格 |
| reusable pinned host batch + latest-frame scheduler | 46.383 Hz | 20.138 ms | 100% | 不合格 |
| 上記 + candidate workspace再利用、再接続後 | 48.067 Hz | 17.568 ms | 100% | 合格 |
| 同じPython経路の再測定 | 45.133 Hz | 20.830 ms | 100% | 不合格 |
| Native headless最適化後の再測定 | 47.350 Hz | 18.166 ms | 100% | 不合格 |
| 上記 + candidate filter融合 | 49.183 Hz | 13.771 ms | 100% | 合格 |

baselineに含まれていた初回実画像CUDA compileは、実フレームを2回warm-upして測定区間から除外した。
共有メモリread用host batch、CUDA device input、candidate抽出用の固定workspaceを再利用し、
全sourceが有効な通常経路からadvanced-indexing copyを除去した。pageable `cp.asarray`はpinned batchからの
preallocated device input `set`へ変更した。schedulerはduplicate frameを読んだ時に次の20ms slotを
消費せず、最新frame到着までbounded retryする。

baselineから最終結果までpublicationは3.284 Hz増え、cycle p95は3.429 ms減った。shared read p95は
1.398 msから0.452 ms、H2D wall p95は0.976 msから0.568 msへ減った。最終測定は2,884 batchを公開し、
4 sourceすべて2,884 frame、marker instance 20,236件を保持した。

途中の別測定では処理cycle p95 16.785 msを維持した一方、`11.4` coverage 58%、`11.5` coverage 82%で
失敗した。これにより、処理性能と入力健全性を単一の`passed`へ混ぜると原因を誤ることが確認できた。
reportは`inputReady`、`throughputPassed`、最終`passed`を分離し、4 sourceの各coverage 95%以上、
publication 47.5 Hz以上、cycle p95 20 ms以下を要求する。

この結果は60秒の短時間gateであり、運用上限の確定ではない。次は10分soak、1時間soak、
Native Observer再接続時のsource別停止時間、MADSYSTEM同居条件を分けて測定する。

同一PC、同一4 sourceでも短時間gateの再現性は確立していない。GPU時系列を取得した20秒測定では、
RTX 4060 Laptop GPUのgraphics clockが開始時2250 MHzから、温度56から57度、power violation 0、
thermal violation 0のまま660から840 MHzへ低下した。candidate filter p95は7.305 ms、decode p95は
4.038 msとなり、cycle p95は18.166 msに収まったがpublicationは47.350 Hzで47.5 Hz gateを
0.150 Hz下回った。現行実装はGPUのadaptive clock条件に対する余力がない。管理者権限なしの
`nvidia-smi --lock-gpu-clocks`は拒否されたため、運用でclock固定を前提にして合格扱いしていない。

stage分解ではcandidate filter、decode、unionの順で支配的だった。workspace reset p95は1.037 ms以下で、
再利用はallocationを除去したが、巨大な統計配列の初期化とcandidate後処理が残っていた。GPU側P2として、
candidate root scan、corner構築、valid判定を1つのRawKernelへ統合し、CuPyの複数elementwise launchと
dynamic intermediateを除去した。candidate filter p95は8.096 msから0.096 ms、candidate全体p95は
13.845 msから6.447 msへ低下した。全50 marker ID、同一IDの複数物理marker、source分離のparity testを
通過した。

融合後の4 source 60秒再測定は2,951 batch、49.183 Hz、cycle p95 13.771 ms、全source coverage 100%で
厳格gateを通過した。active区間のGPU clockは570から2250 MHz、平均906.7 MHzまで低下したが、SM平均
33.9%、最大43%、温度最大58度、power / thermal violation 0の条件で合格を維持した。frame age p95は
source別35.343から36.493 msだった。

最初の融合後60秒試験は途中で`cudaErrorUnknown`となった。同時刻のWindows System eventにNVIDIA
`nvlddmkm`と`NVDisplay.ContainerLocalSystem`の再登録があり、driver再初期化後は同じCUDA parity testと
60秒測定が成功した。この1件をkernel不具合または解決済みdriver問題のどちらにも断定せず、10分soakで
driver eventを併記する。

Native P1として、`--shared-output-headless`ではMADSYSTEM共有BGRAとshared luma生成後にreturnし、
表示専用の2回目scale / BGRA変換を停止した。HV反転のshared lumaコピーはscalar per-pixel loopから
libyuv `MirrorPlane`へ変更した。Windows CUDA無効ビルドは成功した。この変更はNative側CPUとSink lockを
減らすが、RTX上のcandidate処理時間を直接短縮するものではない。
