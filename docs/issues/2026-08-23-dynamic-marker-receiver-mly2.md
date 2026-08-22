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
  -> one variable-size GPU detection tick
  -> one MMO1 mapping (1..32 source records)
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

- cycle-time p95 exceeds 80 percent of the current period, or
- deadline misses exceed five percent,
- for three consecutive windows.

After a downgrade, a ten-second hold prevents oscillation. Automatic downgrade remains available
during Green because preserving fresh observations is preferable to accumulating delay. Automatic
upgrade is evaluated only at the next idle/Prepare boundary after at least 60 seconds below 55 percent
period utilization and less than one percent deadline misses. The controller never automatically
drops below 25 Hz. Continued overload at 25 Hz sets `capacity_exceeded`, keeps latest-frame dropping,
and requires fewer cars or another Marker Node.

Changing Hz must not change passage semantics. Timing Engine now offers the explicit
`qualification.mode=elapsed_time` path, using unique source sequences, `minimumPresenceMs`, and
`exitDurationMs`. Tests apply the same passage to 50, 40, 33, and 25 Hz and verify that replaying one
frozen source sequence cannot qualify a passage. Existing configurations default to
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

| Sources | Duration | Result | Final profile | Publication rate | Cycle p95 |
| ---: | ---: | --- | ---: | ---: | ---: |
| 1 | 10 s | pass | 50 Hz | 49.80 Hz | 2.99 ms |
| 5 | 15 s | pass | 50 Hz | 49.87 Hz | 5.07 ms |
| 8 | 20 s | pass | 50 Hz | 49.90 Hz | 5.65 ms |
| 12 | 30 s | pass | 50 Hz | 48.97 Hz | 8.45 ms |
| 20 | 60 s | pass | 50 Hz | 49.88 Hz | 12.97 ms |
| 32 | 60 s | pass | 50 -> 40 -> 33 Hz | 37.73 Hz mixed average | 23.88 ms |
| 32 | 30 s | pass | fixed 25 Hz | 24.93 Hz | 21.84 ms |

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

## Notes

- Existing MLY1 and the legacy MADSYSTEM adapter remain available for rollback during migration.
- MLY2 does not increase WebRTC bandwidth; it removes fixed four-plane publication and unnecessary
  whole-batch copies after decode.
- GPU throughput still sets a physical node limit. The verified RTX 5070 result is not a guarantee
  for an RTX 3060 or another host. The contract stops at 32, so this run does not claim that 32 is the
  hardware failure point; larger races still require another Marker Node or a future contract change.
- Real independent Momo cameras, one-source loss/recovery, and event-day soak evidence remain before
  this path replaces the four-source production marker path.
