# Pilot-local race callouts

Status: open

## Context

Per-car central synthesis does not scale cleanly to 20 or 30 Pilots. The Pilot
already receives authoritative position and `intervalToAheadMs` values, and the
Browser Kokoro runtime can generate personal audio locally.

## Goal

Add bounded, structured ahead/behind callouts without moving timing ownership
to the Viewer or accepting arbitrary speech text from a browser.

## Acceptance Criteria

- Viewer planner announces only useful changes and applies explicit cooldowns.
- Relay validates `race_audio_callout_request` and creates fixed Japanese and
  English templates.
- Relay enforces deduplication and a hard per-Pilot rate limit.
- Existing lap and finish paths are preserved.
- Viewer distribution and Relay tests pass.

## Verification

- `npm test` in `E:\src\momo-fpv-viewer`
- `npm run build:relay` and `npm run build:pages` in the Viewer repository
- `tools\Invoke-RelayTests.ps1` in `E:\src\momo`
- `go vet ./...` using `tools\Resolve-GoExecutable.ps1`
- Live race-state test with two or more cars is still required after automated
  verification.

## Notes

The central pre-race/global announcement bus is a separate server-level phase.
It must synthesize once per language and broadcast the same clip; it must not
reuse the current per-source detector to generate duplicate clips.

Automated status on 2026-08-19: Viewer 190/190, Race Audio Service 32/32,
Relay Go tests and `go vet` passed. The 32-request `/v1/prepare` burst completed
without errors in Japanese and English after increasing the HTTP accept queue
to 64.
