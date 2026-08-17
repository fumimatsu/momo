# Marker Observer Legacy Adapter Guide

## Current scope

The first integration slice moves ArUco image recognition outside MADSYSTEM while keeping
MADSYSTEM as the timing engine. It is intentionally local and reversible.

```text
Relay source x 4
  -> one Native Observer WebRTC connection and decode per source
  -> Video Sink I420 Y plane batch (Local\MomoObserverLumaV1)
  -> GPU DICT_4X4_50 detection at 50 Hz
  -> Local\MomoMarkerObservationsV1
  -> MadsObserverBridge.dll
  -> ArUcoWebCamMulti external mode
  -> existing checkpoint, PIT, pilot assignment, and Race Control publisher paths
```

The existing `Local\MomoObserverFrameV1` composite video mapping is an optional transition
path for MADSYSTEM visuals. Marker observation IPC does not carry video and does not create
or modify a control DataChannel. Normal marker processing requires only
`Local\MomoObserverLumaV1` and `Local\MomoMarkerObservationsV1`.

This slice consumes live Relay/WebRTC sources but does not emit qualified checkpoint or PIT
events. MADSYSTEM still owns temporal confirmation, passage semantics, lap/sector state,
and Race Control API publication.

`Run-GpuMarkerObserverSharedFrame.ps1` remains available as a transitional diagnostic path.
This validation path copies BGRA through `Local\MomoObserverFrameV1`, so it must not be used
for capacity decisions or normal Marker Observer operation.

## Replay producer

Initialize the existing GPU validation environment once, then run four simulated sources:

```powershell
.\tools\Initialize-ArucoCapacity.ps1 -IncludeNvCodec
.\tools\Run-GpuMarkerObserverReplay.ps1 `
  -DurationSeconds 30 `
  -OutputPath .\tools\.artifacts\marker-observer\replay.json
```

With no `-Sources`, the wrapper reuses the validated 960x528/50 FPS recording as
`sim-01..sim-04`. Use repeated `SOURCE_ID=VIDEO_PATH` values to select other inputs:

```powershell
.\tools\Run-GpuMarkerObserverReplay.ps1 -Sources @(
  'car-a=E:\recordings\car-a.mp4',
  'car-b=E:\recordings\car-b.mp4'
)
```

IDs 17, 34, and 37 are excluded from the operational allowlist. Multiple physical markers
with the same ID remain separate observations.

## Live Y-plane producer

`start-mads-observer.ps1` defaults to `-ObserverVisualOutput legacy` and starts Native
Observer with both mappings. The Native process owns the Relay/WebRTC connection and decoder
exactly once per source. Its Video Sink copies the decoded I420 Y plane into a fixed
four-source triple buffer before any BGRA conversion:

```powershell
.\tools\start-mads-observer.ps1 -ObserverHeadless
.\tools\Run-GpuMarkerObserverLuma.ps1 `
  -DetectionHz 50 `
  -OutputPath .\tools\.artifacts\marker-observer\live-luma.json
```

When MADSYSTEM no longer needs the composite video for presentation, start the same receive
and marker path without creating `Local\MomoObserverFrameV1`:

```powershell
.\tools\start-mads-observer.ps1 `
  -ObserverVisualOutput off `
  -RestartObserver
```

`off` keeps Relay/WebRTC receive, decode, Y-plane publication, and MMO1 marker observations
active. It omits `--shared-frame-name` and forces Native Observer headless so the BGRA
conversion, shared frame publication, and preview window are not part of the operating path.

To validate a feature build without replacing the operational binary, pass its full path
with `start-mads-observer.ps1 -ObserverExecutable <path> -RestartObserver`.

Use `-SourceIds @('11.5')` to diagnose only selected configured sources. With no source
selection, the producer follows the source IDs and slot indices published by Native
Observer. It copies all valid selected planes to CUDA in one batch, performs no BGRA-to-gray
conversion, and publishes the existing `Local\MomoMarkerObservationsV1` contract.

The Y-plane mapping is a latest-frame transport: three buffers, four fixed 960x528 planes,
per-source receive timestamp/sequence/valid flag, and a batch seqlock. A slow detector drops
old frames rather than accumulating latency. It is independent from the BGRA mapping used
for MADSYSTEM visuals.

GPU stage profiling defaults to `-ProfilingMode sampled`: detection, cycle time, drops, and
source age remain frame-rate data, while CUDA Events and stage-specific synchronization run
once per second. Use `off` to disable stage profiling, or `full` only for bounded performance
comparisons that require per-frame stage percentiles.

