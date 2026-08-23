# Relay race-state scale validation

## Purpose

Race Control has one authenticated WebSocket connection to Relay. Relay then:

1. publishes one common `RACE:` snapshot to `/ws/race-state` for Team/Program Observer;
2. adds the source-specific `viewerCarId` and publishes it to each Pilot path.

This document measures the second step. It does not measure video, marker detection, browser DOM work,
or network/TLS overhead.

## Fixed fixture

- 1, 4, 20, 32, and 64 vehicle sources;
- 20 completed laps per car;
- three sector values per standing and lap;
- complete `race_state` v2 snapshots;
- up to 64 total lap-history rows, matching the Race Control retention limit.

The benchmark is deterministic and uses the production `raceMessagesForCars` function.

```powershell
$go = & .\tools\Resolve-GoExecutable.ps1
Push-Location .\tools\momo-relay
try {
  & $go test . `
    -run '^$' `
    -bench '^BenchmarkRaceStateSourceFanout$' `
    -benchmem `
    -count 5 `
    -benchtime 1s `
    -cpu 1
} finally {
  Pop-Location
}
```

## Finding and change

The former path decoded the same complete JSON once per vehicle and then encoded a vehicle-specific
copy. The current path decodes once per accepted Race Control snapshot and only repeats the required
per-vehicle encoding. `viewerCarId` and the `RACE:` wire format are unchanged.

Measurements below are medians of five one-second runs on the same Ryzen 7 9700X host. The baseline
was captured immediately before the production change.

| Sources | Baseline time | Current time | Change | Baseline bytes/op | Current bytes/op | Baseline allocs/op | Current allocs/op |
|---:|---:|---:|---:|---:|---:|---:|---:|
| 1 | 44.138 us | 47.401 us | +7.4% | 15,153 | 15,169 | 79 | 80 |
| 4 | 505.813 us | 321.264 us | -36.5% | 171,474 | 127,702 | 316 | 173 |
| 20 | 3.908 ms | 1.930 ms | -50.6% | 1,287,500 | 864,297 | 1,582 | 670 |
| 32 | 7.906 ms | 3.854 ms | -51.3% | 2,649,890 | 1,808,610 | 2,532 | 1,044 |
| 64 | 23.918 ms | 11.650 ms | -51.3% | 7,970,676 | 5,357,719 | 5,068 | 2,040 |

At the maximum 5 Hz Race Control cadence, the 64-source transformation falls from about 12.0% to
5.8% of one CPU core and from about 39.9 MB/s to 26.8 MB/s of temporary allocation. The one-source
result is effectively unchanged and is dominated by benchmark/runtime variation.

## Runtime evidence

`GET /api/v1/status` now reports the following per downstream client:

- `lastRaceDeliveryAgeMs`;
- `raceMessages` and `raceBytes`;
- `raceSendErrors`;
- `raceQueueReplacements` for latest-only WebSocket delivery;
- `raceBufferedBytes` for the reliable race DataChannel.

Operations Dashboard displays these as `RACE <age> Q<replacement> E<error> B<buffered>`.
Queue replacement is not itself a race-state error: the queue intentionally retains only the newest
complete snapshot. A continually increasing replacement count or buffered byte count means the
downstream consumer cannot keep up and needs investigation.

## Remaining boundary

Relay must still encode and send one complete snapshot per Pilot because `viewerCarId` is currently
inside each payload. At 64 cars and 5 Hz this is approximately 12 MiB/s of application payload before
WebSocket/DataChannel and network overhead. Do not replace this with deltas until runtime metrics show
actual pressure: a delta contract also needs reconnect snapshots, sequence-gap recovery, and a
connection-scoped viewer identity.

Viewer source already has synthetic 4/32/64/100-car fixtures and short-run render samplers. Its recorded
100-car, 10 Hz Team Observer result stayed below 4.2 ms maximum overview render time with no long task,
so it does not currently outrank Relay delivery as a bottleneck. The next browser gate is a 15-minute
live Relay run with up to four selected videos while recording these Relay buffer metrics and the
existing Viewer render metrics together.

The Windows race detector remains unavailable on this host because no working CGO C compiler is
configured. This change does not depend on race-detector instrumentation: normal Relay tests, focused
queue/identity tests, `go vet ./...`, and the single-CPU deterministic benchmark all pass. A race-enabled
soak test remains a separate toolchain task.
