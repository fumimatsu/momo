from __future__ import annotations

import argparse
import datetime
import html
import json
from collections import defaultdict
from pathlib import Path

import numpy as np

from tts_comparison_corpus import PROMPT_LABELS

ENGLISH_COLUMNS = (
    ("Qwen3 0.6B / Ryan", "qwen3-tts-0-6b-customvoice", "en-US", "Ryan"),
    ("Faster Qwen3 1.7B / Ryan", "faster-qwen3-tts-1-7b-customvoice", "en-US", "Ryan"),
    ("Piper Plus / Tsukuyomi", "piper-plus", "en-US", "tsukuyomi"),
    ("Piper Plus CSS10", "piper-plus", "en-US", "css10"),
    ("Kokoro / af_heart", "kokoro", "en-US", "af_heart"),
    ("Kokoro / am_michael", "kokoro", "en-US", "am_michael"),
    ("Kokoro / jf_alpha", "kokoro", "en-US", "jf_alpha"),
    ("Pocket / alba", "pocket-tts-2.1.0", "en-US", "alba"),
    ("Pocket / michael", "pocket-tts-2.1.0", "en-US", "michael"),
)

JAPANESE_COLUMNS = (
    ("Qwen3 0.6B / Ono_Anna", "qwen3-tts-0-6b-customvoice", "ja-JP", "Ono_Anna"),
    ("Faster Qwen3 1.7B / Ono_Anna", "faster-qwen3-tts-1-7b-customvoice", "ja-JP", "Ono_Anna"),
    ("Piper Plus / Tsukuyomi", "piper-plus", "ja-JP", "tsukuyomi"),
    ("Piper Plus CSS10", "piper-plus", "ja-JP", "css10"),
    ("Kokoro / jf_alpha", "kokoro", "ja-JP", "jf_alpha"),
    ("VOICEVOX / Zundamon normal", "voicevox", "ja-JP", "speaker-3"),
    ("Pocket / alba (unsupported)", "pocket-tts-2.1.0", "ja-JP-unsupported", "alba"),
)


def load_results(directory: Path) -> dict[tuple[str, str, str, str], dict[str, object]]:
    results: dict[tuple[str, str, str, str], dict[str, object]] = {}
    for path in directory.glob("results-*.json"):
        payload = json.loads(path.read_text(encoding="utf-8"))
        for result in payload.get("results", []):
            key = (
                str(result["engine"]),
                str(result["language"]),
                str(result["voice"]),
                str(result["prompt_id"]),
            )
            results[key] = result
    return results


def load_bursts(directory: Path) -> list[dict[str, object]]:
    bursts: list[dict[str, object]] = []
    for path in directory.glob("results-*.json"):
        payload = json.loads(path.read_text(encoding="utf-8"))
        burst = payload.get("burst")
        if not isinstance(burst, dict):
            continue
        baselines = {
            str(result["prompt_id"]): result
            for result in payload.get("results", [])
        }
        requests = []
        for request in burst.get("requests", []):
            item = dict(request)
            baseline = baselines.get(str(item.get("promptId", "")))
            baseline_audio_ms = int(baseline["audio_ms"]) if baseline else 0
            ratio = (
                float(item.get("audioMs", 0)) / baseline_audio_ms
                if baseline_audio_ms > 0
                else 0.0
            )
            item["baselineAudioMs"] = baseline_audio_ms
            item["audioDurationRatio"] = round(ratio, 2)
            item["suspectedDurationOutlier"] = ratio >= 2.0
            requests.append(item)
        bursts.append(
            {
                "engine": str(payload.get("engine", "unknown")),
                "language": str(payload.get("language", "unknown")),
                "voice": ", ".join(str(value) for value in payload.get("voices", [])),
                "size": int(burst.get("size", 0)),
                "wallMs": int(burst.get("wallMs", 0)),
                "clientP50Ms": int(burst.get("clientP50Ms", 0)),
                "clientP95Ms": int(burst.get("clientP95Ms", 0)),
                "requests": requests,
            }
        )
    return sorted(bursts, key=lambda item: (str(item["engine"]), str(item["language"])))