With `-ObserverVisualOutput legacy`, `-ObserverHeadless` suppresses only the duplicate Native
Observer window draw. WebRTC receive/decode, BGRA visual publication, Y-plane publication,
and MADSYSTEM display remain active. Use it when the same GPU runs the detector or when the
Native preview is redundant. Use `-ObserverVisualOutput off`, not `-ObserverHeadless` alone,
to remove the BGRA visual path. On the 2026-08-14 validation PC, Native decode used a
different GPU LUID from CUDA detection, so headless mode did not remove the detector
bottleneck by itself.

### GPU placement observed on the validation PC

The Windows GPU numbers shown by Task Manager are machine-specific. Identify the adapter by
name and LUID instead of assuming that GPU 0 or GPU 1 has a fixed role. The 2026-08-14 PC used:

- Native Observer: Intel UHD Graphics 630, LUID `0x11BB9`, including oneVPL video decode
- GPU Marker Worker: CUDA device 0, NVIDIA GeForce RTX 3060, LUID `0x10724`
- MADSYSTEM rendering: NVIDIA GeForce RTX 3060, LUID `0x10724`

This already assigns decode and detection to different adapters. It is not a zero-copy path:
Native Observer publishes host-visible Y planes and the Python worker calls `cp.asarray` to
copy each selected batch to the RTX. The cross-adapter host copy, Python scheduling, and
MADSYSTEM rendering must therefore be included in capacity measurements.

### 2026-08-14 live result

The first two-source checkpoint was:

- live inputs present: `11.4`, `11.5`; `11.3` and `11.6` remained configured but invalid
- detected IDs: `11.4=[1,1]`, `11.5=[2,2,2]`
- 15-second Y-plane run without MADSYSTEM: 44.57 Hz, processing p95 25.359 ms
- full stack with MADSYSTEM External: approximately 39-42 Hz, processing p95 30-35 ms
- transitional BGRA full-stack path observed before this change: approximately 21-24 Hz

After a clean reboot, all four live inputs reached 50 FPS and the complete stack was measured:

- Relay + Race Control + Native Observer + MADSYSTEM External + GPU Marker Worker were active
- Native Observer used about 2.61 CPU cores and 713 MB
- MADSYSTEM used about 1.95 CPU cores and 1.45 GB
- GPU Marker Worker used about 1.04 CPU cores and 296 MB
- Relay used about 0.59 CPU cores and 71 MB
- the measured processes used 6.24 of 12 logical CPUs, approximately 52% of the machine
- RTX utilization averaged 30.3%, peaked at 41%, and used approximately 2.9 GB VRAM
- live four-source publication stabilized around 17.5 Hz with processing p95 around 89 ms

An isolated, warmed four-source replay previously reached 49.55 Hz with processing p95
19.92 ms. Repeating the same replay while the full live stack was active reached 17.716 Hz,
processing p95 79.022 ms, and detected 3,160 marker instances. This difference shows that the
full-stack host transfer and scheduling path, rather than RTX compute capacity alone, is the
current bottleneck.

MADSYSTEM connected to `Local\MomoMarkerObservationsV1` in External mode without a dropped-batch
warning. The replay produced an external ID 2 overlay in the MADSYSTEM view. After returning to
the live worker, `11.4` produced duplicate-preserving ID 1 observations and `11.5` produced ID 8
observations while all four Relay inputs remained at 50 FPS. This verifies detection and display
integration. A real race with checkpoint order `1 -> 2 -> 3`, lap advancement, PIT ID 8, and Race
Control snapshots remains the acceptance test for timing semantics.

The direct Y-plane path removes the BGRA-to-gray copy and substantially improves the live
rate, but this PC does not sustain 50 Hz for the two active real feeds. Keep 50 FPS input,
but treat 50 Hz detection as a remaining optimization/capacity item rather than a completed
guarantee. Initial MADSYSTEM scene loading can also overflow the 16-batch observation ring;
start MADSYSTEM before the worker or increase consumer startup readiness before production.

## Repeating the live test on another PC

Use the same branch in both repositories, keep generated environments and reports outside Git,
and record adapter names rather than Task Manager GPU numbers.

1. Build the feature Native Observer and the MADSYSTEM External consumer from their matching
   `codex/marker-observer-legacy-adapter` branches.
