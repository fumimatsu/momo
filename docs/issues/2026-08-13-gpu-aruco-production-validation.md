# GPU ArUco production validation

Status: open

## Context

The Python/CuPy PoC keeps NVDEC luma, candidate extraction, perspective normalization,
and `DICT_4X4_50` decoding on the GPU. A 1,500-frame recording passed the PoC thresholds,
but this is a one-source algorithm validation rather than a production capacity result.

## Goal

Turn the GPU-only ID detector into an isolated Marker Observer path that has a measured
10- to 20-source boundary without sharing Relay or RC-control failure domains.

## Acceptance Criteria

- Reuse per-resolution connected-component workspaces instead of initializing large
  arrays for every frame.
- Measure 50 Hz detection with multiple CUDA streams or batches on RTX 5070 and RTX 3060.
- Keep precision at or above 98% and recall at or above 95% on the current reference
  recording for both per-frame ID presence and physical marker instances, then add blur,
  exposure, oblique-angle, and partial-occlusion recordings.
- Preserve repeated IDs when multiple physical markers with the same ID are visible. A
  presence-only result is insufficient for CPU-equivalent validation.
- The shared micro-batch capacity scheduler is implemented and exceeds the CPU boundary.
  Reusable workspaces and batched resize remain required before production integration.
  Per-source non-blocking CUDA streams were measured and were slower.
- Require a course/run marker allowlist plus repeated-frame passage confirmation and
  cooldown before emitting a reliable marker event.
- Keep a measured marker-ID suitability list. ID 17 has repeated false detections in the
  current recording despite not being installed and should remain reserved unless a wider
  corpus proves it reliable.
- Make GPU connected-component output deterministic. Repeated full-video scans changed
  GPU ID 17 false-detection counts from 202 to 206 with identical input and settings.
- Enforce IDs 17, 34, and 37 as reserved IDs in course and marker-assignment configuration.
- Apply the reservation to checkpoint, bonus/pit, and pilot auto-assignment validation;
  the current pilot pair decoder otherwise accepts marker IDs 5 through 49.
- Keep the current sparse-checkpoint queue until mini-sector markers are introduced. Do
  not reset a valid candidate merely because an unconfigured false ID appears.
- Before mini-sector rollout, replace the shared MADSYSTEM checkpoint queue with per-ID
  temporal candidate state, bounded missing-frame tolerance, one-shot emission/cooldown,
  and course-order validation.
- Demonstrate bounded latest-frame queues and fallback behavior under decoder or detector
  overload.
- Replace the 2x2 WebcamMulti image-processing boundary with one NVDEC plus four-quadrant
  GPU batch detection while preserving per-quadrant marker observations.
- Repeat the 2x2 test with four different live FPV feeds; a duplicated recording validates
  scheduling and quadrant isolation but not independent-stream decode or reconnect behavior.

## Verification

- `Validate-GpuArucoId.ps1` passes 1,500 frames.
- `Validate-GpuArucoDetector.ps1` passes 1,500 frames for the configured course IDs.
- A multi-source 10-minute test passes at the selected operating count, followed by a
  one-hour soak and Relay/WebRTC-to-marker-event E2E validation.

## Notes

The instance-aware 2026-08-14 MADX-DESKTOP validation measured 99.113% presence precision,
96.336% presence recall, 99.146% instance precision, and 92.658% instance recall. It is now
FAIL because instance recall is below 95%. GPU processing p95 was 6.305ms and GPU-path
wall-time p95 was 6.359ms. Fourteen CPU candidates
reported non-course IDs 17 or 37; they remain diagnostic and do not weaken geometry,
dictionary validation, or the course allowlist.

The 30-second CPU/GPU capacity comparison passed both paths at one source. At four sources,
CPU sustained 49.924 Hz with 7.874ms processing p95; GPU sustained 49.110 Hz but failed the
20ms interval gate at 23.541ms p95. The five-second GPU sweep fell to 27.22 Hz at eight
sources while GPU utilization remained below 20%. The next milestone is batched kernels,
not production integration of the current per-source detector.

