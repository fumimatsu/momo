# Dynamic Marker Receiver and MLY2

Status: doing

## Context

The current live marker path publishes four fixed 960x528 Y planes through `MLY1` and runs one
GPU worker against that fixed batch. Four is an IPC implementation limit, not a WebRTC, ArUco,
MMO1, or race contract limit. Exposing groups of four to race operations would make source addition,
removal, and detector assignment unnecessarily fragile.

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

Changing Hz must not change passage semantics. Before Green-time adaptation is enabled, Timing Engine
qualification must be based on elapsed presence time and unique source sequences rather than a fixed
number of batches. Until that change is verified, automatic selection runs during preflight and the
selected profile remains locked for the race.

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
selection, and the isolated adaptive rate controller have unit tests. The frame sampler covers stale,
skewed, missing, and duplicate source frames without blocking another source. MMO1 writers can update
the advertised effective detection rate. The controller is not connected to the legacy MLY1 worker
because fixed-batch qualification still has frame-count semantics.

MLY2 writer/reader, variable GPU batch, time-based qualification, and end-to-end adaptive-rate tests
are still required before a live five-car authority run.

## Notes

- Existing MLY1 and the legacy MADSYSTEM adapter remain available for rollback during migration.
- MLY2 does not increase WebRTC bandwidth; it removes fixed four-plane publication and unnecessary
  whole-batch copies after decode.
- GPU throughput still sets a physical node limit. Supporting 32 sources in the contract is not a
  claim that every GPU can detect 32 sources at 25 or 50 Hz.
