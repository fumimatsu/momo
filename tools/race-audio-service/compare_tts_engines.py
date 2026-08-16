from __future__ import annotations

import argparse
import json
import re
import time
import wave
from dataclasses import asdict, dataclass
from pathlib import Path

import numpy as np


PROMPTS = (
    (
        "lap_time",
        "Lap four complete. Thirteen point seven one five seconds. Position two.",
        "4 \u5468\u76ee\u3092 13.715 \u79d2\u3067\u5b8c\u4e86\u3002\u73fe\u5728 2 \u4f4d\u3067\u3059\u3002",
    ),
    (
        "pilot_name",
        "Mad Max sets a new personal best. Thirteen point seven one five seconds.",
        "\u30de\u30c3\u30c9\u30de\u30c3\u30af\u30b9\u304c\u81ea\u5df1\u30d9\u30b9\u30c8\u3092\u66f4\u65b0\u300213.715 \u79d2\u3067\u3059\u3002",
    ),
    (
        "pit_service",
        "Car three is in the pit. Fuel and damage recovery are in progress.",
        "3 \u53f7\u8eca\u304c\u30d4\u30c3\u30c8\u30a4\u30f3\u3002\u71c3\u6599\u3068\u30c0\u30e1\u30fc\u30b8\u3092\u56de\u5fa9\u4e2d\u3067\u3059\u3002",
    ),
    (
        "blue_flag",
        "Blue flag. A faster car is approaching from behind.",
        "\u30d6\u30eb\u30fc\u30d5\u30e9\u30c3\u30b0\u3002\u5f8c\u65b9\u304b\u3089\u901f\u3044\u8eca\u4e21\u304c\u63a5\u8fd1\u3057\u3066\u3044\u307e\u3059\u3002",
    ),
    (
        "boost_ready",
        "Boost is ready. Shift up to activate.",
        "\u30d6\u30fc\u30b9\u30c8\u4f7f\u7528\u53ef\u80fd\u3002\u30b7\u30d5\u30c8\u30a2\u30c3\u30d7\u3067\u767a\u52d5\u3057\u307e\u3059\u3002",
    ),
    (
        "race_finish",
        "Race finished. Mad Max takes second place.",
        "\u30ec\u30fc\u30b9\u7d42\u4e86\u3002\u30de\u30c3\u30c9\u30de\u30c3\u30af\u30b9\u306f 2 \u4f4d\u3067\u3059\u3002",
    ),
)


@dataclass(frozen=True)
class Result:
    engine: str
    language: str
    voice: str
    prompt_id: str
    text: str
    output: str
    first_chunk_ms: int
    generation_ms: int
    audio_ms: int
    realtime_factor: float


def write_wave(path: Path, samples: np.ndarray, sample_rate: int) -> None:
    normalized = np.nan_to_num(np.asarray(samples, dtype=np.float32).reshape(-1))
    normalized = np.clip(normalized, -1.0, 1.0)
    pcm = np.round(normalized * 32767.0).astype("<i2")
    with wave.open(str(path), "wb") as output:
        output.setnchannels(1)
        output.setsampwidth(2)
        output.setframerate(sample_rate)
        output.writeframes(pcm.tobytes())


def file_part(value: str) -> str:
    return re.sub(r"[^a-zA-Z0-9_.-]+", "-", value).strip("-")


def prompts_for(language: str) -> list[tuple[str, str]]:
    index = 1 if language == "en-US" else 2
    return [(prompt[0], prompt[index]) for prompt in PROMPTS]


def make_result(
    engine: str,
    language: str,
    voice: str,
    prompt_id: str,
    text: str,
    output: Path,
    first_chunk_seconds: float,
    generation_seconds: float,
    audio_seconds: float,
) -> Result:
    return Result(
        engine=engine,
        language=language,
        voice=voice,
        prompt_id=prompt_id,
        text=text,
        output=output.name,
        first_chunk_ms=round(first_chunk_seconds * 1000),
        generation_ms=round(generation_seconds * 1000),
        audio_ms=round(audio_seconds * 1000),
        realtime_factor=round(generation_seconds / max(audio_seconds, 0.001), 3),
    )


