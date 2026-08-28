# Race Audio Scaling Implementation Plan

Status: Phase 1 and the first central pre-race slice are implemented; live race validation pending

## Goal

Keep race-wide announcements consistent while allowing each Pilot Viewer to
receive useful, low-latency personal callouts as the field grows beyond four
cars.

## Responsibility boundary

| Responsibility | Owner | Transport |
| --- | --- | --- |
| Race start, pre-race, finish, safety and commentary shared by all drivers | Central race audio bus | One synthesized Opus clip broadcast to all Pilot peers |
| Lap time, car ahead, rear pressure and future vehicle-local status | Pilot Viewer + Relay validation | Reliable `momo-race-audio` DataChannel, Browser Kokoro |
| Official timing, position and gaps | Race Control timing state | Existing `race_state v2` |
| Speech text and phoneme policy | Relay + Race Audio Service | Fixed templates and `/v1/prepare` |
| Audio inference for personal callouts | Pilot browser | Web Worker + WebGPU Kokoro |

The Viewer does not calculate official gaps or race results. It only decides
when an already accepted `intervalToAheadMs` value is useful to announce.

## Phase 1: personal Pilot callouts

### Scope

- Keep `lap_complete` on Browser Kokoro when the browser reports support.
- Add `gap_ahead` and `gap_behind` callouts.
- Prefer an explicit `carNumber`; during migration only, extract the trailing
  number from `carId` such as `CP-2` or `FPV-02`.
- Do not speak arbitrary pilot names. A future locked roster may provide an
  explicit spoken name or phoneme form.
- Keep `race_finish` on the central Opus path.

### DataChannel request

The Pilot sends the following JSON on the existing reliable
`momo-race-audio` DataChannel:

```json
{
  "type": "race_audio_callout_request",
  "version": 1,
  "requestId": "gap_behind-12",
  "kind": "gap_behind",
  "carNumber": 7,
  "gapMs": 600
}
```

Rules:

- `kind` is limited to `gap_ahead` and `gap_behind`.
- `carNumber` is an integer from 1 through 999.
- `gapMs` is an integer from 100 through 5000 and is normalized to 100 ms.
- `requestId` is a bounded opaque deduplication key; it is not interpreted.
- Raw text, voice, model, URL and phonemes are not accepted from the browser.
- Relay applies a hard two-second limit per Pilot in addition to Viewer UX
  cooldowns.

### Viewer trigger policy

- Evaluate only accepted `race_state v2` snapshots while phase is `green`.
- Seed state from the first snapshot without announcing it.
- Announce when a rival changes, a gap crosses into 2.5 seconds, or the gap
  changes by at least 0.5 seconds at a later timing marker.
- Rear pressure has priority over the car ahead.
- Use an eight-second cooldown per direction and a three-second global
  cooldown. A transition into the one-second rear critical band may bypass the
  direction cooldown, but not the global guard.
- Do not announce lap-difference-only values as seconds.

## Phase 2: central race-wide bus

The current race audio source is owned by one Relay source/car. Generating the
same pre-race announcement through every source would multiply synthesis work
and can drift between drivers. The central bus therefore requires a separate
server-level component:

1. Consume one authoritative race event stream.
2. Deduplicate once by `raceRunId` and event key.
3. Synthesize or fetch one cached Opus clip.
4. Broadcast the same packet sequence to every subscribed Pilot track.
5. Keep per-Pilot language policy explicit. The initial operational choice is
   one event language per race; multilingual races require one cached clip per
   language, not per car.

Initial global events are pre-race briefing, grid ready, race start warning,
finish and safety announcements. Countdown light sounds remain local and
clock-based so audio generation cannot affect the start signal.

