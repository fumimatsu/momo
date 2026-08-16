# GPU ArUco Implementation Plan

## Status

- Phase 1 capacity backend: implemented
- Phase 1 frame parity tool: implemented
- Phase 1 12-source / 10-minute soak: passed
- Phase 1 one-hour and WebRTC end-to-end validation: pending
- Production Marker Observer integration: not started
- GPU-resident dictionary decode PoC: implemented and validated
- GPU-resident candidate extraction PoC: implemented and validated for one source
- Full GPU marker detection: PoC passed; multi-source capacity and production integration pending

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

## Phase 2 PoC result on MADX-DESKTOP

The first PoC goes beyond the original Phase 2 boundary and also performs deterministic
dictionary decoding on the GPU:

```text
NVDEC device NV12
  -> DLPack / CuPy device view
  -> CUDA luma resize and adaptive threshold
  -> CUDA union-find connected components
  -> GPU quad extrema and geometry filters
  -> CUDA perspective warp / Otsu / border validation
  -> CUDA DICT_4X4_50 Hamming decode
  -> marker IDs only to host
```

`tools/GpuArucoId.py` contains the resize and dictionary decoder. The decoder follows the
OpenCV 4.10 canonical 24x24 marker extraction: nearest-neighbor warp, 4 pixels per cell,
one border cell, Otsu threshold, maximum five white border bits, and zero corrected bits
for `maxCorrectionBits=1` with `errorCorrectionRate=0.6`.

`tools/GpuArucoDetector.py` adds adaptive thresholding, connected components, eight-direction
quad extrema, and geometry filtering without copying image pixels to the CPU. It preserves
multiple physical markers carrying the same ID instead of reducing them to frame presence.
The course
allowlist is part of the detector input. Unknown dictionary IDs are never accepted merely
because their 4x4 code matches background texture.

Validate dictionary decode against CPU-provided candidate corners:

```powershell
.\tools\Validate-GpuArucoId.ps1 `
  -InputPath .\tools\.artifacts\aruco-input\cpu-shadow-20260731T122739445Z-732b1f8f-upright-h264.mp4 `
  -FrameCount 1500 -ExpectedMarkerIds 1,2,3
```

Validate the GPU-only candidate and ID path. OpenCV receives a host image only as the
validation oracle; the measured GPU path does not:

```powershell
.\tools\Validate-GpuArucoDetector.ps1 `
  -InputPath .\tools\.artifacts\aruco-input\cpu-shadow-20260731T122739445Z-732b1f8f-upright-h264.mp4 `
  -FrameCount 1500 -ExpectedMarkerIds 1,2,3
```

On the normalized 960x528 / 50 FPS recording at recognition quality 0.6:

- GPU dictionary decode matched all 1,253 CPU candidates for allowlisted IDs 1, 2, and 3.
  Fourteen CPU detections for non-course IDs 17 and 37 remained diagnostic and were not
  used to weaken the GPU decoder or create race events.
- GPU-only candidate extraction and ID decode achieved 99.113% precision and 96.336%
  recall for per-frame ID presence against expected IDs 1, 2, and 3 over 1,500 frames.
- After physical marker multiplicity became part of the contract, instance precision was
  99.146% but instance recall was 92.658%. CPU counted 1,253 expected marker instances and
  GPU counted 1,171. This fails the required 95% instance recall and supersedes the earlier
  presence-only PASS classification.
- GPU kernel processing p95 was 6.305ms and GPU-path wall-time p95 was 6.359ms, including
  device resize, candidate extraction, and ID decode. Image transfer for the CPU oracle
  was outside the GPU path and is reported separately.
- The acceptance gate now applies precision >=98% and recall >=95% to both per-frame ID
  presence and physical marker instances, plus GPU p95 <20ms and GPU-path wall-time <20ms.

This proves that ID-only detection can remain on the GPU for one source, but the current
candidate geometry is not yet CPU-equivalent when several physical markers share an ID.
It also does not prove the 10- or 20-source operating count. The current Python/CuPy PoC
allocates and clears
large connected-component work arrays per frame and synchronizes when returning IDs.
Before production integration, reuse per-resolution workspaces and measure multiple CUDA
streams or batches under the same 50 Hz real-time scheduler used by the capacity suite.

### CPU versus GPU capacity result

`Compare-CpuGpuArucoCapacity.ps1` runs CPU OpenCV (`nvcodec`) and the full GPU detector
(`nvcodec-gpu`) against the same video, source counts, duration, quality, and 50 Hz target.
It reports physical marker instances separately from frame presence.

On MADX-DESKTOP, the 30-second boundary test produced:

