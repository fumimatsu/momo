# Relay DataChannel text / binary diagnostic

## Status

`doing`: production operation keeps the Pilot telemetry and vehicle-event WebSocket path.
The diagnostic below is opt-in and is intended to identify the root cause on the affected
Pilot PC before changing that transport.

## Observed behavior

- One Pilot PC stopped receiving DataChannel responses after the recent integration.
- Another PC can connect to the same Relay host and source.
- The affected PC does not enter the 15-second no-video reconnect path.
- M5 audio continues to decode.
- Race state did not appear to update over its DataChannel.

M5 audio is an `AUD:` binary message on `momo-telemetry`. `TEL:`, `VHS:`, `PONG:`,
race state, and vehicle events are text messages. If M5 audio arrives while text does not,
ICE, DTLS, SCTP, and binary DataChannel delivery are established. The remaining boundary is
Pion `SendText` to the affected browser, or browser-side classification of the received type.

## Current workaround boundary

For a local Relay Pilot, text telemetry and vehicle events are currently delivered over the
signaling WebSocket. Binary audio remains on `momo-telemetry`. This is an unconditional
transport split, not evidence that DataChannel text delivery has recovered.

The diagnostic must therefore send its probes directly on `momo-telemetry`; it must not use
the normal telemetry broadcast function.

## Diagnostic contract

The Viewer enables the probe only when `dcTextProbe=1` is present. It sends a low-frequency
request over the existing signaling WebSocket:

```json
{"type":"datachannel-probe","data":"<opaque-token>"}
```

The Relay validates the token and sends the same UTF-8 payload twice on the requesting
Pilot's `momo-telemetry` DataChannel:

```text
DIAG:DC_TEXT_BINARY:<opaque-token>
```

The first transmission uses Pion `SendText`; the second uses binary `Send`. The request is
not forwarded to the vehicle Momo, Race Control, or the command path. The default is three
attempts at one-second intervals, bounded to ten attempts.

## Production test

Open the normal Relay Pilot URL on the affected PC and append:

```text
&dcTextProbe=1&dcTextProbeAttempts=5
```

Do the same on a known-good PC. After connection, run this in DevTools Console:

```js
JSON.stringify(fpvViewer.getDiagnostics().dataChannelTextProbe, null, 2)
```

Also retain the Relay log lines containing `DataChannel probe`. Each line records the token,
the local result of text and binary sends, and the Pion buffered amount.

## Interpretation

| Result | Interpretation |
| --- | --- |
| text and binary counts both match requests | DataChannel text delivery is working in that run |
| binary matches requests, text remains zero | Pion-to-browser text classification or text PPID interoperability is the primary boundary |
| both remain zero, Relay logged successful sends | Browser did not deliver either probe; inspect channel ID/state and WebRTC internals |
| Relay reports no telemetry channel | DataChannel negotiation/opening failed before payload delivery |
| request count is zero | The diagnostic URL or signaling WebSocket request did not run |

Race state is useful supporting evidence but is not a controlled probe because it may not
change during the capture window.

## Next decision

If only binary succeeds on the affected PC and both forms succeed on the control PC, preserve
DataChannel transport and evaluate a binary UTF-8 envelope for Relay-to-Viewer text messages.
Do not remove the WebSocket workaround until command, telemetry, race, events, and M5 audio
have all passed an end-to-end production test.

## Verification

- Viewer static tests confirm the opt-in URL, WebSocket request, binary/text classifier, and
  exported diagnostic snapshot.
- Relay unit tests confirm bounded ASCII probe-token validation.
- Runtime confirmation requires the affected PC and a known-good PC.
