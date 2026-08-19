from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import math
import statistics
import threading
import time
from urllib import request


def parse_levels(value: str) -> list[int]:
    levels = [int(item.strip()) for item in value.split(",") if item.strip()]
    if not levels or any(level < 1 or level > 128 for level in levels):
        raise argparse.ArgumentTypeError("levels must contain integers from 1 through 128")
    return levels


def percentile(values: list[float], fraction: float) -> float:
    ordered = sorted(values)
    index = max(0, min(len(ordered) - 1, math.ceil(len(ordered) * fraction) - 1))
    return ordered[index]


def post_prompt(url: str, token: str, payload: bytes, barrier: threading.Barrier | None) -> float:
    if barrier is not None:
        barrier.wait(timeout=10)
    headers = {"Content-Type": "application/json"}
    if token:
        headers["Authorization"] = f"Bearer {token}"
    started = time.perf_counter()
    with request.urlopen(request.Request(url, data=payload, headers=headers), timeout=15) as response:
        body = json.load(response)
        if response.status != 200 or not body.get("modelInputIds"):
            raise RuntimeError(f"invalid prepare response: HTTP {response.status}")
    return (time.perf_counter() - started) * 1000


def run_level(base_url: str, token: str, language: str, voice: str, level: int) -> dict[str, object]:
    barrier = threading.Barrier(level) if level > 1 else None
    payloads = [
        json.dumps(
            {
                "eventKey": f"prepare-burst-{level}-{index}",
                "language": language,
                "voice": voice,
                "text": "前、7号車、差0.6" if language == "ja-JP" else "Car 7 ahead. Gap 0 point six seconds",
                "speed": 1.04,
            }
        ).encode("utf-8")
        for index in range(level)
    ]
    started = time.perf_counter()
    errors: list[str] = []
    latencies: list[float] = []
    with ThreadPoolExecutor(max_workers=level) as executor:
        futures = [
            executor.submit(post_prompt, f"{base_url}/v1/prepare", token, payload, barrier)
            for payload in payloads
        ]
        for future in futures:
            try:
                latencies.append(future.result())
            except Exception as error:  # noqa: BLE001 - benchmark must report every worker failure.
                errors.append(str(error))
    wall_ms = (time.perf_counter() - started) * 1000
    return {
        "concurrency": level,
        "requests": level,
        "errors": errors,
        "wallMs": round(wall_ms, 2),
        "meanMs": round(statistics.fmean(latencies), 2) if latencies else None,
        "p50Ms": round(percentile(latencies, 0.50), 2) if latencies else None,
        "p95Ms": round(percentile(latencies, 0.95), 2) if latencies else None,
        "maxMs": round(max(latencies), 2) if latencies else None,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description="Measure concurrent /v1/prepare latency.")
    parser.add_argument("--url", default="http://127.0.0.1:18090")
    parser.add_argument("--token", default="")
    parser.add_argument("--levels", type=parse_levels, default=parse_levels("1,4,8,16,32"))
    parser.add_argument("--language", choices=("en-US", "ja-JP"), default="ja-JP")
    args = parser.parse_args()
    voice = "jf_alpha" if args.language == "ja-JP" else "am_michael"
    base_url = args.url.rstrip("/")

    warmup = json.dumps(
        {
            "eventKey": "prepare-burst-warmup",
            "language": args.language,
            "voice": voice,
            "text": "準備完了" if args.language == "ja-JP" else "Ready",
            "speed": 1.04,
        }
    ).encode("utf-8")
    post_prompt(f"{base_url}/v1/prepare", args.token, warmup, None)
    results = [run_level(base_url, args.token, args.language, voice, level) for level in args.levels]
    report = {
        "url": base_url,
        "language": args.language,
        "voice": voice,
        "results": results,
    }
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 1 if any(result["errors"] for result in results) else 0


if __name__ == "__main__":
    raise SystemExit(main())
