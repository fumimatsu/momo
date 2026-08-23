# Dynamic Marker Receiver and MLY2

Status: doing

## Context

The legacy live marker path publishes four fixed 960x528 Y planes through `MLY1` and runs one GPU
worker against that fixed batch. Four is an IPC implementation limit, not a WebRTC, ArUco, MMO1, or
race contract limit. Exposing groups of four to race operations would make source addition, removal,
and detector assignment unnecessarily fragile. The `MLY2` path described here is now implemented;
the legacy path remains only as migration rollback.

## Goal

Run one dedicated Native Marker Receiver and one logical GPU detector for the one through 32 cars
locked into a race. Source membership is selected from the Relay marker-source manifest at Prepare.
The receiver owns WebRTC, hardware decode, latest-frame storage, source health, and reconnection. It
does not own audio, telemetry parsing, SDL presentation, BGRA composition, timing, or race state.

```text
Relay marker source manifest
  -> p2p-marker-recv (1..32 independent PeerConnections)
  -> MLY2 source-local latest Y planes
  -> one 50 Hz GPU detection epoch with opportunistic source batches
  -> one MMO1 mapping (partial updates, up to 32 source records per batch)
  -> Timing Engine
```

More than 32 cars may use another Marker Node and MMO1 mapping. Four-source grouping is not part of
the new operating contract.

## Race lifecycle

1. While idle, Relay derives candidates from DRIVE-enabled vehicle sources.
2. Prepare locks the roster and source IDs for the run.
3. Marker Receiver reconciles to that manifest and warms every available source.
4. Preflight reports source age, decode state, effective detection Hz, and missing sources.
5. Countdown and Green do not add, remove, or reorder sources.
6. A disconnected source keeps its source slot but publishes no observation until a new frame arrives.
7. Finish keeps the run mapping for diagnostics. The next Prepare may replace it without restarting the
   service.

## Connection lifecycle

The Marker Receiver keeps one long-lived process and reconciles source topology without a process
restart. It does not start all PeerConnections at once. A bounded queue starts four WebRTC media
negotiations concurrently by default, with CLI overrides from one through eight and a 20-second
per-attempt timeout. A completed, failed, or timed-out attempt releases its queue slot immediately so
another source can proceed.

Every callback carries the receiver generation and a source-local attempt ID. Notifications from a
closed PeerConnection therefore cannot disconnect or reconnect a same-named source in a newer run.
The connection queue is cleared on topology replacement; source reconnects return to that queue and
do not block healthy sources. This removes the simultaneous ICE/NVDEC allocation burst observed when
32 local sources were started together.

## MLY2 latest-frame contract

MLY2 is one mapping with a maximum of 32 source slots. Each source owns an independent metadata
seqlock and one latest 960x528 Y plane. A decoded frame updates only its own plane; the receiver never
copies or clears all 32 planes just because one source changed.

Required header fields:

- magic/version and bounded mapping size
- source capacity and active source count
- manifest revision and receiver generation
- QueryPerformanceCounter frequency used by all source timestamps
- source table with stable source ID and slot index for the locked run

Required source metadata:

- source-local write guard and frame sequence
- frame-received QPC tick and optional diagnostic Unix timestamp
- connected, video-valid, and format flags
- width, height, stride, and plane offset
- receive frame count and overwritten-frame count

The source sequence is monotonic for a slot throughout the locked run and does not reset when that
source reconnects. A new manifest revision at the next Prepare may create a new receiver generation
and reset the worker's last-detected sequence table.

The worker samples all sources at one detection tick `T`. For each source it atomically takes the
newest frame available at `T`; it never waits for another source and never reads an older queue entry.
A Windows auto-reset frame-ready event wakes the worker when any MLY2 source publishes a frame. The
wait is bounded by the next detection deadline and does not make one source wait for another; after
waking, the worker still samples every source independently under the same freshness and skew rules.
A frame is eligible only when all of the following are true:

- the source is connected and its latest frame is valid
- its source sequence has not already been detected
- its frame age is within the configured freshness limit
- its age relative to the newest eligible source is within the configured batch-skew limit

