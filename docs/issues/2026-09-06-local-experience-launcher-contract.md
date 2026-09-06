# Local Experience launcher contract

- Status: done
- Updated: 2026-09-06

## Context

Timing commit `224eeec` passes `SkipObserver` and `WaitForMappingSeconds` to Momo scripts.
The published Momo launchers did not declare these parameters and silently ignored them. As a
result Native Observer restarted with visual output, and the GPU observer used a 20-second
Mapping wait instead of the requested 120 seconds.

## Goal

Make the existing Local Experience launcher arguments effective and reject unknown options before
starting processes. Keep regular Native Observer startup available for its existing callers.

## Acceptance Criteria

- [x] `SkipObserver` needs no Native Observer executable or WER registry write.
- [x] `SkipObserver` with `RestartObserver` stops Native Observer without launching it again.
- [x] Standalone Pilot and Marker Receiver processes are not stopped by that combination.
- [x] Relay startup still works while Native Observer is skipped.
- [x] GPU Mapping wait is passed exactly, including zero and fractional seconds.
- [x] Invalid wait values and unknown options fail before process effects.
- [x] Windows Relay validation includes the launcher regression test.

## Verification

- `tools/Test-ObserverLaunchers.ps1`: 16 cases passed with synthetic files and intercepted process
  and registry operations. No device, GPU, or live service was used.
- `tools/Invoke-RelayTests.ps1`: passed with Go 1.26.5, including the Relay Go suite and all
  16 launcher cases. Changed PowerShell scripts also passed parser validation.
- Physical Mapping startup, camera recognition, and live process replacement remain unverified by
  these local tests. This change does not complete the field qualification gates.

## Notes

Update Momo `master` and Timing `main` together. Timing owns the stopped-Coordinator recovery fixes
and its `docs/issues/2026-09-06-local-race-operations-recovery.md` records that work. No Viewer
distribution or Momo C++ source changes are required for these launcher fixes.
