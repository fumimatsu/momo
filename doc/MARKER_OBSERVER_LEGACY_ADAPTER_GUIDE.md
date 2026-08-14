# Marker Observer Legacy Adapter Guide

## Current scope

The first integration slice moves ArUco image recognition outside MADSYSTEM while keeping
MADSYSTEM as the timing engine. It is intentionally local and reversible.

```text
recorded source x 4
  -> NVDEC + GPU DICT_4X4_50 detection at 50 Hz
  -> Local\MomoMarkerObservationsV1
  -> MadsObserverBridge.dll
  -> ArUcoWebCamMulti external mode
  -> existing checkpoint, PIT, pilot assignment, and Race Control publisher paths
```

The existing `Local\MomoObserverFrameV1` composite video mapping remains independent and
continues to supply MADSYSTEM visuals. Marker observation IPC does not carry video and does
not create or modify a control DataChannel.

This slice does not yet consume live Relay/WebRTC sources and does not emit qualified
checkpoint or PIT events. MADSYSTEM still owns temporal confirmation, passage semantics,
lap/sector state, and Race Control API publication.

## Replay producer

Initialize the existing GPU validation environment once, then run four simulated sources:

```powershell
.\tools\Initialize-ArucoCapacity.ps1 -IncludeNvCodec
.\tools\Run-GpuMarkerObserverReplay.ps1 `
  -DurationSeconds 30 `
  -OutputPath .\tools\.artifacts\marker-observer\replay.json
```

With no `-Sources`, the wrapper reuses the validated 960x528/50 FPS recording as
`sim-01..sim-04`. Use repeated `SOURCE_ID=VIDEO_PATH` values to select other inputs:

```powershell
.\tools\Run-GpuMarkerObserverReplay.ps1 -Sources @(
  'car-a=E:\recordings\car-a.mp4',
  'car-b=E:\recordings\car-b.mp4'
)
```

IDs 17, 34, and 37 are excluded from the operational allowlist. Multiple physical markers
with the same ID remain separate observations.

## IPC contract

- mapping: `Local\MomoMarkerObservationsV1`
- mutex: `Local\MomoMarkerObservationsV1-Mutex`
- producer lease: `Local\MomoMarkerObservationsV1-Producer`; a second producer is rejected
- magic/version: `MMO1` / 1
- bounded ring: 16 batches
- maximum sources: 32
- maximum detections per source and frame: 16
- source observation: source ID/index, source and batch sequence, frame/detection timestamps,
  video-valid flag, candidate count, repeated marker IDs, normalized center and area

The consumer reads batches in sequence. If it falls behind the 16-slot ring, it resumes at
the oldest retained batch and reports the number dropped. Producer PID/creation changes or
sequence rollback reset the reader cursor, so restarting the producer does not strand the
consumer on an old sequence. Marker observations are tiny and ordered; video remains a
latest-frame path.

## MADSYSTEM modes

MADSYSTEM defaults to `Internal`, so deployment of the new DLL alone does not change race
behavior. Set `MADSYSTEM_MARKER_OBSERVER_MODE` before starting MADSYSTEM:

| Value | Behavior |
| --- | --- |
| `internal` | Current OpenCV recognition and timing behavior |
| `shadow` | Current recognition remains authoritative; external per-source ID counts are compared once per second on mismatch |
| `external` | GPU observations replace the internal detection result, then enter the existing MADSYSTEM checkpoint/PIT/assignment path |

An invalid external video source clears its pending checkpoint evidence instead of treating
video loss as marker exit. A valid frame with no marker continues through the existing exit
confirmation logic.

## Promotion order

1. Run replay producer plus native smoke reader and confirm 50 Hz with zero drops.
2. Run MADSYSTEM in `shadow` with the same four live feeds and record mismatch plus timing
   logs without changing race results.
3. Compare CPU/GPU marker instances, passage outcomes, PIT presence, and video timestamps.
4. Run `external` in a non-race test and verify checkpoint order, duplicate suppression,
   PIT IN/OUT, pilot assignment, and Race Control snapshots.
5. Replace replay inputs with live Relay/WebRTC inputs while retaining the same IPC.
6. Only after parity, move temporal qualification to Marker Observer and define a Reliable
   marker-event ingest contract. Do not combine this with the live-input migration.

The USB/Webcam composite replacement is a separate, later path and is not part of this
FPVRC/WebRTC sequence.
