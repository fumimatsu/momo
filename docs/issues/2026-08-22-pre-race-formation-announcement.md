# Pre-race formation announcement

- Status: doing

## Context

The local experience event needs one Japanese announcement shared by all Pilot
Viewers while drivers complete a formation lap and take their grid positions.
The existing per-car race audio source must not synthesize the same clip three
times or drift into three different start times.

## Goal

Race Operations owns one `Formation + Start` command. Relay synthesizes a
fixed Japanese phrase once, broadcasts the same Opus clip to the locked roster,
and Coordinator starts the existing red-light sequence after playback and a
short hold.

## Acceptance Criteria

- No arbitrary text, voice, model, or URL enters Relay from the Console.
- The command requires every locked car to have an active Pilot audio track.
- One synthesis response is reused across all target Pilot queues.
- Exact retries do not replay the announcement within the same run.
- Operations `ABORT` cancels the pending formation/countdown wait before run rollback.
- Countdown and Green continue to use the existing authoritative state machine.
- Manual `Start Sequence` remains available as an explicit fallback.

## Verification

- Relay and Coordinator automated tests pass.
- VOICEVOX speaker 51 produces the fixed phrase through Race Audio Service.
- A three-Pilot live run hears one synchronized announcement, then the existing
  red-light sequence starts after the configured hold.

## Notes

Automated verification is complete. Three-Pilot audible timing, M5 audio
ducking, and event-room volume remain live gates.