def generate_kokoro(args: argparse.Namespace, output_dir: Path) -> tuple[list[Result], float]:
    from race_audio_service import KokoroEngine

    started = time.perf_counter()
    engine = KokoroEngine(
        Path(args.kokoro_model),
        Path(args.kokoro_voices),
        warm_up=False,
    )
    load_seconds = time.perf_counter() - started
    results: list[Result] = []
    for voice in args.voice:
        combined: list[np.ndarray] = []
        sample_rate = 24_000
        for prompt_id, text in prompts_for(args.language):
            started = time.perf_counter()
            samples, sample_rate = engine.synthesize(text, args.language, voice, args.speed)
            elapsed = time.perf_counter() - started
            output = output_dir / f"kokoro-{file_part(args.language)}-{file_part(voice)}-{prompt_id}.wav"
            write_wave(output, samples, sample_rate)
            audio_seconds = len(samples) / sample_rate
            results.append(
                make_result(
                    "kokoro",
                    args.language,
                    voice,
                    prompt_id,
                    text,
                    output,
                    elapsed,
                    elapsed,
                    audio_seconds,
                )
            )
            combined.extend((samples, np.zeros(sample_rate // 2, dtype=np.float32)))
        write_wave(
            output_dir / f"kokoro-{file_part(args.language)}-{file_part(voice)}-all.wav",
            np.concatenate(combined),
            sample_rate,
        )
    return results, load_seconds


def generate_pocket(args: argparse.Namespace, output_dir: Path) -> tuple[list[Result], float]:
    import torch
    from pocket_tts import TTSModel

    started = time.perf_counter()
    model = TTSModel.load_model(language="english")
    load_seconds = time.perf_counter() - started
    results: list[Result] = []
    language_label = args.language if args.language == "en-US" else "ja-JP-unsupported"
    for voice in args.voice:
        voice_state = model.get_state_for_audio_prompt(voice)
        combined: list[np.ndarray] = []
        for prompt_id, text in prompts_for(args.language):
            started = time.perf_counter()
            first_chunk_seconds = 0.0
            chunks = []
            for chunk in model.generate_audio_stream(voice_state, text):
                if not chunks:
                    first_chunk_seconds = time.perf_counter() - started
                chunks.append(chunk.detach().cpu())
            elapsed = time.perf_counter() - started
            if not chunks:
                raise RuntimeError(f"Pocket TTS returned no audio for {prompt_id}")
            samples = torch.cat(chunks).numpy().astype(np.float32)
            output = output_dir / f"pocket-{file_part(language_label)}-{file_part(voice)}-{prompt_id}.wav"
            write_wave(output, samples, model.sample_rate)
            audio_seconds = len(samples) / model.sample_rate
            results.append(
                make_result(
                    "pocket-tts-2.1.0",
                    language_label,
                    voice,
                    prompt_id,
                    text,
                    output,
                    first_chunk_seconds,
                    elapsed,
                    audio_seconds,
                )
            )
            combined.extend((samples, np.zeros(model.sample_rate // 2, dtype=np.float32)))
        write_wave(
            output_dir / f"pocket-{file_part(language_label)}-{file_part(voice)}-all.wav",
            np.concatenate(combined),
            model.sample_rate,
        )
    return results, load_seconds


def generate_voicevox(args: argparse.Namespace, output_dir: Path) -> tuple[list[Result], float]:
    from race_audio_service import VoicevoxEngine

    results: list[Result] = []
    for voice in args.voice:
        engine = VoicevoxEngine(args.voicevox_url, int(voice))
        combined: list[np.ndarray] = []
        sample_rate = 24_000
        for prompt_id, text in prompts_for(args.language):
            started = time.perf_counter()
            samples, sample_rate = engine.synthesize(text, args.language, voice, args.speed)
            elapsed = time.perf_counter() - started
            output = output_dir / f"voicevox-{file_part(args.language)}-speaker-{file_part(voice)}-{prompt_id}.wav"
            write_wave(output, samples, sample_rate)
            audio_seconds = len(samples) / sample_rate
            results.append(
                make_result(
                    "voicevox",
                    args.language,
                    f"speaker-{voice}",
                    prompt_id,
                    text,
                    output,
                    elapsed,
                    elapsed,
                    audio_seconds,
                )
            )
            combined.extend((samples, np.zeros(sample_rate // 2, dtype=np.float32)))
        write_wave(
            output_dir / f"voicevox-{file_part(args.language)}-speaker-{file_part(voice)}-all.wav",
            np.concatenate(combined),
            sample_rate,
        )
    return results, 0.0


def generate_piper_plus(args: argparse.Namespace, output_dir: Path) -> tuple[list[Result], float]:
    from race_audio_service import PiperPlusEngine

    started = time.perf_counter()
    engine = PiperPlusEngine(
        Path(args.piper_model),
        Path(args.piper_config),
        Path(args.piper_nltk_data),
        args.piper_length_scale,
    )
    load_seconds = time.perf_counter() - started
    results: list[Result] = []
    for voice in args.voice:
        combined: list[np.ndarray] = []
        sample_rate = 22_050
        for prompt_id, text in prompts_for(args.language):
            started = time.perf_counter()
            samples, sample_rate = engine.synthesize(text, args.language, voice, args.speed)
            elapsed = time.perf_counter() - started
            output = output_dir / (
                f"piper-plus-{file_part(args.language)}-{file_part(voice)}-{prompt_id}.wav"
            )
            write_wave(output, samples, sample_rate)
            audio_seconds = len(samples) / sample_rate
            results.append(
                make_result(
                    "piper-plus",
                    args.language,
                    voice,
                    prompt_id,
                    text,
                    output,
                    elapsed,
                    elapsed,
                    audio_seconds,
                )
            )
            combined.extend((samples, np.zeros(sample_rate // 2, dtype=np.float32)))
        write_wave(
            output_dir
            / f"piper-plus-{file_part(args.language)}-{file_part(voice)}-all.wav",
            np.concatenate(combined),
            sample_rate,
        )
    return results, load_seconds


def main() -> None:
    parser = argparse.ArgumentParser(description="Compare race announcement TTS engines")
    parser.add_argument(
        "--engine",
        choices=("kokoro", "pocket", "voicevox", "piper-plus"),
        required=True,
    )
    parser.add_argument("--language", choices=("en-US", "ja-JP"), required=True)
    parser.add_argument("--voice", action="append", required=True)
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--speed", type=float, default=1.04)
    parser.add_argument("--kokoro-model", default="models/kokoro-v1.0.onnx")
    parser.add_argument("--kokoro-voices", default="models/voices-v1.0.bin")
    parser.add_argument("--voicevox-url", default="http://127.0.0.1:50021")
    parser.add_argument("--piper-model", default="models/css10-ja-6lang-fp16.onnx")
    parser.add_argument("--piper-config", default="models/css10-ja-6lang-config.json")
    parser.add_argument("--piper-nltk-data", default="models/nltk_data")
    parser.add_argument("--piper-length-scale", type=float, default=1.2)
    args = parser.parse_args()
    args.output_dir.mkdir(parents=True, exist_ok=True)

    if args.engine == "kokoro":
        results, load_seconds = generate_kokoro(args, args.output_dir)
    elif args.engine == "pocket":
        results, load_seconds = generate_pocket(args, args.output_dir)
    elif args.engine == "piper-plus":
        results, load_seconds = generate_piper_plus(args, args.output_dir)
    else:
        results, load_seconds = generate_voicevox(args, args.output_dir)
    manifest = {
        "engine": args.engine,
        "language": args.language,
        "voices": args.voice,
        "modelLoadMs": round(load_seconds * 1000),
        "results": [asdict(result) for result in results],
    }
    manifest_path = args.output_dir / (
        f"results-{file_part(args.engine)}-{file_part(args.language)}-{'-'.join(map(file_part, args.voice))}.json"
    )
    manifest_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps(manifest, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