| Sources | CPU FPS | GPU FPS | CPU processing p95 | GPU processing p95 | CPU p95 | GPU-path CPU p95 | Result |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 1 | 49.957 | 49.921 | 2.828ms | 4.265ms | 5.675% | 1.894% | both pass |
| 4 | 49.924 | 49.110 | 7.874ms | 23.541ms | 16.462% | 9.275% | CPU pass, GPU latency fail |

A five-second sweep kept the CPU path above 48 Hz through 16 sources. The GPU path passed
one source, narrowly maintained rate at four sources but exceeded 20ms p95, and fell to
27.22 Hz at eight sources. GPU utilization remained at or below 20%, showing a scheduler
and per-frame synchronization limit rather than exhausted RTX 5070 compute. A trial with
one non-blocking CUDA stream per source made throughput worse and was removed.

The shared micro-batch capacity backend is now implemented as `nvcodec-gpu-batch`:

1. Decoder workers publish only their latest device frame into bounded one-slot mailboxes.
2. One GPU scheduler gathers one current frame from every configured source per 50 Hz tick.
3. Threshold, connected components, and quad extraction use a CUDA batch dimension.
   Resize currently launches once per source and the batch array is assembled on-device.
4. Results return as `(sourceId, frameSequence, markerIds[])`, preserving duplicate IDs.
5. Stale frame results are discarded; no detector backlog can delay Relay or RC control.
6. CPU and GPU reports use detector-active time for FPS; NVDEC destruction after the run is
   reported in overall elapsed time but cannot lower measured detection FPS.

The MADX-DESKTOP 30-second boundary comparison measured:

| Sources | CPU FPS | CPU p95 | CPU usage p95 | GPU batch FPS | GPU batch p95 | GPU-path CPU p95 |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: |
| 20 | 49.09 | 13.79ms | 74.94% | 49.73 | 15.41ms | 7.72% |
| 24 | 49.27 | 14.41ms | 86.62% | 49.83 | 19.49ms | 7.76% |
| 28 | 47.48 | 28.95ms | 88.01% | 49.79 | 19.17ms | 8.16% |

At the 47.5 Hz and 20ms gates, CPU passed through 24 sources and failed at 28. GPU batch
passed 28 sources. This satisfies the capacity PoC objective: the GPU path exceeds the CPU
simultaneous-source boundary while leaving substantially more CPU headroom. It does not set
the production operating count. Twenty-four sources is the first soak candidate; 28 is a
measured boundary with too little latency margin for immediate operation.

The first 24-source stability run passed for 60 seconds at 49.833 Hz, 17.976ms processing
p95, 7.394% process CPU p95, 41% GPU p95, 23% NVDEC p95, and 3,568MB maximum GPU memory.
This is a pre-soak check, not a substitute for the required 10-minute and one-hour runs.

### 1920x1080 / 60 FPS validation

The `20260323.mp4` validation source is H.264 Main, progressive 1920x1080, 60 FPS, and
113.9 seconds. At recognition quality 0.6 the detector input is approximately 1152x648.
The acceptance gate is 57 Hz and 16.67ms processing p95.

Thirty-second capacity results were:

| Backend | Sources | Detection FPS | Processing p95 | Process CPU p95 | Result |
| --- | ---: | ---: | ---: | ---: | --- |
| CPU OpenCV | 3 | 59.87 | 14.21ms | 29.20% | pass |
| CPU OpenCV | 4 | 59.45 | 16.91ms | 36.72% | fail |
| GPU batch | 8 | 59.73 | 11.99ms | 4.73% | pass |
| GPU batch | 12 | 59.87 | 16.52ms | 8.16% | pass |
| GPU batch | 16 | 59.82 | 17.83ms | 10.41% | fail |

The GPU batch path therefore increases the measured 60 Hz boundary from three to twelve
sources. Sixteen sources is close enough that workspace reuse, batched resize, and removal
of the on-device `cp.stack` copy are credible optimization targets.

### 2x2 composite / WebcamMulti replacement check

A 30-second 1920x1080 / 60 FPS composite was generated by scaling the same FPV source to
960x540 and placing it in all four quadrants. The validation path used one NVDEC decoder,
split the luma frame into four quadrants on the GPU, stacked the quadrants as one batch,
and ran four independent marker detections in that batch.

| Metric | Result |
| --- | ---: |
| Composite frames | 1,800 |
| GPU processing p95 | 6.638ms |
| Decode-to-result wall time p95 | 7.221ms |
| Decode-to-result wall time p99 | 9.736ms |
| Frames over 16.67ms | 4 / 1,800 (0.222%) |
| Processing throughput | 177.397 composite FPS |