An ineligible or missing source produces an MMO1 source record with `videoValid=false` and no
detections for that tick. It does not delay the other sources. Reusing one frozen frame on repeated
ticks must not satisfy the passage confirmation count.

Independent cameras are not genlocked, so exact capture-time equality cannot be promised. Alignment
means one receiver-side sampling tick, newest-frame selection, measured per-source age, and bounded
inter-source skew. Candidate initial limits are 60 ms maximum frame age and 40 ms batch skew at a
50 fps input, but they remain validation values until replay and live evidence establish production
thresholds.

MMO1 already carries source sequence, frame received time, detection time, video-valid state, and up
to 32 source records. It remains the first output contract. The writer must update its advertised
effective detection Hz when the adaptive controller changes profile. A future MMO2 is required only
if Timing Engine needs receiver QPC ticks rather than producer-side freshness decisions and the
existing Unix evidence fields.

## Adaptive detection rate

Detection frequency is node-wide, never source-specific. All available sources are sampled on the
same tick cadence so one car cannot receive a systematically different temporal confirmation rule.

Supported automatic profiles are `50 -> 40 -> 33 -> 25 Hz`. The controller uses rolling five-second
windows and changes at most one level at a time.

Initial downgrade proposal:

- active processing-time p95, excluding the bounded frame-ready wait, exceeds 80 percent of the
  current period, or
- deadline misses exceed five percent,
- for three consecutive windows.

The end-to-end cycle duration and frame-ready wait remain diagnostic metrics. Treating intentional
wait time as GPU processing load causes phase-dependent false downgrades even when actual detection
has ample headroom.

After a downgrade, a ten-second hold prevents oscillation. Automatic downgrade remains available
during Green because preserving fresh observations is preferable to accumulating delay. Automatic
upgrade is evaluated only at the next idle/Prepare boundary after at least 60 seconds below 55 percent
period utilization and less than one percent deadline misses. The controller never automatically
drops below 25 Hz. Continued overload at 25 Hz sets `capacity_exceeded`, keeps latest-frame dropping,
and requires fewer cars or another Marker Node.

Changing Hz must not change passage semantics. Timing Engine now offers the explicit
`qualification.mode=elapsed_time` path, using unique source sequences, `minimumPresenceMs`,
`exitDurationMs`, and the bounded `maximumObservationGapMs`. A short sampling miss suspends elapsed
time instead of contributing presence or exit evidence; a longer miss clears the candidate. Tests
apply the same passage to 50, 40, 33, and 25 Hz and verify that replaying one frozen source sequence
cannot qualify a passage. Existing configurations default to
`legacy_frames`; elapsed-time mode must be selected explicitly until site replay fixes production
durations.

A noisy image must not consume an unbounded share of a tick. Connected components and marker
candidates remain bounded per source. A source exceeding its per-frame budget is marked
`source_over_budget` for that tick instead of creating an unbounded queue or reducing only that
source's cadence.

## Acceptance Criteria

- One receiver and one MLY2 mapping accept one through 32 manifest sources without four-source groups.
- Five 50 fps replay sources create one five-source GPU batch and one MMO1 mapping.
- Stalling one source makes only that source invalid; other sources keep their target cadence.
- Every MMO1 record can be traced to a unique source frame sequence and measured frame age.
- No source is detected twice from the same frozen frame for passage qualification.
- Synthetic overload steps through 50, 40, 33, and 25 Hz without per-source rate divergence or rapid
  oscillation.
- Overload at 25 Hz reports `capacity_exceeded` and does not silently lower the rate further.
- Prepare locks source membership; a source change is applied to the next run, not midway through Green.
- Replay tests cover five-source skew, one-source loss, delayed frame arrival, reconnect generation,
  and profile transitions before live testing.

## Verification

