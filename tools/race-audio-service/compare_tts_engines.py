from __future__ import annotations

import argparse
import concurrent.futures
import json
import re
import threading
import time
import wave
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any

import numpy as np

from tts_comparison_corpus import prompts_for


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
    gpu_baseline_mb: int = 0
    gpu_peak_mb: int = 0


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
    gpu_baseline_mb: int = 0,
    gpu_peak_mb: int = 0,
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
        gpu_baseline_mb=gpu_baseline_mb,
        gpu_peak_mb=gpu_peak_mb,
    )


def percentile(values: list[float], quantile: float) -> float:
    if not values:
        return 0.0
    return float(np.percentile(np.asarray(values, dtype=np.float64), quantile))


def summarize_results(results: list[Result]) -> dict[str, object]:
    first_chunk_ms = [float(result.first_chunk_ms) for result in results]
    generation_ms = [float(result.generation_ms) for result in results]
    realtime_factors = [float(result.realtime_factor) for result in results]
    speed_factors = [
        float(result.audio_ms) / max(float(result.generation_ms), 1.0)
        for result in results
    ]
    return {
        "samples": len(results),
        "firstChunkP50Ms": round(percentile(first_chunk_ms, 50)),
        "firstChunkP95Ms": round(percentile(first_chunk_ms, 95)),
        "generationP50Ms": round(percentile(generation_ms, 50)),
        "generationP95Ms": round(percentile(generation_ms, 95)),
        "realtimeFactorP50": round(percentile(realtime_factors, 50), 3),
        "realtimeFactorP95": round(percentile(realtime_factors, 95), 3),
        "speedFactorP50": round(percentile(speed_factors, 50), 3),
        "speedFactorP95": round(percentile(speed_factors, 95), 3),
        "gpuPeakMb": max((result.gpu_peak_mb for result in results), default=0),
    }


def qwen_engine_name(model_name: str) -> str:
    basename = model_name.rstrip("/\\").split("/")[-1].split("\\")[-1]
    normalized = re.sub(r"[^a-z0-9]+", "-", basename.lower()).strip("-")
    normalized = normalized.removeprefix("qwen3-tts-12hz-")
    return f"qwen3-tts-{normalized}"


def qwen_runtime_engine_name(model_name: str, backend: str) -> str:
    name = qwen_engine_name(model_name)
    return f"faster-{name}" if backend == "faster" else name


def qwen_language(language: str) -> str:
    return "English" if language == "en-US" else "Japanese"


def comparison_prompts(args: argparse.Namespace) -> list[tuple[str, str]]:
    prompts = prompts_for(args.language)
    return prompts if args.limit == 0 else prompts[: args.limit]


