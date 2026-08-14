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