Each quadrant found one ID 1 appearance group over frames 319 through 433, matching the
independent CPU quadrant oracle in all four quadrants. Frame and physical-instance counts
were close but not identical, so the result proves passage-event parity for this clip, not
general frame-level detector parity.

Current MADSYSTEM `ArUcoWebCamMulti.ProcessFrameSinglePass` does not run four independent
detectors. It resizes the complete composite frame, calls `detectMarkers` once, then uses
`MapDetectionsToQuadrants` to assign each marker by center position. The GPU experiment is
therefore an alternative internal implementation, not a line-for-line port. It can replace
the same output responsibility by returning bounded per-quadrant ID lists while retaining
MADSYSTEM's allowlist, temporal confirmation, checkpoint ordering, pit observation, and
pilot-assignment behavior above the detector boundary.

This result supports replacing the WebcamMulti image-processing stage with an isolated GPU
Marker Observer. Production readiness still requires four different simultaneous FPV
feeds, live composite or per-stream input, latest-frame dropping, reconnect handling, and
end-to-end passage-event verification. The four over-budget frames also mean an unbounded
frame queue is unacceptable even though the p95 gate passed.

Full-video ID 1 comparison at quality 0.6 found 192 CPU-positive frames and 152 GPU-positive
frames. Presence precision/recall was 94.737% / 75.000%; physical-instance precision/recall
was 93.533% / 65.113%. This frame-level result is below the CPU parity target. However, with
a ten-frame grouping gap, CPU produced five marker-appearance groups and GPU detected all
five with no extra independent group. That supports passage-event experimentation but does
not establish general marker accuracy.

Increasing the adaptive window from 13 to 31 improved presence recall to 84.375% and
instance recall to 81.833%, while reducing precision to 89.503% / 84.692%. Quality 0.5
allowed 16 sources at 58.81 Hz / 15.87ms and improved recall, but precision remained below
90%. Neither is a production default. The preferred next accuracy work is selective
multi-window confirmation around plausible candidates, followed by passage-event parity,
rather than globally weakening the detector.

The next optimization and integration work is:

1. Reuse preallocated per-resolution connected-component and extrema workspaces.
2. Add a pointer-table or batched resize kernel to remove per-source resize launches and
   `cp.stack` copies.
3. Replace recording decoders with bounded latest-frame mailboxes fed by Relay/RTP sources.
4. Add dropped/stale frame counters and preserve source sequence through result delivery.
5. Run 24 sources for 10 minutes and one hour, then repeat on RTX 3060 without transferring
   this machine's source limit.

### Marker ID and large-component checks

The dictionary table contains and compares all `DICT_4X4_50` IDs from 0 through 49. A
synthetic 48px marker test detected all 50 IDs, including ID 16. This verifies code-table
coverage only; real video still requires per-ID blur, distance, angle, and exposure tests.

The connected-component ceiling is a PoC-specific GPU filter, not an existing OpenCV
ArUco area limit. It counts thresholded pixels in one connected component rather than the
geometric screen area occupied by the marker. The default remains 10%.

On the 1,500-frame recording, changing the ceiling from 10% to 20% produced no detection
change:

| Profile | Limit | Candidate p95 | Precision | Recall | GPU ID 17 frames |
| --- | ---: | ---: | ---: | ---: | ---: |
| Course IDs 1,2,3; window 13 | 10% | 22 | 99.113% | 96.336% | blocked |
| Course IDs 1,2,3; window 13 | 20% | 22 | 99.113% | 96.336% | blocked |
| All IDs diagnostic; window 13 | 10% | 22 | 90.303% | 93.515% | 40 |
| All IDs diagnostic; window 13 | 20% | 22 | 90.303% | 93.515% | 40 |

Large-marker recognition depends on both the adaptive-threshold window and the component
ceiling. For a centered synthetic ID 16 on a 256x256 image, window 13 detected only 48px
and 64px with either ceiling. Window 31 detected through 88px with the 10% ceiling and
through 144px with the 20% ceiling. On the real recording, window 31 slightly improved
course-ID recall to 96.552% and reduced diagnostic ID 17 frames from 40 to 34. This is not
enough evidence to replace the default: retain window 13 and the 10% ceiling until close,
blurred real-marker recordings pass on both RTX 5070 and RTX 3060.

ID 17 is unsuitable as a timing marker in the current corpus. Both CPU OpenCV and the GPU
PoC report ID 17 even though it is not installed. Keep it outside the course allowlist and
reserve it unless a broader negative and positive recording set demonstrates acceptable
false-positive behavior. Allowlisting and passage debounce are complementary; debounce
alone is insufficient because the false ID can persist across several frames.