class QwenComparisonEngine:
    def __init__(
        self,
        model_name: str,
        device: str,
        dtype_name: str,
        attention: str,
        local_files_only: bool,
        backend: str,
        streaming: bool,
        max_new_tokens: int,
    ) -> None:
        import torch

        if not torch.cuda.is_available():
            raise RuntimeError("Qwen comparison requires a CUDA GPU")
        dtype = {
            "bfloat16": torch.bfloat16,
            "float16": torch.float16,
            "float32": torch.float32,
        }[dtype_name]
        self._torch = torch
        self._device = device
        self._lock = threading.Lock()
        self._backend = backend
        self._streaming = streaming and backend == "faster"
        self._max_new_tokens = max_new_tokens
        self.name = qwen_runtime_engine_name(model_name, backend)
        self.model_name = model_name
        if backend == "faster":
            from faster_qwen3_tts import FasterQwen3TTS

            self.model = FasterQwen3TTS.from_pretrained(
                model_name,
                device=device,
                dtype=dtype,
                attn_implementation=attention,
                local_files_only=local_files_only,
            )
        else:
            from qwen_tts import Qwen3TTSModel

            self.model = Qwen3TTSModel.from_pretrained(
                model_name,
                device_map=device,
                dtype=dtype,
                attn_implementation=attention,
                local_files_only=local_files_only,
            )

    def warmup(self, language: str, voice: str, seed: int) -> float:
        if self._backend != "faster":
            started = time.perf_counter()
            self.synthesize(
                "Radio ready." if language == "en-US" else "実況音声の準備ができました。",
                language,
                voice,
                seed,
            )
            return time.perf_counter() - started
        torch = self._torch
        started = time.perf_counter()
        with self._lock:
            torch.manual_seed(seed)
            torch.cuda.manual_seed_all(seed)
            torch.cuda.synchronize()
            self.model.warmup(prefill_len=100)
            torch.cuda.synchronize()
        self.synthesize(
            "Radio ready." if language == "en-US" else "実況音声の準備ができました。",
            language,
            voice,
            seed,
        )
        return time.perf_counter() - started

    def synthesize(
        self,
        text: str,
        language: str,
        voice: str,
        seed: int,
    ) -> tuple[np.ndarray, int, float, float, int, int]:
        torch = self._torch
        with self._lock:
            torch.manual_seed(seed)
            torch.cuda.manual_seed_all(seed)
            torch.cuda.synchronize()
            torch.cuda.reset_peak_memory_stats()
            baseline_mb = round(torch.cuda.memory_allocated() / (1024 * 1024))
            started = time.perf_counter()
            first_chunk_seconds = 0.0
            if self._streaming:
                chunks = []
                sample_rate = 24_000
                for chunk, sample_rate, _timing in self.model.generate_custom_voice_streaming(
                    text=text,
                    language=qwen_language(language),
                    speaker=voice,
                    instruct=None,
                    max_new_tokens=self._max_new_tokens,
                    chunk_size=8,
                ):
                    if not chunks:
                        torch.cuda.synchronize()
                        first_chunk_seconds = time.perf_counter() - started
                    chunks.append(np.asarray(chunk, dtype=np.float32).reshape(-1))
                if not chunks:
                    raise RuntimeError("Faster Qwen3-TTS returned no streaming audio")
                samples = np.concatenate(chunks)
            else:
                wavs, sample_rate = self.model.generate_custom_voice(
                    text=text,
                    language=qwen_language(language),
                    speaker=voice,
                    instruct=None,
                    non_streaming_mode=True,
                    max_new_tokens=self._max_new_tokens,
                )
                if not wavs:
                    raise RuntimeError("Qwen3-TTS returned no audio")
                samples = np.asarray(wavs[0], dtype=np.float32).reshape(-1)
            torch.cuda.synchronize()
            elapsed = time.perf_counter() - started
            if first_chunk_seconds == 0.0:
                first_chunk_seconds = elapsed
            peak_mb = round(torch.cuda.max_memory_allocated() / (1024 * 1024))
        return (
            samples,
            int(sample_rate),
            first_chunk_seconds,
            elapsed,
            baseline_mb,
            peak_mb,
        )


def run_qwen_burst(
    engine: QwenComparisonEngine,
    args: argparse.Namespace,
    output_dir: Path,
) -> dict[str, object] | None:
    burst_prompts = comparison_prompts(args)[: args.burst_size]
    if not burst_prompts:
        return None
    barrier = threading.Barrier(len(burst_prompts))

    def run_one(index: int, prompt_id: str, text: str) -> dict[str, object]:
        barrier.wait()
        client_started = time.perf_counter()
        (
            samples,
            sample_rate,
            first_chunk_seconds,
            generation_seconds,
            baseline_mb,
            peak_mb,
        ) = engine.synthesize(
            text,
            args.language,
            args.voice[0],
            args.seed + 10_000 + index,
        )
        client_seconds = time.perf_counter() - client_started
        output = output_dir / (
            f"{engine.name}-{file_part(args.language)}-{file_part(args.voice[0])}"
            f"-burst-{index + 1}-{prompt_id}.wav"
        )
        write_wave(output, samples, sample_rate)
        return {
            "promptId": prompt_id,
            "output": output.name,
            "clientMs": round(client_seconds * 1000),
            "firstChunkMs": round(first_chunk_seconds * 1000),
            "generationMs": round(generation_seconds * 1000),
            "audioMs": round(len(samples) * 1000 / sample_rate),
            "gpuBaselineMb": baseline_mb,
            "gpuPeakMb": peak_mb,
        }

    wall_started = time.perf_counter()
    with concurrent.futures.ThreadPoolExecutor(max_workers=len(burst_prompts)) as executor:
        futures = [
            executor.submit(run_one, index, prompt_id, text)
            for index, (prompt_id, text) in enumerate(burst_prompts)
        ]
        requests = [future.result() for future in futures]
    wall_ms = round((time.perf_counter() - wall_started) * 1000)
    client_values = [float(request["clientMs"]) for request in requests]
    return {
        "size": len(requests),
        "wallMs": wall_ms,
        "clientP50Ms": round(percentile(client_values, 50)),
        "clientP95Ms": round(percentile(client_values, 95)),
        "requests": requests,
    }


