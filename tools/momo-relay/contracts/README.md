# Momo Race Contract

This directory is the canonical machine-readable contract for MADSYSTEM timing ingest and Relay/Web Observer race distribution.

- `timing-snapshot-v2.schema.json`: MADSYSTEM to Race Control request.
- `race-state-v2.schema.json`: Race Control to Relay/Web clients state.
- `fixtures/sector-progress.*.json`: equivalent in-progress sector state. Sector 2 intentionally has `lastMs` without `bestMs`; a completed split is available before the lap-level personal best is finalized.

Run `tools/sync-race-contracts.ps1` from this repository to refresh the vendored copies in `momo-relay` and `MADSYSTEM`.