The full 8,600-frame reference recording contains only installed IDs 1, 2, and 3. A
diagnostic scan with all 50 IDs enabled produced the following non-course ID results:

| Detector | ID 17 | ID 34 | ID 37 | Other one-off or two-frame IDs |
| --- | ---: | ---: | ---: | --- |
| OpenCV CPU | 78 | 4 | 19 | 15, 23, 28, 40 |
| GPU PoC | 202-206 | 2 | 8 | 22, 28, 30, 36, 42 |

Visual inspection found no physical non-course marker at these candidate positions. Most
were narrow high-contrast structures on walls, equipment, or the vehicle. The single GPU
ID 30 result appears to be a distant installed marker misdecoded as another ID. ID 17 had
the strongest and most persistent false-positive tendency: up to two consecutive frames
on CPU and nine on the GPU PoC. ID 17 is therefore a reserved/excluded ID for this course.

ID 34 is also an avoid-list candidate based on the user's prior field observation and six
combined false detections in this recording. ID 37 produced more false detections than ID
34 in this sample and must be evaluated at the same priority. Do not globally retire every
single-frame ID from one recording; keep the production rule as a per-course allowlist and
maintain 34 and 37 as provisional exclusions until additional negative and positive clips
are measured.

The operational decision is now fixed: IDs 17, 34, and 37 are reserved and must not be
assigned to course, pit, bonus, or pilot-assignment markers. Other IDs remain available;
isolated false detections are handled by MADSYSTEM's configured marker allowlist and a
per-ID temporal evidence gate rather than by extending a global denylist.

Current MADSYSTEM behavior only partially satisfies that policy. `ArUcoWebCamMulti`
already ignores IDs outside `CheckPointNo` and `BonusCheckPointNo`. However, its current
`RecogCount` queue counts configured-ID observations and later returns the first queued ID
after a recognition gap. This behavior is intentional for the current sparse checkpoint
layout: an unconfigured false ID such as 30 is ignored and must not reset evidence for the
real marker. With checkpoints far apart, two valid IDs are not expected to overlap.

Do not change this to an unconditional reset whenever any different ID appears. When
mini-sector markers make valid IDs closer together, replace the shared queue with per-ID
candidate state for each vehicle. Each configured ID keeps its own observation count,
last-seen time, bounded missing-frame allowance, armed/emitted state, and cooldown. Unknown
or reserved IDs are diagnostic only and do not alter candidates. A different configured ID
is tracked independently; course order and physically plausible transition rules decide
which armed candidate becomes a timing event. This preserves current noise tolerance while
preventing observations from multiple valid mini-sector IDs from being mixed in one queue.

The existing standard checkpoint path also ignores IDs 11 and above, but that is not a
complete reservation mechanism: `BonusCheckPointNo` is evaluated first, and pilot auto
assignment currently accepts payload marker IDs 5 through 49. MADSYSTEM configuration
validation must therefore reject 17, 34, and 37 for every operational marker role rather
than relying on the standard-checkpoint range check.

ID 16 had zero false detections in the full negative recording and passed synthetic
positive detection. It is a viable test candidate, not yet a production-qualified marker:
a real printed ID 16 must still pass distance, angle, blur, exposure, and close-range tests.

The GPU ID 17 count changed from 202 to 206 across repeated full-video scans. This exposes
non-deterministic convergence in the current parallel union-find PoC. Production work must
make connected-component labeling deterministic and add a repeated-run equality test
before marker suitability numbers are treated as final.

The limits can be reproduced without changing code:

```powershell
.\tools\Validate-GpuArucoDetector.ps1 `
  -InputPath .\tools\.artifacts\aruco-input\cpu-shadow-20260731T122739445Z-732b1f8f-upright-h264.mp4 `
  -FrameCount 1500 -ExpectedMarkerIds 1,2,3 `
  -AdaptiveWindowSize 31 -MaximumComponentAreaRatio 0.2
```

## Phase 3: production full GPU detector

The custom CUDA PoC establishes the algorithm boundary. Production work still requires:

1. Reusable GPU workspaces instead of per-frame large-array initialization.
2. Multi-source scheduling and a measured 50 Hz capacity boundary on RTX 5070 and RTX 3060.
3. More candidate-recall recordings covering blur, exposure, oblique markers, and partial
   occlusion; add another threshold window only when evidence requires it.
4. Course allowlist, repeated-frame passage confirmation, cooldown, and event idempotency.
5. Native integration or an isolated detector process with bounded latest-frame queues.

A learned model remains an optional quad proposer only. It must not directly become the
timing authority without frame-by-frame parity and false-positive testing.

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