The Relay manifest contract, stable topology revision, conditional GET, source-local latest-frame
selection, MLY2 writer/reader, frame sampler, variable GPU batch, MMO1 output, and adaptive rate
controller have automated tests. Timing Engine has elapsed-time qualification tests at 50, 40, 33,
and 25 Hz. The legacy MLY1 worker and legacy frame-count qualifier remain unchanged.

The dummy upstream is one looped 960x528 H.264 50 fps recording exposed as independent Relay sources.
Momo receives every source through WebRTC, decodes each stream with NVDEC, publishes MLY2, and runs
one GPU detector tick. Measurements were made on Windows with an AMD Ryzen 7 9700X, NVIDIA GeForce
RTX 5070 12 GB, and driver 610.47.

### Instrumented single-source baseline

One source was measured without browser rendering or screen recording for 60 seconds. The original
worker passed its old publication-rate gate at 49.967 Hz, but it actually detected only 2,463 fresh
frames, or approximately 41.05 Hz. It published empty MMO1 batches for the remaining ticks and
reported 512 duplicate-frame samples plus 21 frames changed during copy. This established that the
old gate overestimated detector capacity.

The updated path adds a source-local fresh-frame ratio, receiver input rate, warm-up exclusion,
per-stage timing, process-tree CPU/memory sampling, and GPU sampling. It also wakes on the MLY2
frame-ready event, avoids a redundant GPU synchronization, and uses a direct plane copy when the
decoded Y plane already matches 960x528 without flips. Three repeated 60-second optimized runs gave:

| Run | Receiver input | Fresh detection | Fresh ticks | Publication | Processing p95 | Frame age p95 |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| A | 50.009 Hz | 49.933 Hz | 99.867% | 50.000 Hz | not yet separated | 17.440 ms |
| B | 50.013 Hz | 49.900 Hz | 99.833% | 49.983 Hz | 4.050 ms | 7.867 ms |
| C | 49.997 Hz | 49.817 Hz | 99.633% | 50.000 Hz | 3.975 ms | 15.787 ms |

All runs passed the 95 percent fresh-source coverage gate at fixed 50 Hz. In the two runs with the
final timing schema, active processing stayed at 3.975-4.050 ms p95 and 5.336-5.628 ms maximum against
a 20 ms period. Depending on input/detector phase, the bounded event wait changed end-to-end cycle
p95 from 5.628 to 13.687 ms; it did not represent compute saturation and is now excluded from the
adaptive load decision.

Across the repeated runs, total GPU utilization averaged 12.8-13.1 percent and reached 14 percent
p95. A contemporaneous ten-sample idle check on the same desktop averaged 8.3 percent, so these values
are whole-GPU readings rather than process-attributed utilization. The Marker Receiver used
approximately 309-311 MiB working set and averaged 5.1-7.2 percent of one logical CPU core. The GPU
worker used approximately 266-267 MiB working set and peaked at a 9.1 percent one-core average in the
most CPU-active run. This leaves substantial one-source compute headroom on this host.

The known clip produced marker IDs 1, 2, and 3. Each optimized run also produced one single-frame ID
10 candidate; it does not satisfy elapsed-time passage qualification, but it remains recognition
quality evidence and is not counted as a performance improvement.

Evidence:

- `tools/.artifacts/dynamic-marker/single-baseline-20260823/sources-01.json`
- `tools/.artifacts/dynamic-marker/single-optimized-20260823/`
- `tools/.artifacts/dynamic-marker/single-optimized-run2b-20260823/`
- `tools/.artifacts/dynamic-marker/single-optimized-run3-20260823/`

### Strict multi-source matrix

The same schema-version-2 gate was run for 60 seconds at fixed 50 Hz with 5, 10, 15, and 20 replay
sources. Browser rendering and recording were disabled. `ContinueOnValidationFailure` kept the
ascending matrix running after a failed operating point so every requested count retained evidence.