def generate_qwen(
    args: argparse.Namespace, output_dir: Path
) -> tuple[list[Result], float, float, dict[str, object] | None, dict[str, object]]:
    started = time.perf_counter()
    engine = QwenComparisonEngine(
        args.qwen_model,
        args.qwen_device,
        args.qwen_dtype,
        args.qwen_attention,
        args.qwen_local_files_only,
        args.qwen_backend,
        args.qwen_streaming,
        args.qwen_max_new_tokens,
    )
    load_seconds = time.perf_counter() - started
    warmup_seconds = engine.warmup(args.language, args.voice[0], args.seed)
    results: list[Result] = []
    for voice_index, voice in enumerate(args.voice):
        combined: list[np.ndarray] = []
        sample_rate = 24_000
        for prompt_index, (prompt_id, text) in enumerate(comparison_prompts(args)):
            (
                samples,
                sample_rate,
                first_chunk_seconds,
                elapsed,
                baseline_mb,
                peak_mb,
            ) = engine.synthesize(
                text,
                args.language,
                voice,
                args.seed + (voice_index * 1000) + prompt_index + 1,
            )
            output = output_dir / (
                f"{engine.name}-{file_part(args.language)}-{file_part(voice)}-{prompt_id}.wav"
            )
            write_wave(output, samples, sample_rate)
            audio_seconds = len(samples) / sample_rate
            results.append(
                make_result(
                    engine.name,
                    args.language,
                    voice,
                    prompt_id,
                    text,
                    output,
                    first_chunk_seconds,
                    elapsed,
                    audio_seconds,
                    baseline_mb,
                    peak_mb,
                )
            )
            combined.extend((samples, np.zeros(sample_rate // 2, dtype=np.float32)))
        write_wave(
            output_dir / f"{engine.name}-{file_part(args.language)}-{file_part(voice)}-all.wav",
            np.concatenate(combined),
            sample_rate,
        )
    burst = run_qwen_burst(engine, args, output_dir) if args.burst_size else None
    runtime = {
        "model": engine.model_name,
        "device": args.qwen_device,
        "dtype": args.qwen_dtype,
        "attention": args.qwen_attention,
        "backend": args.qwen_backend,
        "streaming": engine._streaming,
        "maxNewTokens": args.qwen_max_new_tokens,
        "batchOutput": not engine._streaming,
    }
    return results, load_seconds, warmup_seconds, burst, runtime


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
        for prompt_id, text in comparison_prompts(args):
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
        for prompt_id, text in comparison_prompts(args):
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
        for prompt_id, text in comparison_prompts(args):
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
        for prompt_id, text in comparison_prompts(args):
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
        choices=("kokoro", "pocket", "voicevox", "piper-plus", "qwen3"),
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
    parser.add_argument(
        "--qwen-model",
        default="Qwen/Qwen3-TTS-12Hz-0.6B-CustomVoice",
    )
    parser.add_argument("--qwen-device", default="cuda:0")
    parser.add_argument(
        "--qwen-dtype",
        choices=("bfloat16", "float16", "float32"),
        default="bfloat16",
    )
    parser.add_argument("--qwen-attention", choices=("sdpa", "eager"), default="sdpa")
    parser.add_argument("--qwen-backend", choices=("upstream", "faster"), default="upstream")
    parser.add_argument("--qwen-streaming", action="store_true")
    parser.add_argument("--qwen-max-new-tokens", type=int, default=256)
    parser.add_argument("--qwen-local-files-only", action="store_true")
    parser.add_argument("--burst-size", type=int, choices=range(0, 9), default=0)
    parser.add_argument("--limit", type=int, choices=range(0, 21), default=0)
    parser.add_argument("--seed", type=int, default=20260819)
    args = parser.parse_args()
    args.output_dir.mkdir(parents=True, exist_ok=True)

    warmup_seconds = 0.0
    burst = None
    runtime: dict[str, Any] = {}
    if args.engine == "kokoro":
        results, load_seconds = generate_kokoro(args, args.output_dir)
    elif args.engine == "qwen3":
        results, load_seconds, warmup_seconds, burst, runtime = generate_qwen(
            args, args.output_dir
        )
    elif args.engine == "pocket":
        results, load_seconds = generate_pocket(args, args.output_dir)
    elif args.engine == "piper-plus":
        results, load_seconds = generate_piper_plus(args, args.output_dir)
    else:
        results, load_seconds = generate_voicevox(args, args.output_dir)
    manifest_engine = results[0].engine if results else args.engine
    manifest = {
        "engine": manifest_engine,
        "language": args.language,
        "voices": args.voice,
        "modelLoadMs": round(load_seconds * 1000),
        "warmupMs": round(warmup_seconds * 1000),
        "summary": summarize_results(results),
        "burst": burst,
        "runtime": runtime,
        "results": [asdict(result) for result in results],
    }
    manifest_path = args.output_dir / (
        f"results-{file_part(manifest_engine)}-{file_part(args.language)}-"
        f"{'-'.join(map(file_part, args.voice))}.json"
    )
    manifest_path.write_text(json.dumps(manifest, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps(manifest, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