2. Initialize the GPU environment with `Initialize-ArucoCapacity.ps1 -IncludeNvCodec`.
3. Start Race Control and Relay, then confirm each active source reaches its configured input FPS.
4. Start Native Observer with `-ObserverExecutable <feature-momo.exe> -ObserverHeadless`.
5. Start MADSYSTEM with `MADSYSTEM_MARKER_OBSERVER_MODE=external` and wait for its scene to settle.
6. Start the live worker for a bounded run so that it writes a JSON report:

```powershell
.\tools\Run-GpuMarkerObserverLuma.ps1 `
  -DetectionHz 50 `
  -DurationSeconds 60 `
  -ProfilingMode full `
  -PythonExecutable .\tools\.artifacts\aruco-venv-gpu-313\Scripts\python.exe `
  -OutputPath .\tools\.artifacts\marker-observer\live-four-source.json
```

Record per-process CPU and working set, input FPS, publication rate, processing p95/p99, GPU
engine utilization, VRAM, invalid-source intervals, and MADSYSTEM dropped batches. A 50 Hz target
passes only when the bounded report reaches at least 47.5 Hz and the full race-path acceptance
test passes. Do not infer node capacity from an isolated detector replay.

## Transitional BGRA producer

Start Native Observer with its fixed four slots and shared-frame output, then run:

```powershell
.\tools\Run-GpuMarkerObserverSharedFrame.ps1 `
  -Sources @('11.3=0', '11.4=1', '11.5=2', '11.6=3') `
  -DurationSeconds 30 `
  -PythonExecutable .\tools\.artifacts\aruco-venv-gpu-313\Scripts\python.exe `
  -OutputPath .\tools\.artifacts\marker-observer\live.json
```

The integer after `=` is both the 2x2 Native Observer slot and the MADSYSTEM source index.
For a single-source Native Observer, use slot zero only for a source that MADSYSTEM also
maps to index zero. Keep all four fixed slots when validating `11.5` as source index two.

The transitional producer reads only stable, completed shared frames, skips duplicate composite frames,
marks the green Native Observer placeholder as invalid video, batches all valid slots on the
GPU, and publishes the same `Local\MomoMarkerObservationsV1` contract as replay.

## IPC contract

- mapping: `Local\MomoMarkerObservationsV1`
- mutex: `Local\MomoMarkerObservationsV1-Mutex`
- producer lease: `Local\MomoMarkerObservationsV1-Producer`; a second producer is rejected
- magic/version: `MMO1` / 1
- bounded ring: 16 batches
- maximum sources: 32
- maximum detections per source and frame: 16
- source observation: source ID/index, source and batch sequence, frame/detection timestamps,
  video-valid flag, candidate count, repeated marker IDs, normalized center and area

The consumer reads batches in sequence. If it falls behind the 16-slot ring, it resumes at
the oldest retained batch and reports the number dropped. Producer PID/creation changes or
sequence rollback reset the reader cursor, so restarting the producer does not strand the
consumer on an old sequence. Marker observations are tiny and ordered; video remains a
latest-frame path.

## MADSYSTEM modes

MADSYSTEM defaults to `Internal`, so deployment of the new DLL alone does not change race
behavior. Set `MADSYSTEM_MARKER_OBSERVER_MODE` before starting MADSYSTEM:

| Value | Behavior |
| --- | --- |
| `internal` | Current OpenCV recognition and timing behavior |
| `shadow` | Current recognition remains authoritative; external per-source ID counts are compared once per second on mismatch |
| `external` | GPU observations replace the internal detection result, then enter the existing MADSYSTEM checkpoint/PIT/assignment path |

An invalid external video source clears its pending checkpoint evidence instead of treating
video loss as marker exit. A valid frame with no marker continues through the existing exit
confirmation logic.

## Promotion order

1. Run replay producer plus native smoke reader and confirm 50 Hz with zero drops.
2. Run MADSYSTEM in `shadow` with the same four live feeds and record mismatch plus timing
   logs without changing race results.
3. Compare CPU/GPU marker instances, passage outcomes, PIT presence, and video timestamps.
4. Run `external` in a non-race test and verify checkpoint order, duplicate suppression,
   PIT IN/OUT, pilot assignment, and Race Control snapshots.
5. Compare the transitional BGRA path with the Native Y-plane path, then retire BGRA marker
   input after parity.
6. Only after parity, move temporal qualification to Marker Observer and define a Reliable
   marker-event ingest contract. Do not combine this with the live-input migration.

The USB/Webcam composite replacement is a separate, later path and is not part of this
FPVRC/WebRTC sequence.