| Sources | Input min | Detection min / mean | Fresh min / mean | Processing p95 / max | GPU p95 | Receiver CPU mean | Result |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 5 | 49.996 Hz | 36.833 / 42.710 Hz | 73.67 / 85.42% | 4.319 / 8.854 ms | 17% | 0.40 core | fail |
| 10 | 49.980 Hz | 36.017 / 43.153 Hz | 72.03 / 86.31% | 7.372 / 14.902 ms | 25% | 0.90 core | fail |
| 15 | 49.980 Hz | 36.867 / 43.477 Hz | 73.73 / 86.95% | 10.015 / 20.942 ms | 34% | 1.52 cores | fail |
| 20 | 49.962 Hz | 37.700 / 42.962 Hz | 75.40 / 85.93% | 12.358 / 26.541 ms | 48% | 2.04 cores | fail |

Every receiver source stayed at approximately 50 fps, publication stayed at 50 Hz, and active GPU
processing p95 stayed below the 16 ms headroom limit through 20 sources. The 20-source run recorded
nine deadline misses in 3,000 ticks, but compute saturation was not the primary failure. Every
operating point failed only the 95 percent per-source fresh-frame coverage gate.

The cause is the current batching scheduler. It waits for a frame-ready event only when no source is
eligible. With independent multi-source arrival phases, at least one source is almost always fresh,
so the worker never waits (`frameEventSignals=0` for all four runs). It samples one batch immediately;
sources whose next decoded frame arrives just after that tick are reported as duplicate on that tick,
and bursty decode can overwrite an intermediate latest frame before the next tick. This produced
2,049, 4,023, 5,790, and 8,115 duplicate samples respectively even though the receiver input rate did
not fall.

This means the current worker does not provide a fair 50 fresh detections per second to every car once
multiple asynchronous sources are present. It does still exceed the present 25 Hz operational floor:
the worst source was 36.017 Hz at ten sources and 37.700 Hz at twenty sources. A lower fixed global
profile may therefore produce more even useful coverage without changing passage semantics, but that
must be measured rather than inferred.

Before splitting across multiple Marker Nodes, the next scheduler experiment should collect
source-local readiness during a bounded micro-batch window and process each newly arrived source once.
If every received frame must be preserved, MLY2 also needs a small source-local ring rather than one
latest plane. The existing latest-plane contract can remain if the target is a fair 25-40 Hz and
dropping intermediate frames is accepted. The strict matrix does not yet justify splitting at 20:
GPU p95 was 48 percent and processing p95 was 12.358 ms, while total GPU memory was 7.33 GiB including
the approximately 2.10 GiB desktop baseline.

Evidence is stored in
`tools/.artifacts/dynamic-marker/strict-05-10-15-20-20260823/summary.json` and its per-count detector
and runtime reports.

### QPC scheduler and opportunistic partial batches

The fixed-50-Hz matrix was repeated after correcting the scheduler clock and adding opportunistic
partial batches. Python 3.12 on Windows implements `time.monotonic()` with `GetTickCount64`, which is
quantized to 15.625 ms on this host. The worker had therefore used a clock that could not represent
its 20 ms period or a 3 ms coalescing deadline accurately. Detection epochs, deadlines, elapsed time,
and inter-batch measurements now use QueryPerformanceCounter through `time.perf_counter()`.

One detection epoch processes every source that is fresh at the epoch boundary immediately. While
GPU work is running, independently phased frames may become ready. The worker samples again and
publishes only those newly ready sources in another GPU batch before the same epoch deadline. A
source is processed at most once per epoch. MMO1's reserved slot flag now marks these writes as
partial updates: an omitted source means no update, while a source record explicitly carrying
`videoValid=false` remains the invalidation signal. Zero flags retain the original full-snapshot
behavior and continue to reset an absent configured source.

The partial-batch worker and the Timing Engine that understands `BatchFlagPartial` are one deployment
unit. An older Timing Engine ignores the reserved flag and treats omitted sources as absent, so it
must not be paired with this worker. Existing complete-snapshot producers remain compatible with the
new Timing Engine.

The same 1080p50 replay was measured for 60 seconds at fixed 50 Hz with browser rendering and
recording disabled:

