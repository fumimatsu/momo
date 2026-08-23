# 2026-08-24 mixed CPU-shadow fleet replay

Status: done

## Context

The original virtual fleet test replayed one 50 fps recording at different start offsets. It proved
the 20-source media and Marker pipeline but did not produce enough relative pace variation to test
frequent changes in race order. The SDK Drive folder also contains CPU-shadow captures with matching
video, IMU telemetry, and steering command records.

The real transport directions must remain distinct:

```text
recorded IMU telemetry
  -> virtual Momo serial DataChannel
  -> Relay
  -> Pilot telemetry delivery

recorded steering commands
  -> virtual Pilot momo-command DataChannel
  -> Relay
  -> virtual Momo serial DataChannel
```

Sending steering commands from virtual Momo would test the wrong direction and is not supported.

## Goal

- Replay at least two real driving captures across a 20-car roster.
- Keep every virtual upstream at 50 fps while varying the recorded driving pace and start point.
- Produce authoritative Marker timing and observable race-order changes.
- Exercise recorded IMU and steering traffic alongside the video path without telemetry drops or
  command send errors.
- Record a two-minute Team Observer view as visual evidence.

## Implementation

- `New-CpuShadowFleetReplay.ps1` discovers complete CPU-shadow triplets in one directory, selects the
  two longest captures, prepares four 50 fps H.264 pace variants, and writes a 1-to-64-source replay
  manifest. Every fifth source uses the second capture; the others use the primary capture.
- `momo-virtual-source -profile-manifest` loads unique H.264 assets once, assigns one asset and
  keyframe-aligned start point per source, and replays recorded `TEL:` messages through each
  upstream `serial` DataChannel. Health output includes serial state, telemetry sent/dropped/errors,
  and commands received.
- `momo-relay-load -command-replay-jsonl -spread-command-starts` replaces the constant synthetic
  command with the original 50 Hz steering lines. Each Pilot begins at a different point in the
  command log. Replay-only command audit lines carry virtual `G:5` so the Team Observer displays
  the recorded `T:1500..1700` range instead of saturating it against the normal G1 range. Vehicle
  gameplay state remains unchanged.
- Replay telemetry is normalized once at startup to source-local monotonic `seq` and `t_us` values.
  Two deterministic eight-hex-digit `boot` IDs alternate at loop boundaries, so Viewer sequence
  validation accepts the next loop without per-message JSON processing during playback.
- `Start-VirtualFleetMapDemo.ps1` accepts both replay inputs and still uses real Marker detection,
  Coordinator timing, Race Control snapshots, and Team Observer rendering.
- `Measure-VirtualFleetReplay.ps1` records ranking order transitions and before/after transport
  counters.

The 8.9-second capture `cpu-shadow-20260731T123058717Z-fbe0ff40` was rejected for authoritative
timing after a first run: video, telemetry, and command replay worked, but none of the four assigned
sources produced a qualified Marker event. The usable inputs were the approximately 172-second
`cpu-shadow-20260731T122739445Z-732b1f8f` run and the approximately 77-second
`cpu-shadow-20260731T093643811Z-9ee91411` run.

## Acceptance Criteria

- All 20 virtual Momo sources are `STREAMING` and have an open `serial` channel.
- Marker IDs 1, 2, and 3 are observed and every source produces a qualified event.
- Team Observer renders 20 map markers and at least two distinct map progress positions.
- A timed sample contains more than one race order and at least one position transition.
- Recorded telemetry and commands traverse the expected DataChannels.
- Telemetry replay drops, telemetry send errors, and command send errors remain zero.
- The two-minute recording is readable at 1920x1080 and contains four selected onboard videos.

## Verification

The authoritative 20-car startup passed with Marker IDs 1, 2, and 3 from every source and 19
distinct map positions at the validation point.

The initial 30-second ranking and transport sample produced:

| Metric | Result |
| --- | ---: |
| Unique race orders | 10 |
| Order transitions | 9 |
| Cars with a position change | 15 |
| IMU telemetry sent | 17,314 |
| IMU telemetry dropped / send errors | 0 / 0 |
| Commands sent / received by virtual Momo | 30,550 / 30,587 |
| Command send errors | 0 |

After the HUD replay correction, virtual source 05 was selected because its approximately 77-second
capture starts near 79 percent and crosses the source loop boundary early. Team Observer samples
remained active after the boundary. Throttle changed `35 -> 40 -> 40 -> 38` percent, brake remained
at the recorded zero percent, and the G marker moved through `55.8/49.3`, `60.9/49.7`, `50.3/50.1`,
and `47.5/52.1` percent positions while `motionActive` stayed true.

The final recording used 15 fps screen capture to preserve headroom for the four selected WebRTC
videos while leaving every virtual Momo source at 50 fps and Marker detection at 25 Hz.

| Recording validation | Result |
| --- | ---: |
| Resolution / duration / frames | 1920x1080 / 120 s / 1,800 |
| File size | 84,865,677 bytes |
| Unique race orders | 12 |
| Order transitions | 12 |
| Cars with a position change | 8 |
| IMU telemetry sent | 68,120 |
| IMU telemetry dropped / send errors | 0 / 0 |
| Commands sent / received by virtual Momo | 120,343 / 120,401 |
| Command send errors | 0 |

Generated videos, normalized H.264 assets, raw captures, and measurement JSON remain under
`tools/.artifacts` and are intentionally excluded from Git.

Run the same test after placing the extracted CPU-shadow triplets in one directory:

```powershell
$replay = .\tools\New-CpuShadowFleetReplay.ps1 `
  -CaptureDirectory E:\recordings\cpu-shadow `
  -CarCount 20

.\tools\Start-VirtualFleetMapDemo.ps1 `
  -CarCount 20 `
  -DetectionHz 25 `
  -ReplayProfileManifestPath $replay.ManifestPath `
  -CommandReplayJsonl $replay.CommandReplayJsonl `
  -ReplayClipDurationSeconds 0

.\tools\Measure-VirtualFleetReplay.ps1 -DurationSeconds 120
```

## Notes

- Pace variants are valid re-encodes at fixed 50 fps; they do not change the Marker detector input
  cadence. They deliberately alter elapsed playback time to create relative race pace.
- Telemetry and command timestamps are aligned to replay time and keyframe starts, but the original
  browser MediaRecorder timestamps are not a camera hardware clock. This is a transport and race
  behavior simulation, not millisecond vehicle-dynamics ground truth.
- The source capture proves that the browser accepted the original command send. This test adds
  Relay forwarding and virtual Momo receipt, but cannot retroactively prove that the physical car
  applied the original recorded command.
- All sources, GPU detection, Relay, Race Control, browser rendering, and recording ran on one host.
  The result is deliberately harsher than a split-PC event setup but is not a network-loss test.