The subsequent batch implementation changed the capacity result. In a 30-second test CPU
passed 24 sources at 49.27 Hz / 14.41ms p95 but used 86.62% CPU, then failed 28 sources at
47.48 Hz / 28.95ms. GPU batch passed 28 sources at 49.79 Hz / 19.17ms and 8.16% CPU. The
capacity objective is therefore achieved. Keep the issue open for the 24-source soak,
workspace reuse, live Relay/RTP input, and end-to-end marker-event validation.

The 24-source pre-soak passed for 60 seconds at 49.833 Hz and 17.976ms processing p95.
Process CPU p95 was 7.394%, GPU p95 41%, NVDEC p95 23%, and maximum GPU memory 3,568MB.

The 1920x1080 / 60 FPS validation established a three-source CPU boundary and a twelve-source
GPU batch boundary at the 57 Hz / 16.67ms gates. GPU batch passed 12 sources at 59.87 Hz and
16.52ms p95; 16 sources sustained 59.82 Hz but failed at 17.83ms p95. Full-video ID 1
frame-level recall was 75%, but all five CPU marker-appearance groups were also observed by
GPU with no extra group. Add passage-event parity to the acceptance suite before changing
the detector thresholds.

Increasing the connected-component ceiling from 10% to 20% did not change any detection
or candidate-count result in the reference recording. A larger adaptive-threshold window,
combined with the 20% ceiling, extended synthetic ID 16 detection from 64px to 144px on a
256x256 image. Window 31 with the 10% ceiling stopped at 88px. Window 31 remains a
validation profile rather than the default until real close-marker recordings and
multi-source capacity tests pass.

The complete 8,600-frame negative scan contained only installed IDs 1, 2, and 3. CPU
OpenCV nevertheless reported ID 17 on 78 frames, ID 37 on 19, and ID 34 on 4. The GPU PoC
reported ID 17 on 202-206 frames, ID 37 on 8, and ID 34 on 2. Visual samples showed false
quads on background or vehicle features rather than physical non-course markers. ID 16
had zero false detections and passed the synthetic positive suite, but still needs a real
printed-marker positive recording.

Policy decision: 17, 34, and 37 are formal exclusions. Other marker IDs are not added to a
global denylist based on isolated detections. They remain subject to the per-course
allowlist and MADSYSTEM temporal confirmation. The current shared queue is acceptable for
the sparse layout because unconfigured IDs are ignored and valid checkpoints do not
overlap. Mini-sector layouts require separate evidence per ID; a blanket reset on any ID
change would make false IDs reduce valid-marker reliability.

The 2x2 composite validation repeated one 1920x1080 / 60 FPS FPV clip in four 960x540
quadrants and processed 1,800 frames through one NVDEC decoder plus one four-item GPU
detection batch. GPU processing p95 was 6.638ms and decode-to-result wall-time p95 was
7.221ms. Four frames exceeded 16.67ms. Every quadrant matched the CPU oracle's single ID 1
appearance group at frames 319 through 433. This passes the single-input WebcamMulti
scheduling check. It does not yet prove four independent live sources, and the production
queue must discard stale frames instead of accumulating work during the measured spikes.

MADSYSTEM currently resizes and detects the complete composite once, then maps marker
centers to quadrants. The GPU PoC splits the decoded frame and batch-detects each quadrant.
Integration must preserve MADSYSTEM's per-quadrant ID list contract and keep allowlist,
temporal confirmation, checkpoint state, pit observations, and pilot assignment outside
the detector process until those responsibilities are migrated deliberately.

On 2026-08-14 the first Legacy Adapter slice was implemented. Four independent replay
decoders publish per-frame observations through `Local\MomoMarkerObservationsV1`; the
MADSYSTEM native reader consumed 500 batches in the final 10-second overlap with zero drops. The
15-second producer run published 743 batches at 49.52 Hz with 11.294ms processing p95.
The final standalone 30-second gate published 1,494 batches at 49.792 Hz with 9.944ms
processing p95. This closes replay-to-MADSYSTEM IPC and preserves repeated marker instances. Keep this
issue open: live Relay/WebRTC input, shadow parity evidence, one-hour operation, and
qualified Reliable marker events remain incomplete.