| Sources | Detection min / mean | Fresh min | Batches/epoch p50 / p95 | Processing p95 | Inter-batch skew p50 / p95 | GPU avg / p95 | Observer CPU avg | Result |
| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: | --- |
| 5 | 49.983 / 49.993 Hz | 99.967% | 2 / 2 | 6.114 ms | 2.066 / 2.743 ms | 18.3 / 19% | 0.27 core | pass |
| 10 | 49.950 / 49.986 Hz | 99.900% | 2 / 3 | 10.280 ms | 2.970 / 5.827 ms | 28.7 / 32% | 0.42 core | pass |
| 15 | 49.933 / 49.978 Hz | 99.867% | 2 / 3 | 11.948 ms | 2.515 / 5.688 ms | 36.3 / 39% | 0.49 core | pass |
| 20 | 49.899 / 49.944 Hz | 99.800% | 2 / 4 | 15.775 ms | 4.569 / 8.766 ms | 46.8 / 50% | 0.66 core | pass |

Compared with the immediate single-batch scheduler, the worst per-source rate improved by
12.199-13.933 Hz and every source passed the 95 percent fresh-frame gate. Five through fifteen
sources keep representative inter-source detection differences in the requested few-millisecond
range. Twenty sources still hold 50 Hz and remain below the 16 ms active-processing p95 gate, but
the p95 difference reaches 8.766 ms and 36 deadline misses occurred in 3,000 epochs. Twenty is
therefore a demonstrated fixed-50-Hz point on this RTX 5070 Ti host, not a universal production
capacity guarantee. A second Marker Node remains the next scaling option when lower GPU classes,
simultaneous rendering/recording, or tighter cross-source timing is required.

At 50 Hz, one video frame is 20 ms. Inter-batch detection skew p95 stayed within one frame at every
source count. The observed maximums for 5, 10, 15, and 20 sources were 16.073, 15.863, 18.266, and
21.896 ms respectively; only the single 20-source maximum slightly exceeded one frame. The current
result therefore supports a one-frame operational tolerance, while the p95 values remain the useful
capacity criterion rather than an absolute zero-outlier guarantee.

The worker published 88-119 MMO1 batches per second across this matrix. Timing Engine polls every
10 ms, drains up to all 16 ring slots per poll, and its full test suite plus the immutable replay pass
with the partial-batch contract. A live authority run must still verify dropped-batch diagnostics
remain zero before this mode is released.

Evidence is stored in
`tools/.artifacts/dynamic-marker/partial-batch-qpc-fixed50-05-10-15-20-20260823/summary.json` and its
per-count detector and runtime reports.

```powershell
.\tools\Invoke-DynamicMarkerCapacityValidation.ps1 `
  -InputPath E:\recordings\marker-test.mp4 `
  -SourceCounts 5,10,15,20 `
  -FrameRate 50 `
  -MeasurementSeconds 60 `
  -InitialDetectionHz 50 `
  -NoAdaptive `
  -ContinueOnValidationFailure