The first implemented slice is `pre_race_formation`. Race Operations sends a
fixed command for the locked `raceRunId` and ordered grid identity
(`carId`, `displayNumber`, and `pilotName`). Relay accepts no arbitrary text,
builds the fixed grid-introduction grammar, synthesizes the Japanese phrase
once, queues the same Opus clip to every active Pilot track, and returns its
duration. Coordinator leaves the run Prepared. Countdown-to-Green starts only
when the operator later presses Start Sequence. A missing Pilot audio track,
Race Voice Off, synthesis failure, or an announcement beyond the single-clip
limit fails visibly without guessing or silently omitting a Pilot.

## Phase 3: remove central prepare work for fixed phrases

For 20 to 30 cars, browser inference scales with Pilot hardware, but central
`/v1/prepare` calls can still burst at a checkpoint. After Phase 1 is measured:

- ship a versioned local phrase grammar for digits, car numbers, lap, position
  and gaps;
- run the same tokenizer in the browser Worker;
- keep `/v1/prepare` for dynamic commentary and roster-provided spoken names;
- require the browser grammar version in Relay capabilities before using it.

## Performance and failure gates

- Viewer sends at most one request in three seconds and Relay accepts at most
  one in two seconds per Pilot.
- Relay work queues are bounded; a full queue drops the personal callout without
  affecting command, telemetry, race or video paths.
- Browser generation remains latest-useful-wins. Old generated PCM is not
  played after a newer prompt supersedes it.
- Browser Kokoro failure falls back to OS speech for the current event and then
  advertises `remote` mode.
- Measure `/v1/prepare` bursts at 1, 4, 8, 16 and 32 concurrent requests,
  recording error rate and p50/p95 latency. Thread safety of Japanese G2P is a
  release gate for Phase 3, not an assumption.
- Run the Pilot with live video, M5 audio and telemetry while measuring Worker
  generation time, dropped video frames and main-thread long tasks.

## WBS

| ID | Work | Status |
| --- | --- | --- |
| A1 | Browser Kokoro lab and Japanese phoneme policy | done |
| A2 | `/v1/prepare` and local lap-time inference | done |
| A3 | Pilot gap planner and structured request | done |
| A4 | Relay validation, rate limit and fixed templates | done |
| A5 | Viewer distribution sync and automated tests | done |
| A6 | Live multi-car gap and audio-priority validation | pending |
| B1 | Server-level global event detector | partial: fixed pre-race command complete |
| B2 | One-clip multi-Pilot Opus broadcast | automated verification complete |
| B3 | Pre-race announcement operational test | pending |
| C1 | Versioned browser phrase grammar | pending |
| C2 | 32-Pilot prepare/inference load test | pending |

## Acceptance criteria for Phase 1

- Ahead and behind callouts use the same cars and gaps as Battle Meter.
- A malformed or arbitrary browser message cannot make Relay speak supplied
  text.
- Duplicate and rapid requests do not create repeated prompts.
- Existing remote Opus finish audio and Browser Kokoro lap audio still work.
- Viewer tests, Relay Go tests, `go vet`, distribution build and source-to-
  distribution hash checks pass.

## 2026-08-19 desktop validation

The Kokoro service was started with the production Japanese G2P policy and
`benchmark_prepare_burst.py` was run against `/v1/prepare`. The HTTP accept
queue was raised from the Python default to 64 after the first run exposed
connection backlog at 16 or more simultaneous requests.

| Language | Concurrent requests | Errors | P50 | P95 | Max |
| --- | ---: | ---: | ---: | ---: | ---: |
| ja-JP / `jf_alpha` | 32 | 0 | 10.83 ms | 18.19 ms | 18.73 ms |
| en-US / `am_michael` | 32 | 0 | 8.85 ms | 14.82 ms | 16.11 ms |

These figures cover central phoneme/token preparation only. Browser model load,
local inference, live video frame impact and audible scheduling still require a
Pilot PC test. The benchmark is reproducible with:

```powershell
cd E:\src\momo\tools\race-audio-service
.\.venv\Scripts\python.exe .\benchmark_prepare_burst.py --language ja-JP
.\.venv\Scripts\python.exe .\benchmark_prepare_burst.py --language en-US
```
