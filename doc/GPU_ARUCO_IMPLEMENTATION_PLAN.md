# GPU ArUco Implementation Plan

## Status

- Phase 1 capacity backend: implemented
- Phase 1 frame parity tool: implemented
- Phase 1 12-source / 10-minute soak: passed
- Phase 1 one-hour and WebRTC end-to-end validation: pending
- Production Marker Observer integration: not started
- GPU-resident candidate extraction: not started
- Full GPU marker detection: research stage

## Goal

Keep H.264 decode and image preprocessing on the GPU long enough to reduce CPU cost per
50 Hz source, without changing marker IDs, passage timing, or the reliable event contract.
The production target is not merely a faster recording benchmark. It is a Marker Observer
node that consumes Relay video, emits the same accepted marker observations as the CPU
reference, and does not share a failure boundary with RC control.

## Phase 1: direct NVDEC with CPU ArUco

The `nvcodec` capacity backend uses NVIDIA PyNvVideoCodec in the measurement process.
Each source owns an NVDEC session. NV12 is copied to host memory, only the Y plane is
resized, and the existing OpenCV `DICT_4X4_50` detector remains the reference detector.

```text
H.264 file
  -> PyNvVideoCodec / NVDEC
  -> host NV12
  -> Y plane only
  -> resize to recognition quality
  -> CPU cv::aruco
```

This removes one FFmpeg subprocess, rawvideo pipe, gray conversion, and process boundary
per source. It deliberately does not claim GPU ArUco detection. It establishes a native
NVDEC session and a frame boundary that Phase 2 can keep in device memory.

Initialize the optional backend on an NVIDIA Windows node:

```powershell
.\tools\Initialize-ArucoCapacity.ps1 -IncludeNvCodec
```

For a clean installation on another PC, including RTX 3060 capacity discovery and license
boundaries, follow [Direct NVDEC ArUco Validation Guide](DIRECT_NVDEC_ARUCO_VALIDATION_GUIDE.md).

Run the 50 Hz comparison:

```powershell
.\tools\Measure-ArucoCapacity.ps1 `
  -InputPath .\tools\.artifacts\aruco-input\cpu-shadow-20260731T122739445Z-732b1f8f-upright-h264.mp4 `
  -SourceCounts 4,8,10,12,14,16,17 `
  -DurationSeconds 30 -DetectionHz 50 -Decoder nvcodec
```

Compare CPU and direct NVDEC detection on identical frame indices before moving any
preprocessing stage to the GPU:

```powershell
.\tools\Compare-ArucoBackends.ps1 `
  -InputPath .\tools\.artifacts\aruco-input\cpu-shadow-20260731T122739445Z-732b1f8f-upright-h264.mp4 `
  -FrameCount 1500
```

The Phase 1 decoder parity gate requires 99.0% frame-set agreement for expected course
marker IDs and also records detection groups separated by more than 10 frames. Small
boundary differences between software and hardware H.264 decoders are expected. Qualified
group counts must match after isolated groups with fewer than 3 detections are removed.
Phase 2
algorithm parity must compare CPU and GPU preprocessing from the same NVDEC luma frame and
use a stricter gate. Unknown IDs are still reported and must be reviewed, but are not
allowed to become timing events without the course allowlist.

`PyNvVideoCodec` and the pip CUDA runtime are optional dependencies. CPU-only nodes keep
using `opencv`; the existing FFmpeg `cuda` backend remains a comparison path.

## Phase 1 acceptance

- Input and detection rate remain at or above 47.5 Hz for a 50 Hz profile.
- Detection latency p95 remains at or below 20 ms.
- Process CPU p95 remains at or below 60%.
- Marker IDs 1, 2, and 3 are compared against the CPU reference on the same source frames.
- Unknown IDs remain diagnostic data and never become race events without the course
  allowlist and passage-state checks.
- A selected operating count passes 10 minutes, then one hour, including input loop or
  reconnect boundaries.

## Phase 1 result on MADX-DESKTOP

The test node is a Ryzen 7 9700X, RTX 5070 12GB, and 64GB RAM Windows PC. With the
normalized 960x528 50 FPS H.264 recording:

- The 30-second capacity boundary was 16 sources at 49.69 Hz, 9.04ms detection p95, and
  59.62% CPU p95. 17 sources failed only the 60% CPU gate at 62.19%.
- The operating candidate of 12 sources passed 600 seconds at minimum 49.988 Hz,
  7.971ms detection p95, 44.531% CPU p95, and 466.352MB maximum working set.
- The 600-second run crossed the 172-second input boundary multiple times with no worker
  errors or read errors.
- A 1500-frame CPU/NVDEC comparison passed at 99.333% expected-marker frame agreement.
  IDs 1, 2, and 3 had identical total detection counts and identical qualified group
  counts; isolated single-frame differences remained diagnostic only.

Keep 12 sources as the provisional direct-NVDEC profile. Do not declare it the production
limit until the one-hour soak and Relay/WebRTC-to-marker-event end-to-end test pass.

## Phase 2: GPU-resident preprocessing

Switch PyNvVideoCodec to device-memory output and retain the decoder CUDA stream. Perform
crop, resize, luma normalization, adaptive thresholding, and quadrilateral candidate
extraction on the GPU. Transfer candidate corners and small warped marker patches to the
CPU; keep dictionary decoding and error correction in the reference implementation.

```text
NVDEC device NV12
  -> CUDA Y-plane view
  -> CUDA resize / threshold / candidate extraction
  -> candidate corners and small patches only
  -> CPU dictionary decode and refine
```

This phase is the preferred route to meaningful GPU value. Merely compiling OpenCV with
CUDA does not move `cv::aruco::ArucoDetector` to the GPU because OpenCV has no CUDA ArUco
implementation.

## Phase 3: full GPU detector

Only start this phase if Phase 2 still cannot meet the target node count. Candidate options
are a custom CUDA implementation or a GPU model that proposes marker quads followed by a
deterministic dictionary decoder. A learned model must not directly become the timing
authority without frame-by-frame parity, false-positive, occlusion, blur, and exposure
tests.

Changing from ArUco to AprilTag is a protocol and physical-course migration, not a drop-in
optimization. NVIDIA VPI AprilTag support currently targets CPU/PVA rather than CUDA on
this Windows RTX node, so it is not the Phase 2 implementation path.

## Production integration boundary

The recording backend is a capacity probe. The production Marker Observer still needs:

1. Relay read-only source assignment and reconnect handling.
2. RTP depacketization and H.264 access-unit/keyframe handling before NVDEC.
3. Per-source latest-frame queues with no unbounded backlog.
4. Course marker allowlists and passage debounce/state machines.
5. Reliable marker-event ingest with source, run, sequence, and event idempotency.
6. CPU fallback when NVDEC initialization or the GPU pipeline fails.

Do not put GPU detection inside the Relay process. Decoder or detector overload must not
delay Pilot control, telemetry forwarding, or reliable race events.
