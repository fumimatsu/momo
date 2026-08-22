#!/usr/bin/env python3
"""Wait until an MLY2 topology has the requested fresh video sources."""

from __future__ import annotations

import argparse
import sys
import time

from MarkerLumaV2 import MAX_SOURCES, open_reader


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--mapping-name", default=r"Local\MomoMarkerLumaV2")
    parser.add_argument("--required-source-count", type=int, required=True)
    parser.add_argument("--timeout-seconds", type=float, default=120.0)
    parser.add_argument("--maximum-frame-age-ms", type=float, default=1000.0)
    parser.add_argument("--stable-seconds", type=float, default=2.0)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    if not 0 <= args.required_source_count <= MAX_SOURCES:
        parser.error(f"--required-source-count must be in 0..{MAX_SOURCES}")
    if args.timeout_seconds <= 0 or args.maximum_frame_age_ms < 0:
        parser.error("timeout must be positive and frame age must not be negative")
    if args.stable_seconds < 0:
        parser.error("--stable-seconds must not be negative")

    started_at = time.monotonic()
    deadline = started_at + args.timeout_seconds
    stable_started_at: float | None = None
    last_status_at = 0.0
    with open_reader(args.mapping_name, args.timeout_seconds) as reader:
        while time.monotonic() < deadline:
            now = time.monotonic()
            topology = reader.read_topology()
            active = 0
            if topology is not None:
                sample_qpc = reader.query_performance_counter()
                for snapshot in reader.read_sources(topology):
                    if snapshot is None or not snapshot.connected or not snapshot.video_valid:
                        continue
                    age_ms = (
                        (sample_qpc - snapshot.received_qpc)
                        * 1000.0
                        / topology.qpc_frequency
                    )
                    if 0 <= age_ms <= args.maximum_frame_age_ms:
                        active += 1
                ready = (
                    len(topology.source_ids) == args.required_source_count
                    and active == args.required_source_count
                )
            else:
                ready = False

            if ready:
                if stable_started_at is None:
                    stable_started_at = now
                if now - stable_started_at >= args.stable_seconds:
                    print(
                        f"MLY2 ready: sources={args.required_source_count} "
                        f"elapsed={now - started_at:.2f}s",
                        flush=True,
                    )
                    return 0
            else:
                stable_started_at = None

            if now - last_status_at >= 1.0:
                configured = len(topology.source_ids) if topology else 0
                print(
                    f"Waiting for MLY2: configured={configured} "
                    f"active={active} required={args.required_source_count}",
                    flush=True,
                )
                last_status_at = now
            time.sleep(0.1)

    print(
        f"MLY2 did not reach {args.required_source_count} fresh sources "
        f"within {args.timeout_seconds:g}s",
        file=sys.stderr,
    )
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
