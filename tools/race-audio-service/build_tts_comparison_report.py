from __future__ import annotations

import argparse
import html
import json
from collections import defaultdict
from pathlib import Path


PROMPT_LABELS = {
    "lap_time": "LAP / time / position",
    "pilot_name": "Pilot name / personal best",
    "pit_service": "PIT service",
    "blue_flag": "Blue flag",
    "boost_ready": "Boost ready",
    "race_finish": "Race finish",
}

ENGLISH_COLUMNS = (
    ("Piper Plus / Tsukuyomi", "piper-plus", "en-US", "tsukuyomi"),
    ("Piper Plus CSS10", "piper-plus", "en-US", "css10"),
    ("Kokoro / af_heart", "kokoro", "en-US", "af_heart"),
    ("Kokoro / am_michael", "kokoro", "en-US", "am_michael"),
    ("Pocket / alba", "pocket-tts-2.1.0", "en-US", "alba"),
    ("Pocket / michael", "pocket-tts-2.1.0", "en-US", "michael"),
)

JAPANESE_COLUMNS = (
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
        f'RTF {float(result["realtime_factor"]):.3f}'
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
            }
        )
    return summary


def main() -> None:
    parser = argparse.ArgumentParser(description="Build a local TTS comparison report")
    parser.add_argument("directory", type=Path)
    args = parser.parse_args()
    directory = args.directory.resolve()
    results = load_results(directory)
    if not results:
        raise SystemExit(f"No result manifests found in {directory}")
    summary = summarize(results)
    (directory / "comparison-summary.json").write_text(
        json.dumps(summary, ensure_ascii=False, indent=2), encoding="utf-8"
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
    code {{ color: #f0c868; }}
    @media (max-width: 1100px) {{ body {{ padding: 12px; }} table {{ min-width: 1100px; }} section {{ overflow-x: auto; }} }}
  </style>
</head>
<body>
  <h1>Momo race audio TTS comparison</h1>
  <p>CPU comparison generated on 2026-08-16. Lower first-chunk and total values are faster. Listen for pilot names, numeric lap times, omitted words, and announcement tone.</p>
  {comparison_table("English", "Officially supported by both engines.", ENGLISH_COLUMNS, results)}
  {comparison_table("Japanese", "Pocket TTS 2.1.0 does not officially support Japanese. Its files are negative-control samples, not deployment candidates.", JAPANESE_COLUMNS, results)}
</body>
</html>
"""
    output = directory / "comparison.html"
    output.write_text(document, encoding="utf-8")
    print(output)


if __name__ == "__main__":
    main()
