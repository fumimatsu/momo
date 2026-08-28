# Pre-race formation announcement

- Status: doing

## Context

The local experience event needs one Japanese announcement shared by all Pilot
Viewers while drivers complete a formation lap and take their grid positions.
The existing per-car race audio source must not synthesize the same clip three
times or drift into three different start times.

## Goal

Race Operations owns one `Formation Announcement` command. Coordinator sends
the locked roster's car numbers and Pilot names. Relay builds the fixed grid
introduction, synthesizes it once, and broadcasts the same Opus clip to the
locked roster. Coordinator leaves the run Prepared until the operator presses
the separate `Start Sequence` command.

## Acceptance Criteria

- No arbitrary text, voice, model, or URL enters Relay from the Console.
- The command requires every locked car to have an active Pilot audio track.
- One synthesis response is reused across all target Pilot queues.
- Exact retries do not replay the announcement within the same run.
- Operations `ABORT` cancels a pending announcement request before run rollback.
- Countdown and Green continue to use the existing authoritative state machine.
- `Start Sequence` is the only Console action that starts Countdown and Green.

## Verification

- Relay and Coordinator automated tests pass.
- VOICEVOX speaker 51 produces the fixed phrase through Race Audio Service.
- A three-Pilot live run hears one synchronized grid introduction and remains
  Prepared until an operator explicitly starts the red-light sequence.

### 2026-08-22 local VoiceVox check

- Race Audio Service health reported `voicevox:http://127.0.0.1:50021:speaker-51`.
- The first uncached fixed phrase took about 4.2 seconds to synthesize.
- The cached fixed phrase took about 12 milliseconds in the service and was accepted by Relay in about 83 milliseconds.
- The generated Opus clip was 14.661 seconds and was queued once for the connected `CP-1` Pilot.
- The Pilot advertised `ja-JP` with remote audio mode; Browser Kokoro was disabled.
- Marker Observer continued to receive about 50 new frames per source each second with a cycle p95 near 15 milliseconds.
- Relay kept `11.3`, `11.4`, and `11.5` at 49 to 50 fps during the check.

## Notes

Automated verification and one-Pilot transport verification are complete. Three-Pilot grid-name pronunciation,
manual Start Sequence separation, M5 audio ducking, and event-room volume remain live gates. The current roster
order is not a qualifying-derived grid position; that requires a future explicit `gridPosition` contract.