def audio_cell(result: dict[str, object] | None) -> str:
    if not result:
        return '<td class="missing">Not generated</td>'
    output = html.escape(str(result["output"]), quote=True)
    text = html.escape(str(result["text"]))
    return (
        "<td>"
        f'<audio controls preload="none" src="{output}"></audio>'
        f'<p class="text">{text}</p>'
        '<p class="metrics">'
        f'first {int(result["first_chunk_ms"])} ms / '
        f'total {int(result["generation_ms"])} ms / '
        f'audio {int(result["audio_ms"])} ms / '
        f'RTF {float(result["realtime_factor"]):.3f} / '
        f'GPU peak {int(result.get("gpu_peak_mb", 0))} MB'
        "</p></td>"
    )


def comparison_table(
    title: str,
    note: str,
    columns: tuple[tuple[str, str, str, str], ...],
    results: dict[tuple[str, str, str, str], dict[str, object]],
) -> str:
    header = "".join(f"<th>{html.escape(column[0])}</th>" for column in columns)
    rows = []
    for prompt_id, label in PROMPT_LABELS.items():
        cells = []
        for _, engine, language, voice in columns:
            cells.append(audio_cell(results.get((engine, language, voice, prompt_id))))
        rows.append(f"<tr><th>{html.escape(label)}</th>{''.join(cells)}</tr>")
    return (
        f"<section><h2>{html.escape(title)}</h2><p>{html.escape(note)}</p>"
        f"<table><thead><tr><th>Announcement</th>{header}</tr></thead>"
        f"<tbody>{''.join(rows)}</tbody></table></section>"
    )


def summarize(results: dict[tuple[str, str, str, str], dict[str, object]]) -> list[dict[str, object]]:
    grouped: dict[tuple[str, str, str], list[dict[str, object]]] = defaultdict(list)
    for (engine, language, voice, _), result in results.items():
        grouped[(engine, language, voice)].append(result)
    summary = []
    for (engine, language, voice), items in sorted(grouped.items()):
        summary.append(
            {
                "engine": engine,
                "language": language,
                "voice": voice,
                "samples": len(items),
                "averageFirstChunkMs": round(
                    sum(int(item["first_chunk_ms"]) for item in items) / len(items)
                ),
                "averageGenerationMs": round(
                    sum(int(item["generation_ms"]) for item in items) / len(items)
                ),
                "averageAudioMs": round(sum(int(item["audio_ms"]) for item in items) / len(items)),
                "averageRealtimeFactor": round(
                    sum(float(item["realtime_factor"]) for item in items) / len(items), 3
                ),
                "firstChunkP50Ms": round(
                    float(np.percentile([int(item["first_chunk_ms"]) for item in items], 50))
                ),
                "firstChunkP95Ms": round(
                    float(np.percentile([int(item["first_chunk_ms"]) for item in items], 95))
                ),
                "generationP50Ms": round(
                    float(np.percentile([int(item["generation_ms"]) for item in items], 50))
                ),
                "generationP95Ms": round(
                    float(np.percentile([int(item["generation_ms"]) for item in items], 95))
                ),
                "speedFactorP50": round(
                    float(
                        np.percentile(
                            [
                                int(item["audio_ms"]) / max(int(item["generation_ms"]), 1)
                                for item in items
                            ],
                            50,
                        )
                    ),
                    3,
                ),
                "gpuPeakMb": max(int(item.get("gpu_peak_mb", 0)) for item in items),
            }
        )
    return summary


