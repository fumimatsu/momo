from __future__ import annotations

import json
import re

from build_tts_comparison_report import load_bursts
from compare_tts_engines import (
    Result,
    qwen_engine_name,
    qwen_language,
    qwen_runtime_engine_name,
    summarize_results,
)
from tts_comparison_corpus import PROMPT_LABELS, PROMPTS, prompts_for


def test_comparison_corpus_has_twenty_bilingual_prompts() -> None:
    assert len(PROMPTS) == 20
    assert len(PROMPT_LABELS) == 20
    assert len(prompts_for("en-US")) == 20
    assert len(prompts_for("ja-JP")) == 20
    assert {prompt[0] for prompt in PROMPTS} == set(PROMPT_LABELS)
    assert all(prompt[1].isascii() for prompt in PROMPTS)
    assert all(not prompt[2].isascii() for prompt in PROMPTS)


def test_japanese_prompts_use_tts_safe_number_spacing_and_endings() -> None:
    japanese_prompts = [prompt[2] for prompt in PROMPTS]
    counter_spacing = re.compile(r"\d(?:\.\d+)?\s+(?:周目|秒|位|号車|灯|周|台)")
    assert all(counter_spacing.search(prompt) is None for prompt in japanese_prompts)
    assert all(not prompt.endswith("。") for prompt in japanese_prompts)
    assert dict(prompts_for("ja-JP"))["lap_time"] == "4周目、13.715"


def test_qwen_model_and_language_names_are_stable() -> None:
    assert (
        qwen_engine_name("Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice")
        == "qwen3-tts-0-6b-customvoice"
    )
    assert (
        qwen_engine_name(r"E:\models\Qwen3-TTS-12Hz-1.7B-CustomVoice")
        == "qwen3-tts-1-7b-customvoice"
    )
    assert qwen_language("en-US") == "English"
    assert qwen_language("ja-JP") == "Japanese"
    assert (
        qwen_runtime_engine_name("Qwen/Qwen3-TTS-12Hz-1.7B-CustomVoice", "faster")
        == "faster-qwen3-tts-1-7b-customvoice"
    )


def test_result_summary_reports_percentiles_and_peak_gpu() -> None:
    results = [
        Result(
            engine="qwen3-tts-0-6b-customvoice",
            language="en-US",
            voice="Ryan",
            prompt_id=f"sample-{index}",
            text="sample",
            output=f"sample-{index}.wav",
            first_chunk_ms=value,
            generation_ms=value,
            audio_ms=1000,
            realtime_factor=value / 1000,
            gpu_baseline_mb=1800,
            gpu_peak_mb=peak,
        )
        for index, (value, peak) in enumerate(((100, 1900), (200, 2000), (300, 2100), (400, 2200)))
    ]

    summary = summarize_results(results)

    assert summary == {
        "samples": 4,
        "firstChunkP50Ms": 250,
        "firstChunkP95Ms": 385,
        "generationP50Ms": 250,
        "generationP95Ms": 385,
        "realtimeFactorP50": 0.25,
        "realtimeFactorP95": 0.385,
        "speedFactorP50": 4.167,
        "speedFactorP95": 9.25,
        "gpuPeakMb": 2200,
    }


def test_burst_report_flags_audio_duration_growth(tmp_path) -> None:
    manifest = {
        "engine": "qwen3-tts-0-6b-customvoice",
        "language": "en-US",
        "voices": ["Ryan"],
        "results": [
            {
                "prompt_id": "pilot_name",
                "audio_ms": 7000,
            }
        ],
        "burst": {
            "size": 1,
            "wallMs": 30000,
            "clientP50Ms": 30000,
            "clientP95Ms": 30000,
            "requests": [
                {
                    "promptId": "pilot_name",
                    "output": "burst.wav",
                    "clientMs": 30000,
                    "generationMs": 20000,
                    "audioMs": 15000,
                }
            ],
        },
    }
    (tmp_path / "results-qwen.json").write_text(json.dumps(manifest), encoding="utf-8")

    bursts = load_bursts(tmp_path)

    request = bursts[0]["requests"][0]
    assert request["baselineAudioMs"] == 7000
    assert request["audioDurationRatio"] == 2.14
    assert request["suspectedDurationOutlier"] is True