```

The five-source authoritative map E2E also passed on 2026-08-23. The same long recording started at
five different keyframes and produced real marker IDs 1, 2, and 3 for every source. Coordinator locked
five DRIVE-enabled participants, consumed MMO1, published Timing Engine snapshots through Race
Control, and Team Observer rendered five distinct course positions. The validation artifact recorded
27 qualified marker events across the five sources at the initial pass point; no synthetic timing
publisher was used. Browser verification found five SVG car markers and four changing positions over
a 2.5-second sample, while the fifth was held at its latest checkpoint correction position.

The same authoritative E2E passed with ten sources on 2026-08-23. Replay start positions were spread
across the first 80 percent of keyframes so every source retained enough post-Green recording time;
this changes only the real video start position and does not synthesize detections or timing. Every
source produced marker events, with IDs 1, 2, and 3 observed globally, and Team Observer rendered ten
SVG markers at ten distinct estimated course positions. The locked roster supplied ten unique colors
from the shared 16-color dark-UI palette while car number remained the primary identifier. The GPU
worker kept the 50 Hz profile with ten eligible sources. Across 790 one-second log samples, cycle
time averaged 6.34 ms, with 7.25 ms p95 and 12.89 ms maximum. Evidence is stored in
`tools/.artifacts/virtual-fleet-map/run-20260823T013435370Z/validation.json`.

Repeated ten-source runs then exposed a timing-boundary defect rather than a GPU throughput limit.
MLY2 intentionally publishes an ineligible source as `videoValid=false` for that sampling tick; the
elapsed-time qualifier treated one such tick as a complete video loss and erased the candidate. It
now suspends a pending candidate for at most `maximumObservationGapMs=120`, excludes the suspended
time from presence and exit evidence, and still resets on a longer gap. A source frame first reported
invalid may be processed when that same sequence later becomes eligible, but a sequence already
processed as valid is never counted twice. Legacy frame-count qualification remains unchanged.

The corrected full path passed again with ten sources at 50 Hz. All ten sources produced three to six
qualified marker events, marker IDs 1, 2, and 3 were observed, and the resulting standings had ten
distinct estimated map positions. Evidence is stored in
`tools/.artifacts/virtual-fleet-map/run-20260823T023922343Z/validation.json`. Browser validation showed
ten visible SVG markers with ten unique palette colors and four selected 960x528 video tracks all
advancing in real time. A 338-sample runtime snapshot stayed at the 50 Hz profile with 6.67 ms mean,
7.78 ms p95, and 10.34 ms maximum GPU cycle time.

Run the reproducible E2E with:

```powershell
.\tools\Start-VirtualFleetMapDemo.ps1 -CarCount 10 -DetectionHz 50 -ReplayClipDurationSeconds 0
```

The launcher writes `tools/.artifacts/virtual-fleet-map/runtime.json` and a run-local
`validation.json`. Stop all isolated test processes with `Stop-VirtualFleetMapDemo.ps1`.

| Sources | Duration | Result | Final profile | Publication rate | Cycle p95 |
| ---: | ---: | --- | ---: | ---: | ---: |
| 1 | 10 s | pass | 50 Hz | 49.80 Hz | 2.99 ms |
| 5 | 15 s | pass | 50 Hz | 49.87 Hz | 5.07 ms |
| 8 | 20 s | pass | 50 Hz | 49.90 Hz | 5.65 ms |
| 12 | 30 s | pass | 50 Hz | 48.97 Hz | 8.45 ms |
| 20 | 60 s | pass | 50 Hz | 49.88 Hz | 12.97 ms |
| 32 | 60 s | pass | 50 -> 40 -> 33 Hz | 37.73 Hz mixed average | 23.88 ms |
| 32 | 30 s | pass | fixed 25 Hz | 24.93 Hz | 21.84 ms |

This capacity matrix predates the strict per-source fresh-frame coverage gate. Its publication rate
shows worker output cadence, not proof that every source supplied a newly detected frame on the same
percentage of ticks. It remains historical evidence, but each multi-source operating point must be
rerun with schema version 2 before setting production capacity or Marker Node split thresholds.

At 32 sources the controller stepped down after the configured three-window overload rule and stayed
at 33 Hz. A separate 25 Hz run kept all 32 sources active, remained below the 40 ms period at p95,
and did not report capacity exhaustion. The measured 50 Hz operating boundary on this machine is at
least 20 sources and below 32 under the current 80 percent headroom policy; the exact crossover was
not narrowed further in this run.

A long-lived receiver changed from one to 32 sources without restarting or exiting; that transition
became video-valid in approximately 30 seconds. A cold 32-source start took 60.23 seconds with four
concurrent negotiations, compared with approximately 85 seconds and a heavy ICE/candidate burst when
all negotiations started simultaneously. The bounded queue prioritizes stability over minimum cold
start time; preflight must allow at least 90 seconds for a 32-source cold start. The reusable
`tools/Invoke-DynamicMarkerCapacityValidation.ps1` command runs an ascending source matrix through
one receiver and writes one JSON report per source count plus a summary.

```powershell
.\tools\Invoke-DynamicMarkerCapacityValidation.ps1 `
  -InputPath E:\recordings\marker-test.mp4 `
  -SourceCounts 1,5,8,12,20,32 `
  -FrameRate 50 `
  -MeasurementSeconds 60