def burst_table(bursts: list[dict[str, object]]) -> str:
    if not bursts:
        return ""
    rows = []
    for burst in bursts:
        for request in burst["requests"]:
            outlier = bool(request["suspectedDurationOutlier"])
            row_class = ' class="outlier"' if outlier else ""
            output = html.escape(str(request["output"]), quote=True)
            rows.append(
                f"<tr{row_class}>"
                f'<td>{html.escape(str(burst["language"]))}</td>'
                f'<td>{html.escape(str(request["promptId"]))}</td>'
                f'<td><audio controls preload="none" src="{output}"></audio></td>'
                f'<td>{int(request["clientMs"]):,}</td>'
                f'<td>{int(request.get("firstChunkMs", request["generationMs"])):,}</td>'
                f'<td>{int(request["generationMs"]):,}</td>'
                f'<td>{int(request["audioMs"]):,}</td>'
                f'<td>{float(request["audioDurationRatio"]):.2f}x</td>'
                f'<td>{"duration outlier" if outlier else "-"}</td>'
                "</tr>"
            )
    summary_rows = "".join(
        "<li>"
        f'{html.escape(str(burst["language"]))}: wall {int(burst["wallMs"]):,} ms, '
        f'client P50 {int(burst["clientP50Ms"]):,} ms, '
        f'client P95 {int(burst["clientP95Ms"]):,} ms'
        "</li>"
        for burst in bursts
    )
    return (
        "<section><h2>Four-request burst</h2>"
        "<p>The comparison worker accepts simultaneous arrivals but serializes one model instance. "
        "A duration ratio of 2.0x or greater against the same sequential prompt is flagged for "
        "possible repetition or unstable generation.</p>"
        f"<ul>{summary_rows}</ul>"
        "<table><thead><tr><th>Language</th><th>Prompt</th><th>Audio</th>"
        "<th>Client ms</th><th>First chunk ms</th><th>Generation ms</th><th>Audio ms</th>"
        "<th>vs baseline</th><th>Flag</th></tr></thead>"
        f"<tbody>{''.join(rows)}</tbody></table></section>"
    )


def main() -> None:
    parser = argparse.ArgumentParser(description="Build a local TTS comparison report")
    parser.add_argument("directory", type=Path)
    args = parser.parse_args()
    directory = args.directory.resolve()
    results = load_results(directory)
    if not results:
        raise SystemExit(f"No result manifests found in {directory}")
    summary = summarize(results)
    bursts = load_bursts(directory)
    (directory / "comparison-summary.json").write_text(
        json.dumps(summary, ensure_ascii=False, indent=2), encoding="utf-8"
    )
    (directory / "comparison-burst-summary.json").write_text(
        json.dumps(bursts, ensure_ascii=False, indent=2), encoding="utf-8"
    )
    document = f"""<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width,initial-scale=1">
  <title>Momo race audio TTS comparison</title>
  <style>
    :root {{ color-scheme: dark; font-family: Segoe UI, sans-serif; background: #080a0c; color: #f1f5f7; }}
    body {{ margin: 0; padding: 24px; }}
    h1 {{ margin: 0 0 8px; font-size: 28px; }}
    h2 {{ margin-top: 32px; color: #78d7e8; }}
    p {{ color: #aebac0; }}
    table {{ width: 100%; border-collapse: collapse; table-layout: fixed; background: #0d1114; }}
    th, td {{ border: 1px solid #2c3439; padding: 10px; vertical-align: top; }}
    th {{ text-align: left; color: #dce7eb; background: #11171b; }}
    thead th:first-child, tbody th {{ width: 150px; }}
    audio {{ width: 100%; min-width: 180px; }}
    .text {{ min-height: 42px; margin: 8px 0; color: #e4ebee; font-size: 13px; }}
    .metrics {{ margin: 0; color: #7ec8a5; font: 12px Consolas, monospace; }}
    .missing {{ color: #e48076; }}
    .outlier td {{ background: #3a1715; }}
    code {{ color: #f0c868; }}
    @media (max-width: 1100px) {{ body {{ padding: 12px; }} table {{ min-width: 1100px; }} section {{ overflow-x: auto; }} }}
  </style>
</head>
<body>
  <h1>Momo race audio TTS comparison</h1>
  <p>Generated {datetime.datetime.now().astimezone().isoformat(timespec="seconds")}. Lower first-chunk and total values are faster. Upstream Qwen returns batch audio in this comparison; Faster Qwen reports the first streamed chunk separately. Listen for pilot names, numeric lap times, omitted or repeated words, and announcement tone.</p>
  {comparison_table("English", "Officially supported by both engines.", ENGLISH_COLUMNS, results)}
  {comparison_table("Japanese", "Pocket TTS 2.1.0 does not officially support Japanese. Its files are negative-control samples, not deployment candidates.", JAPANESE_COLUMNS, results)}
  {burst_table(bursts)}
</body>
</html>
"""
    output = directory / "comparison.html"
    output.write_text(document, encoding="utf-8")
    print(output)


if __name__ == "__main__":
    main()
