# Race Recorder Server

## Status

- Operations Console control boundary: implemented in `momo-race-timing`
- dedicated Recorder process: implemented for `full_archive`
- `program_only`: fail-closed until Program Director output registration exists
- physical M5 audio and four-source venue validation: pending
- MP4/MKV mux and archive packaging: pending

## Boundary

Recording is one Operations Console function but not one in-process component. Coordinator owns the
per-run policy and sends authenticated start/stop commands. `momo-race-recorder.exe` owns media
subscriptions and files. The processes may run on the same PC and are launched separately so storage
or mux failure cannot block timing, race control, or operator commands.

`off` creates no Recorder subscription. `full_archive` creates one read-only Relay Observer
connection for each source locked at Prepare. Recorder receives the already encoded H.264 RTP stream
and does not decode or re-encode video. It also receives `AUD:1` on `momo-telemetry`, decodes the 8 kHz
IMA ADPCM frames, and writes a mono PCM WAV sidecar. Recorder never opens a command or drive channel.

## Start

Create a random token containing at least 32 characters and store it outside the repository.

```powershell
$secretDirectory = Join-Path $env:LOCALAPPDATA 'MomoFPV\secrets'
New-Item -ItemType Directory -Path $secretDirectory -Force | Out-Null
[IO.File]::WriteAllText(
  (Join-Path $secretDirectory 'race-recorder-token.txt'),
  '<replace-with-random-token>'
)

Set-Location E:\src\momo
.\tools\Start-RaceRecorder.ps1 -Rebuild
```

Configure the Coordinator process with `recorderBaseUrl` set to `http://127.0.0.1:8792`, and expose
the same token to Coordinator as `MOMO_RACE_RECORDER_TOKEN`. The token is never sent to a browser.
The default Recorder API is loopback-only. A dedicated Recorder PC requires an explicit management
LAN/Tailscale listen address and matching firewall restriction; do not expose this HTTP API publicly.

## Storage

The default root is `C:\fpv-recordings`. A run uses an exact, path-safe `raceRunId` directory:

```text
C:\fpv-recordings\<raceRunId>\
  manifest.json
  sources\<sourceId>\
    video-0001.h264
    video-0002.h264
    m5-audio.wav
    m5-audio-events.ndjson
```

H.264 segment rotation waits for the next IDR, so the configured duration is a target rather than an
exact boundary. Every segment starts with an IDR and remains independently decodable. The manifest
records run/source identity, first media offsets, frame and byte totals, detected packet loss, audio
sequence gaps, and segment timing. Raw H.264 has no container timestamps; use the manifest timing for
later mux. Do not present these files as final synchronized MP4 output yet.

An attempt that fails before the start response is committed is retained under `_failed` and removed
from the active `raceRunId` path. An explicit Operations retry with a new command ID can therefore
make a new attempt without overwriting or deleting the failure evidence. A completed run directory is
never reused.

Recorder requires the configured free-space reserve before creating a run. A missing source, missing
IDR before the default four-second start timeout, unsafe identity, reused run directory, command conflict, or storage
failure is explicit. Recorder never silently lowers `full_archive` to another mode.

## Verified PoC

On 2026-08-28, one 960x528 H.264 virtual Momo source at 50 fps was passed through the real Relay and
recorded through the production WebRTC signaling path. Recorder completed
`recording -> stopping -> ready`, stored 881 frames in four IDR-aligned segments with zero detected
packet loss, and every segment was recognized by ffprobe as H.264 960x528. The virtual source did not
contain M5 audio, so only an empty valid WAV header was expected in this test.

## Remaining Gates

1. Record one real car and confirm non-empty WAV speech/vehicle audio, sequence gaps, and A/V offsets.
2. Record four real sources while monitoring Relay egress, disk throughput, packet loss, and Pilot FPS.
3. Implement deterministic MP4/MKV mux using the manifest/RTP timing without video re-encoding.
4. Register Program Director output identity, then enable and test `program_only`.
5. Add Recorder binaries and launch metadata to the coordinated Windows release package.
