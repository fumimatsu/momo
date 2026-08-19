from __future__ import annotations

import argparse
import hashlib
import json
import time
import wave
from collections.abc import Mapping
from datetime import UTC, datetime
from pathlib import Path

import numpy as np

from race_audio_service import (
    KOKORO_JAPANESE_TERMINAL_POLICY,
    KokoroEngine,
    apply_kokoro_japanese_pronunciation_dictionary,
    load_kokoro_japanese_pronunciation_dictionary,
    normalize_kokoro_japanese_terminal_phonemes,
)
from tts_comparison_corpus import prompts_for


BROWSER_MODEL_ID = "onnx-community/Kokoro-82M-v1.0-ONNX"
BROWSER_RUNTIME = "kokoro-js@1.2.1"
PROFILE_DEFAULTS = {
    "ja-JP": {
        "label": "日本語",
        "voice": "jf_alpha",
        "pythonRuntime": "kokoro-onnx+misaki-ja-pyopenjtalk",
    },
    "en-US": {
        "label": "English",
        "voice": "am_michael",
        "pythonRuntime": "kokoro-onnx+espeak-ng",
    },
}


def file_sha256(path: Path) -> str:
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def write_wave(path: Path, samples: np.ndarray, sample_rate: int) -> str:
    normalized = np.nan_to_num(np.asarray(samples, dtype=np.float32).reshape(-1))
    normalized = np.clip(normalized, -1.0, 1.0)
    pcm = np.round(normalized * 32767.0).astype("<i2")
    with wave.open(str(path), "wb") as output:
        output.setnchannels(1)
        output.setsampwidth(2)
        output.setframerate(sample_rate)
        output.writeframes(pcm.tobytes())
    return hashlib.sha256(pcm.tobytes()).hexdigest()


def export_fixture(
    engine: KokoroEngine,
    output_dir: Path,
    model_path: Path,
    voices_path: Path,
    speed: float,
    languages: list[str],
    voices: dict[str, str] | None = None,
    prompt_limit: int = 0,
    japanese_pronunciations: Mapping[str, str] | None = None,
) -> dict[str, object]:
    output_dir.mkdir(parents=True, exist_ok=True)
    selected_voices = voices or {}
    selected_pronunciations = japanese_pronunciations or {}
    profiles: list[dict[str, object]] = []

    for language in languages:
        defaults = PROFILE_DEFAULTS[language]
        voice = selected_voices.get(language, str(defaults["voice"]))
        results: list[dict[str, object]] = []
        prompts = prompts_for(language)
        if prompt_limit:
            prompts = prompts[:prompt_limit]
        for prompt_id, text in prompts:
            phoneme_batches = engine.phoneme_batches(text, language)
            if len(phoneme_batches) != 1:
                raise ValueError(
                    f"browser fixture prompt {language}/{prompt_id} produced "
                    f"{len(phoneme_batches)} phoneme batches"
                )
            raw_phonemes = phoneme_batches[0]
            phonemes = raw_phonemes
            if language == "ja-JP":
                phonemes = apply_kokoro_japanese_pronunciation_dictionary(
                    engine, text, phonemes, selected_pronunciations
                )
                phonemes = normalize_kokoro_japanese_terminal_phonemes(phonemes)
            started = time.perf_counter()
            if language == "ja-JP":
                samples, sample_rate = engine.synthesize_phonemes(phonemes, voice, speed)
            else:
                samples, sample_rate = engine.synthesize(text, language, voice, speed)
            generation_ms = round((time.perf_counter() - started) * 1000)
            language_slug = language.lower().replace("-", "_")
            output_name = f"python-{language_slug}-{prompt_id}.wav"
            pcm_sha256 = write_wave(output_dir / output_name, samples, sample_rate)
            result = {
                    "promptId": prompt_id,
                    "text": text,
                    "phonemes": phonemes,
                    "modelInputIds": engine.model_input_ids(phonemes),
                    "referenceWav": output_name,
                    "sampleRate": sample_rate,
                    "sampleCount": int(samples.size),
                    "audioMs": round(samples.size * 1000 / sample_rate),
                    "generationMs": generation_ms,
                    "pcm16Sha256": pcm_sha256,
            }
            if language == "ja-JP":
                result["rawPhonemes"] = raw_phonemes
                result["phonemeTransform"] = KOKORO_JAPANESE_TERMINAL_POLICY
            results.append(result)
        profile = {
                "id": language,
                "label": defaults["label"],
                "language": language,
                "voice": voice,
                "speed": speed,
                "inputMode": "phonemes",
                "pythonRuntime": defaults["pythonRuntime"],
                "prompts": results,
        }
        if language == "ja-JP":
            profile["phonemePolicy"] = KOKORO_JAPANESE_TERMINAL_POLICY
            profile["pronunciationOverrides"] = sorted(selected_pronunciations)
        profiles.append(profile)

    manifest: dict[str, object] = {
        "schemaVersion": 2,
        "generatedAt": datetime.now(UTC).isoformat(),
        "browserRuntime": BROWSER_RUNTIME,
        "browserModelId": BROWSER_MODEL_ID,
        "pythonModel": {
            "name": model_path.name,
            "sha256": file_sha256(model_path),
        },
        "pythonVoices": {
            "name": voices_path.name,
            "sha256": file_sha256(voices_path),
        },
        "profiles": profiles,
    }
    (output_dir / "manifest.json").write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )
    return manifest


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Export Japanese and English Python references for the browser Kokoro lab."
    )
    parser.add_argument("output_dir", type=Path)
    parser.add_argument("--kokoro-model", type=Path, default=Path("models/kokoro-v1.0.onnx"))
    parser.add_argument("--kokoro-voices", type=Path, default=Path("models/voices-v1.0.bin"))
    parser.add_argument("--language", action="append", choices=tuple(PROFILE_DEFAULTS))
    parser.add_argument("--japanese-voice", default="jf_alpha")
    parser.add_argument("--english-voice", default="am_michael")
    parser.add_argument("--speed", type=float, default=1.04)
    parser.add_argument("--limit", type=int, default=0)
    parser.add_argument(
        "--japanese-pronunciation-dictionary",
        type=Path,
        default=Path("japanese_pronunciation_dictionary.json"),
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if not 0.5 <= args.speed <= 2.0:
        raise ValueError("speed must be between 0.5 and 2.0")
    if args.limit < 0:
        raise ValueError("limit must be zero or greater")
    languages = list(dict.fromkeys(args.language or ["ja-JP", "en-US"]))
    japanese_pronunciations = load_kokoro_japanese_pronunciation_dictionary(
        args.japanese_pronunciation_dictionary
    )
    engine = KokoroEngine(
        args.kokoro_model,
        args.kokoro_voices,
        japanese_pronunciations=japanese_pronunciations,
    )
    manifest = export_fixture(
        engine,
        args.output_dir,
        args.kokoro_model,
        args.kokoro_voices,
        args.speed,
        languages,
        {"ja-JP": args.japanese_voice, "en-US": args.english_voice},
        args.limit,
        japanese_pronunciations,
    )
    print(json.dumps({
        "output": str(args.output_dir.resolve()),
        "profiles": [
            {"language": profile["language"], "prompts": len(profile["prompts"])}
            for profile in manifest["profiles"]
        ],
    }))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
