# Relay race-state scaling

Status: done

## Context

Race Control and Timing benchmarks showed acceptable 64-car processing, but Relay still transformed
the same complete race-state once per vehicle. Per-client race-state delivery pressure was not visible
in the operations status.

## Goal

Reduce redundant Relay JSON work without changing the Pilot contract, and expose enough runtime
evidence to decide whether a future delta protocol is justified.

## Acceptance Criteria

- A deterministic 1/4/20/32/64-source benchmark uses the production transform.
- Race state is decoded once per Race Control snapshot while preserving source-specific `viewerCarId`.
- Latest-only WebSocket replacement, send errors, bytes, message count, delivery age, and reliable
  DataChannel buffered bytes are observable per client.
- Relay tests and Go benchmark pass through the repository Go resolver.
- The result and remaining protocol boundary are documented.

## Verification

- `tools/Invoke-RelayTests.ps1`: pass.
- `go vet ./...`: pass.
- `BenchmarkRaceStateSourceFanout`, five one-second runs, one CPU: pass.
- 64-source median improved from 23.918 ms / 7,970,676 B / 5,068 allocations to
  11.650 ms / 5,357,719 B / 2,040 allocations.
- Distinct `viewerCarId` batch output and latest-state queue replacement have focused tests.

## Notes

The complete per-Pilot snapshot is intentionally retained. A live 15-minute run combining selected
video, browser render metrics, and Relay buffered-byte behavior is the next evidence gate before
designing a delta contract.
The Windows race detector is still blocked by the missing CGO C compiler; normal tests, vet, and the
deterministic benchmark do not require it.