```

### Current schema-v2 capacity boundary

The QPC deadline scheduler and partial MMO1 publication path were rerun with the strict per-source
fresh-frame gate on the RTX 5070 host. Fixed 50 Hz remained inside the 80 percent processing
headroom limit through 20 sources. Twenty-four and 28 sources retained more than 98 percent fresh
coverage but exceeded the headroom gate; 32 sources also fell below the 95 percent fresh-coverage
gate. Fixed 25 Hz passed at 32 sources.

| Sources | Requested profile | Result | Minimum fresh ratio | Processing p95 | Headroom limit |
| ---: | --- | --- | ---: | ---: | ---: |
| 24 | fixed 50 Hz | fail | 98.933% | 19.524 ms | 16.000 ms |
| 28 | fixed 50 Hz | fail | 98.499% | 20.819 ms | 16.000 ms |
| 32 | fixed 50 Hz | fail | 93.240% | 22.797 ms | 16.000 ms |
| 32 | fixed 25 Hz | pass | 99.800% | 22.856 ms | 32.000 ms |

Current adaptive runs establish a conservative initial-profile table for this host:

| Sources | Profile history | Result | Minimum fresh ratio | Processing p95 | Final limit |
| ---: | --- | --- | ---: | ---: | ---: |
| 24 | 50 -> 40 Hz at 15.056 s | pass | 99.714% | 17.695 ms | 20.000 ms |
| 28 | 50 -> 40 Hz at 15.042 s | fail | 99.778% | 20.247 ms | 20.000 ms |
| 28 | initial 33 Hz | pass | 99.838% | 19.693 ms | 24.242 ms |
| 32 | 50 -> 40 -> 33 Hz at 15.020 s and 30.040 s | pass | 98.093% | 22.311 ms | 24.242 ms |

The 28-source adaptive run exposes a control-boundary mismatch: no three consecutive five-second
windows exceeded the downgrade threshold, so the controller remained at 40 Hz, while the aggregate
run p95 exceeded the same 80 percent headroom gate by 0.247 ms. Fresh coverage remained healthy, so
this was a safety-margin failure rather than an observation-loss failure. Before production use,
select the initial profile from the frozen roster and retain measured-load fallback:

- 1 through 20 sources: initial 50 Hz
- 21 through 24 sources: initial 40 Hz
- 25 through 32 sources: initial 33 Hz
- overload at 33 Hz: downgrade to 25 Hz
- continued overload at 25 Hz: set `capacity_exceeded` and assign another Marker Node

The count bands are conservative operating defaults, not hardware-independent protocol limits.
The controller still needs a slightly conservative downgrade margin or an explicit near-limit rule
so 28 sources cannot remain at a profile that fails the final acceptance gate. Profile selection and
settling belong in Preflight/Prepare; Green must not begin while the node is still converging from an
obviously excessive initial profile.

## Notes

- Existing MLY1 and the legacy MADSYSTEM adapter remain available for rollback during migration.
- MLY2 does not increase WebRTC bandwidth; it removes fixed four-plane publication and unnecessary
  whole-batch copies after decode.
- Team Observer supports 16 unique run-local vehicle accent colors. Hue alone is not a reliable
  identity channel at that density, so the visible car number remains authoritative. Race Operations
  should assign unique roster colors when possible; the browser resolves missing or exact duplicate
  colors defensively and reuses the palette only after 16 vehicles.
- GPU throughput still sets a physical node limit. The verified RTX 5070 result is not a guarantee
  for an RTX 3060 or another host. The contract stops at 32, so this run does not claim that 32 is the
  hardware failure point; larger races still require another Marker Node or a future contract change.
- Real independent Momo cameras, one-source loss/recovery, and event-day soak evidence remain before
  this path replaces the four-source production marker path.
