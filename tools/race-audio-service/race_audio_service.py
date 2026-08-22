from __future__ import annotations

import argparse
import base64
import hashlib
import hmac
import ipaddress
import json
import math
import os
import re
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import wave
from collections.abc import Mapping
from dataclasses import dataclass
from fractions import Fraction
from http import HTTPStatus
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Protocol

import av
import numpy as np


PROTOCOL_VERSION = 1
OPUS_CLOCK_RATE = 48_000
OPUS_CHANNELS = 1
FRAME_DURATION_MS = 20
FRAME_SAMPLES = OPUS_CLOCK_RATE * FRAME_DURATION_MS // 1000
MAX_REQUEST_BYTES = 32 * 1024
MAX_TEXT_LENGTH = 512
PIPER_TEXT_NORMALIZER_VERSION = 1
PIPER_PROSODY_PIPELINE_VERSION = 1
KOKORO_JAPANESE_TERMINAL_POLICY = "strip-misaki-terminal-prosody-and-append-period-v1"
KOKORO_BROWSER_MODEL_ID = "onnx-community/Kokoro-82M-v1.0-ONNX"


_NUMBER_PATTERN = re.compile(r"\d+(?:\.\d+)?")
_KOKORO_JAPANESE_TERMINAL_PROSODY_PATTERN = re.compile(r"[_^\-][j^_\-]*$")
_ENGLISH_SMALL_NUMBERS = (
    "zero",
    "one",
    "two",
    "three",
    "four",
    "five",
    "six",
    "seven",
    "eight",
    "nine",
    "ten",
    "eleven",
    "twelve",
    "thirteen",
    "fourteen",
    "fifteen",
    "sixteen",
    "seventeen",
    "eighteen",
    "nineteen",
)
_ENGLISH_TENS = (
    "",
    "",
    "twenty",
    "thirty",
    "forty",
    "fifty",
    "sixty",
    "seventy",
    "eighty",
    "ninety",
)
_JAPANESE_DIGITS = ("ゼロ", "一", "二", "三", "四", "五", "六", "七", "八", "九")


def strip_kokoro_japanese_terminal_prosody(phonemes: str) -> str:
    return _KOKORO_JAPANESE_TERMINAL_PROSODY_PATTERN.sub("", phonemes).rstrip()


def normalize_kokoro_japanese_terminal_phonemes(phonemes: str) -> str:
    normalized = strip_kokoro_japanese_terminal_prosody(phonemes)
    if not normalized:
        raise ValueError("Japanese phonemes are empty after terminal normalization")
    if normalized[-1] not in ".,!?":
        normalized += "."
    return normalized


def load_kokoro_japanese_pronunciation_dictionary(path: Path) -> dict[str, str]:
    payload = json.loads(path.read_text(encoding="utf-8"))
    if payload.get("schemaVersion") != 1 or not isinstance(payload.get("entries"), list):
        raise ValueError("unsupported Japanese pronunciation dictionary")
    result: dict[str, str] = {}
    for entry in payload["entries"]:
        if not isinstance(entry, dict):
            raise ValueError("Japanese pronunciation entry must be an object")
        surface = entry.get("surface")
        phonemes = entry.get("phonemes")
        if not isinstance(surface, str) or not surface.strip():
            raise ValueError("Japanese pronunciation surface is required")
        if not isinstance(phonemes, str) or not phonemes.strip():
            raise ValueError(f"Japanese pronunciation phonemes are required: {surface}")
        if surface in result:
            raise ValueError(f"duplicate Japanese pronunciation surface: {surface}")
        result[surface] = phonemes.strip()
    return result


def apply_kokoro_japanese_pronunciation_dictionary(
    engine: "KokoroEngine",
    text: str,
    phonemes: str,
    pronunciations: Mapping[str, str],
) -> str:
    adjusted = phonemes
    for surface, replacement in pronunciations.items():
        if surface not in text:
            continue
        source_batches = engine.phoneme_batches(surface, "ja-JP")
        if len(source_batches) != 1:
            raise ValueError(f"Japanese pronunciation surface produced multiple batches: {surface}")
        source = strip_kokoro_japanese_terminal_prosody(source_batches[0])
        if source not in adjusted:
            raise ValueError(f"Japanese pronunciation surface was not found in prompt phonemes: {surface}")
        adjusted = adjusted.replace(source, replacement)
    return adjusted


def _english_integer_words(value: int) -> str:
    if value < 20:
        return _ENGLISH_SMALL_NUMBERS[value]
    if value < 100:
        tens, ones = divmod(value, 10)
        return _ENGLISH_TENS[tens] + (f" {_ENGLISH_SMALL_NUMBERS[ones]}" if ones else "")
    for unit, label in (
        (1_000_000_000, "billion"),
        (1_000_000, "million"),
        (1_000, "thousand"),
        (100, "hundred"),
    ):
        if value >= unit:
            quotient, remainder = divmod(value, unit)
            words = f"{_english_integer_words(quotient)} {label}"
            return words + (f" {_english_integer_words(remainder)}" if remainder else "")
    raise ValueError(f"unsupported English integer: {value}")


def _japanese_under_10000(value: int) -> str:
    parts: list[str] = []
    for unit, label in ((1_000, "千"), (100, "百"), (10, "十")):
        digit, value = divmod(value, unit)
        if digit:
            if digit > 1:
                parts.append(_JAPANESE_DIGITS[digit])
            parts.append(label)
    if value:
        parts.append(_JAPANESE_DIGITS[value])
    return "".join(parts)


def _japanese_integer_words(value: int) -> str:
    if value == 0:
        return _JAPANESE_DIGITS[0]
    parts: list[str] = []
    for unit, label in ((100_000_000, "億"), (10_000, "万")):
        group, value = divmod(value, unit)
        if group:
            parts.extend((_japanese_under_10000(group), label))
    if value:
        parts.append(_japanese_under_10000(value))
    return "".join(parts)


def _normalize_piper_text(text: str, language: str) -> str:
    def replace(match: re.Match[str]) -> str:
        whole, separator, fraction = match.group(0).partition(".")
        if language == "en-US":
            normalized = _english_integer_words(int(whole))
            if separator:
                normalized += " point " + " ".join(
                    _ENGLISH_SMALL_NUMBERS[int(digit)] for digit in fraction
                )
            return normalized
        normalized = _japanese_integer_words(int(whole))
        if separator:
            normalized += "点" + "".join(_JAPANESE_DIGITS[int(digit)] for digit in fraction)
        return normalized

    return _NUMBER_PATTERN.sub(replace, text)


class AudioEngine(Protocol):
    @property
    def identity(self) -> str: ...

    def synthesize(
        self, text: str, language: str, voice: str, speed: float
    ) -> tuple[np.ndarray, int]: ...


class FixtureEngine:
    @property
    def identity(self) -> str:
        return "fixture-v1"

    def synthesize(
        self, text: str, language: str, voice: str, speed: float
    ) -> tuple[np.ndarray, int]:
        del language, voice
        sample_rate = 24_000
        duration = max(0.3, min(1.2, len(text) / 80.0)) / speed
        points = np.arange(math.ceil(sample_rate * duration), dtype=np.float32)
        envelope = np.minimum(1.0, points / (sample_rate * 0.02))
        envelope *= np.minimum(1.0, (len(points) - points) / (sample_rate * 0.04))
        samples = np.sin(points * (2.0 * math.pi * 523.25 / sample_rate)) * envelope * 0.16
        return samples.astype(np.float32), sample_rate


class KokoroEngine:
    def __init__(
        self,
        model_path: Path,
        voices_path: Path,
        warm_up_voice: str = "am_michael",
        warm_up_japanese_voice: str = "jf_alpha",
        warm_up: bool = True,
        japanese_pronunciations: Mapping[str, str] | None = None,
    ) -> None:
        from kokoro_onnx import Kokoro

        if not model_path.is_file():
            raise FileNotFoundError(f"Kokoro model not found: {model_path}")
        if not voices_path.is_file():
            raise FileNotFoundError(f"Kokoro voices not found: {voices_path}")
        self._model_path = model_path.resolve()
        self._voices_path = voices_path.resolve()
        self._model = Kokoro(str(self._model_path), str(self._voices_path))
        from misaki import ja

        self._japanese_g2p = ja.JAG2P(version="pyopenjtalk")
        self._japanese_pronunciations = dict(japanese_pronunciations or {})
        pronunciation_digest = hashlib.sha256(
            json.dumps(
                self._japanese_pronunciations,
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
            ).encode("utf-8")
        ).hexdigest()[:16]
        self._identity = (
            f"kokoro-onnx:{self._model_path.name}:{self._voices_path.name}:"
            f"{KOKORO_JAPANESE_TERMINAL_POLICY}:dictionary-{pronunciation_digest}"
        )
        self._lock = threading.Lock()
        if warm_up:
            with self._lock:
                self._model.create(
                    "Radio ready.",
                    voice=warm_up_voice,
                    speed=1.04,
                    lang="en-us",
                )
                japanese_phonemes = self.prepare_japanese_phonemes(
                    "実況音声の準備ができました。"
                )
                self._model.create(
                    japanese_phonemes,
                    voice=warm_up_japanese_voice,
                    speed=1.04,
                    is_phonemes=True,
                )

    @property
    def identity(self) -> str:
        return self._identity

    def phonemize_japanese(self, text: str) -> str:
        phonemes, _tokens = self._japanese_g2p(text)
        if not phonemes or "❓" in phonemes:
            raise ValueError("Kokoro Japanese G2P could not phonemize the text")
        return phonemes

    def prepare_japanese_phonemes(self, text: str) -> str:
        phonemes = apply_kokoro_japanese_pronunciation_dictionary(
            self,
            text,
            self.phonemize_japanese(text),
            self._japanese_pronunciations,
        )
        return normalize_kokoro_japanese_terminal_phonemes(phonemes)

    def prepare_browser_prompt(
        self, text: str, language: str, voice: str, speed: float
    ) -> dict[str, object]:
        if language == "ja-JP":
            phoneme_batches = list(self._model._split_phonemes(
                self.prepare_japanese_phonemes(text)
            ))
        elif language == "en-US":
            phoneme_batches = self.english_phoneme_batches(text)
        else:
            raise ValueError(f"unsupported Kokoro phoneme language: {language}")
        if len(phoneme_batches) != 1:
            raise ValueError("browser Kokoro prompt must fit in one phoneme batch")
        phonemes = phoneme_batches[0]
        return {
            "version": PROTOCOL_VERSION,
            "engine": "kokoro",
            "modelId": KOKORO_BROWSER_MODEL_ID,
            "language": language,
            "voice": voice,
            "speed": speed,
            "phonemes": phonemes,
            "modelInputIds": self.model_input_ids(phonemes),
            "phonemePolicy": (
                KOKORO_JAPANESE_TERMINAL_POLICY if language == "ja-JP" else "espeak-ng"
            ),
        }

    def japanese_phoneme_batches(self, text: str) -> list[str]:
        return list(self._model._split_phonemes(self.phonemize_japanese(text)))

    def english_phoneme_batches(self, text: str) -> list[str]:
        phonemes = self._model.tokenizer.phonemize(text, "en-us")
        if not phonemes:
            raise ValueError("Kokoro English G2P could not phonemize the text")
        return list(self._model._split_phonemes(phonemes))

    def phoneme_batches(self, text: str, language: str) -> list[str]:
        if language == "ja-JP":
            return self.japanese_phoneme_batches(text)
        if language == "en-US":
            return self.english_phoneme_batches(text)
        raise ValueError(f"unsupported Kokoro phoneme language: {language}")

    def model_input_ids(self, phonemes: str) -> list[int]:
        return [0, *map(int, self._model.tokenizer.tokenize(phonemes)), 0]

    def synthesize_phonemes(
        self, phonemes: str, voice: str, speed: float
    ) -> tuple[np.ndarray, int]:
        if not phonemes:
            raise ValueError("Kokoro phonemes are required")
        with self._lock:
            samples, sample_rate = self._model.create(
                phonemes,
                voice=voice,
                speed=speed,
                is_phonemes=True,
            )
        return np.asarray(samples, dtype=np.float32), int(sample_rate)

    def synthesize(
        self, text: str, language: str, voice: str, speed: float
    ) -> tuple[np.ndarray, int]:
        if language == "ja-JP":
            return self.synthesize_phonemes(
                self.prepare_japanese_phonemes(text), voice, speed
            )
        with self._lock:
            samples, sample_rate = self._model.create(
                text,
                voice=voice,
                speed=speed,
                lang="en-us",
            )
        return np.asarray(samples, dtype=np.float32), int(sample_rate)


class VoicevoxEngine:
    def __init__(self, base_url: str, speaker: int) -> None:
        parsed = urllib.parse.urlparse(base_url)
        if parsed.scheme not in {"http", "https"} or not parsed.netloc:
            raise ValueError(f"invalid VOICEVOX URL: {base_url}")
        self._base_url = base_url.rstrip("/")
        self._speaker = speaker

    @property
    def identity(self) -> str:
        return f"voicevox:{self._base_url}:speaker-{self._speaker}"

    def synthesize(
        self, text: str, language: str, voice: str, speed: float
    ) -> tuple[np.ndarray, int]:
        del language, voice
        query_url = (
            f"{self._base_url}/audio_query?"
            + urllib.parse.urlencode({"text": text, "speaker": self._speaker})
        )
        with urllib.request.urlopen(
            urllib.request.Request(query_url, method="POST"), timeout=10
        ) as response:
            query = json.loads(response.read())
        query["speedScale"] = float(speed)
        query["outputSamplingRate"] = 24_000
        query["outputStereo"] = False
        synthesis_url = (
            f"{self._base_url}/synthesis?"
            + urllib.parse.urlencode({"speaker": self._speaker})
        )
        request = urllib.request.Request(
            synthesis_url,
            data=json.dumps(query, ensure_ascii=False).encode("utf-8"),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(request, timeout=20) as response:
            return decode_pcm_wave(response.read())


class PiperPlusEngine:
    def __init__(
        self,
        model_path: Path,
        config_path: Path,
        nltk_data_path: Path,
        length_scale: float,
        warm_up: bool = True,
    ) -> None:
        if not model_path.is_file():
            raise FileNotFoundError(f"Piper Plus model not found: {model_path}")
        if not config_path.is_file():
            raise FileNotFoundError(f"Piper Plus config not found: {config_path}")
        if not 0.5 <= length_scale <= 2.0:
            raise ValueError("Piper Plus length scale must be between 0.5 and 2.0")

        nltk_data_path = nltk_data_path.resolve()
        os.environ["NLTK_DATA"] = str(nltk_data_path)
        import nltk

        if str(nltk_data_path) not in nltk.data.path:
            nltk.data.path.insert(0, str(nltk_data_path))
        for resource in (
            "taggers/averaged_perceptron_tagger",
            "taggers/averaged_perceptron_tagger_eng",
            "corpora/cmudict",
        ):
            try:
                nltk.data.find(resource)
            except LookupError as error:
                raise FileNotFoundError(
                    f"Piper Plus English resource is missing: {resource}; "
                    "run download-piper-plus-models.ps1"
                ) from error

        from piper import PiperVoice
        from piper_plus_g2p import get_phonemizer
        from piper_plus_g2p.encode import PiperEncoder

        self._model_path = model_path.resolve()
        self._config_path = config_path.resolve()
        self._voice = PiperVoice.load(self._model_path, self._config_path)
        self._length_scale = length_scale
        self._lock = threading.Lock()
        self._encoder = PiperEncoder(dict(self._voice.config.phoneme_id_map))
        self._phonemizers = {
            "en-US": get_phonemizer("en"),
            "ja-JP": get_phonemizer("ja"),
        }
        self._input_names = {item.name for item in self._voice.session.get_inputs()}
        self._output_names = [item.name for item in self._voice.session.get_outputs()]
        language_map = self._voice.config.language_id_map or {}
        self._language_ids = {
            "en-US": language_map.get("en", 1),
            "ja-JP": language_map.get("ja", 0),
        }
        model_hash = _file_sha256(self._model_path)[:16]
        config_hash = _file_sha256(self._config_path)[:16]
        self._identity = (
            f"piper-plus:{self._model_path.name}:{model_hash}:{config_hash}:"
            f"length-{self._length_scale:.3f}:normalizer-{PIPER_TEXT_NORMALIZER_VERSION}:"
            f"prosody-{PIPER_PROSODY_PIPELINE_VERSION}"
        )
        if warm_up:
            with self._lock:
                self._synthesize_unlocked("System ready.", "en-US", 1.0)
                self._synthesize_unlocked("音声準備完了。", "ja-JP", 1.0)

    @property
    def identity(self) -> str:
        return self._identity

    def synthesize(
        self, text: str, language: str, voice: str, speed: float
    ) -> tuple[np.ndarray, int]:
        del voice
        with self._lock:
            return self._synthesize_unlocked(text, language, speed)

    def _synthesize_unlocked(
        self, text: str, language: str, speed: float
    ) -> tuple[np.ndarray, int]:
        language_id = self._language_ids[language]
        text = _normalize_piper_text(text, language)
        tokens, token_prosody = self._phonemizers[language].phonemize_with_prosody(text)
        phoneme_ids, encoded_prosody = self._encoder.encode_with_prosody(
            tokens, token_prosody
        )
        if not phoneme_ids or len(phoneme_ids) != len(encoded_prosody):
            raise RuntimeError("Piper Plus returned no audio")

        phoneme_array = np.expand_dims(np.asarray(phoneme_ids, dtype=np.int64), 0)
        arguments: dict[str, np.ndarray] = {
            "input": phoneme_array,
            "input_lengths": np.asarray([phoneme_array.shape[1]], dtype=np.int64),
            "scales": np.asarray(
                [
                    self._voice.config.noise_scale,
                    self._length_scale / speed,
                    self._voice.config.noise_w,
                ],
                dtype=np.float32,
            ),
        }
        if "lid" in self._input_names:
            arguments["lid"] = np.asarray([language_id], dtype=np.int64)
        if "sid" in self._input_names:
            arguments["sid"] = np.asarray([[0]], dtype=np.int64)
        if "prosody_features" in self._input_names:
            arguments["prosody_features"] = np.expand_dims(
                np.asarray(
                    [
                        [item.a1, item.a2, item.a3] if item is not None else [0, 0, 0]
                        for item in encoded_prosody
                    ],
                    dtype=np.int64,
                ),
                0,
            )
        if "speaker_embedding" in self._input_names:
            embedding_input = next(
                item
                for item in self._voice.session.get_inputs()
                if item.name == "speaker_embedding"
            )
            embedding_size = (
                embedding_input.shape[1]
                if len(embedding_input.shape) >= 2
                and isinstance(embedding_input.shape[1], int)
                else 256
            )
            arguments["speaker_embedding"] = np.zeros(
                (1, embedding_size), dtype=np.float32
            )
            arguments["speaker_embedding_mask"] = np.asarray([[0]], dtype=np.int64)

        outputs = self._voice.session.run(self._output_names, arguments)
        output_index = (
            self._output_names.index("output")
            if "output" in self._output_names
            else 0
        )
        samples = np.asarray(outputs[output_index], dtype=np.float32).squeeze()
        samples = np.nan_to_num(samples, nan=0.0, posinf=1.0, neginf=-1.0)
        samples = np.clip(samples, -1.0, 1.0)
        if samples.size == 0:
            raise RuntimeError("Piper Plus returned no audio")
        return samples, int(self._voice.config.sample_rate)


def _file_sha256(path: Path) -> str:
    with path.open("rb") as source:
        return hashlib.file_digest(source, "sha256").hexdigest()


def decode_pcm_wave(data: bytes) -> tuple[np.ndarray, int]:
    import io

    with wave.open(io.BytesIO(data), "rb") as source:
        if source.getnchannels() not in {1, 2} or source.getsampwidth() != 2:
            raise ValueError("only mono or stereo PCM16 WAV is supported")
        sample_rate = source.getframerate()
        channels = source.getnchannels()
        samples = np.frombuffer(source.readframes(source.getnframes()), dtype="<i2")
    if channels == 2:
        samples = samples.reshape(-1, 2).mean(axis=1)
    return (samples.astype(np.float32) / 32768.0), sample_rate


def resample_mono(samples: np.ndarray, sample_rate: int) -> np.ndarray:
    samples = np.asarray(samples, dtype=np.float32).reshape(-1)
    if len(samples) == 0 or sample_rate <= 0:
        raise ValueError("synthesized audio is empty")
    samples = np.nan_to_num(samples, nan=0.0, posinf=0.0, neginf=0.0)
    peak = float(np.max(np.abs(samples)))
    if peak > 1.0:
        samples = samples / peak
    if sample_rate == OPUS_CLOCK_RATE:
        return samples
    output_count = max(1, round(len(samples) * OPUS_CLOCK_RATE / sample_rate))
    source_axis = np.linspace(0.0, 1.0, len(samples), endpoint=False)
    output_axis = np.linspace(0.0, 1.0, output_count, endpoint=False)
    return np.interp(output_axis, source_axis, samples).astype(np.float32)


def encode_opus_packets(samples: np.ndarray, sample_rate: int) -> tuple[list[bytes], int]:
    output = resample_mono(samples, sample_rate)
    original_samples = len(output)
    padded_count = math.ceil(original_samples / FRAME_SAMPLES) * FRAME_SAMPLES
    if padded_count > original_samples:
        output = np.pad(output, (0, padded_count - original_samples))

    codec = av.CodecContext.create("libopus", "w")
    codec.sample_rate = OPUS_CLOCK_RATE
    codec.layout = "mono"
    codec.format = "fltp"
    codec.bit_rate = 48_000
    codec.time_base = Fraction(1, OPUS_CLOCK_RATE)
    codec.open()

    packets: list[bytes] = []
    for offset in range(0, len(output), FRAME_SAMPLES):
        frame = av.AudioFrame(format="fltp", layout="mono", samples=FRAME_SAMPLES)
        frame.sample_rate = OPUS_CLOCK_RATE
        frame.time_base = Fraction(1, OPUS_CLOCK_RATE)
        frame.pts = offset
        frame.planes[0].update(output[offset : offset + FRAME_SAMPLES].tobytes())
        packets.extend(bytes(packet) for packet in codec.encode(frame))
    packets.extend(bytes(packet) for packet in codec.encode(None))
    if not packets:
        raise RuntimeError("Opus encoder returned no packets")
    duration_ms = math.ceil(original_samples * 1000 / OPUS_CLOCK_RATE)
    packet_duration_ms = len(packets) * FRAME_DURATION_MS
    duration_ms = max(packet_duration_ms - FRAME_DURATION_MS + 1, min(duration_ms, packet_duration_ms))
    return packets, duration_ms


@dataclass(frozen=True)
class PromptRequest:
    event_key: str
    language: str
    voice: str
    text: str
    speed: float


@dataclass(frozen=True)
class SynthesisRequest(PromptRequest):
    pass


def parse_prompt_request(payload: object) -> PromptRequest:
    if not isinstance(payload, dict):
        raise ValueError("JSON object is required")
    event_key = str(payload.get("eventKey", "")).strip()
    language = str(payload.get("language", "")).strip()
    voice = str(payload.get("voice", "")).strip()
    text = str(payload.get("text", "")).strip()
    try:
        speed = float(payload.get("speed", 1.0))
    except (TypeError, ValueError) as error:
        raise ValueError("speed must be numeric") from error
    if not event_key or len(event_key) > 256:
        raise ValueError("eventKey is required and must not exceed 256 characters")
    if language not in {"en-US", "ja-JP"}:
        raise ValueError("language must be en-US or ja-JP")
    if not voice or len(voice) > 64:
        raise ValueError("voice is required and must not exceed 64 characters")
    if not text or len(text) > MAX_TEXT_LENGTH:
        raise ValueError(f"text is required and must not exceed {MAX_TEXT_LENGTH} characters")
    if not 0.5 <= speed <= 2.0:
        raise ValueError("speed must be between 0.5 and 2.0")
    return PromptRequest(event_key, language, voice, text, speed)


def parse_synthesis_request(payload: object) -> SynthesisRequest:
    request = parse_prompt_request(payload)
    assert isinstance(payload, dict)
    codec = str(payload.get("codec", "")).strip().lower()
    frame_duration_ms = payload.get("frameDurationMs")
    if codec != "opus" or frame_duration_ms != FRAME_DURATION_MS:
        raise ValueError("only 20 ms Opus packets are supported")
    return SynthesisRequest(
        request.event_key,
        request.language,
        request.voice,
        request.text,
        request.speed,
    )


class RaceAudioApplication:
    def __init__(self, engine: AudioEngine, cache_dir: Path) -> None:
        self.engine = engine
        self.cache_dir = cache_dir.resolve()
        self.cache_dir.mkdir(parents=True, exist_ok=True)
        self._locks: dict[str, threading.Lock] = {}
        self._locks_guard = threading.Lock()

    def prepare(self, request: PromptRequest) -> dict[str, object]:
        prepare = getattr(self.engine, "prepare_browser_prompt", None)
        if not callable(prepare):
            raise ValueError("configured engine does not support browser prompts")
        return prepare(request.text, request.language, request.voice, request.speed)

    def synthesize(self, request: SynthesisRequest) -> dict[str, object]:
        started = time.perf_counter()
        cache_key = hashlib.sha256(
            json.dumps(
                {
                    "engine": self.engine.identity,
                    "language": request.language,
                    "voice": request.voice,
                    "text": request.text,
                    "speed": request.speed,
                    "codec": "opus",
                    "frameDurationMs": FRAME_DURATION_MS,
                },
                sort_keys=True,
                ensure_ascii=False,
                separators=(",", ":"),
            ).encode("utf-8")
        ).hexdigest()
        cache_path = self.cache_dir / f"{cache_key}.json"
        with self._lock_for(cache_key):
            if cache_path.is_file():
                cached = json.loads(cache_path.read_text(encoding="utf-8"))
                return {
                    **cached,
                    "cacheHit": True,
                    "serviceElapsedMs": round((time.perf_counter() - started) * 1000),
                }
            samples, sample_rate = self.engine.synthesize(
                request.text, request.language, request.voice, request.speed
            )
            packets, duration_ms = encode_opus_packets(samples, sample_rate)
            digest = hashlib.sha256(b"".join(packets)).hexdigest()
            response: dict[str, object] = {
                "version": PROTOCOL_VERSION,
                "codec": "opus",
                "clockRate": OPUS_CLOCK_RATE,
                "channels": OPUS_CHANNELS,
                "frameDurationMs": FRAME_DURATION_MS,
                "durationMs": duration_ms,
                "sha256": digest,
                "packets": [base64.b64encode(packet).decode("ascii") for packet in packets],
            }
            temporary = cache_path.with_suffix(".tmp")
            temporary.write_text(
                json.dumps(response, ensure_ascii=False, separators=(",", ":")),
                encoding="utf-8",
            )
            temporary.replace(cache_path)
            return {
                **response,
                "cacheHit": False,
                "serviceElapsedMs": round((time.perf_counter() - started) * 1000),
            }

    def _lock_for(self, cache_key: str) -> threading.Lock:
        with self._locks_guard:
            return self._locks.setdefault(cache_key, threading.Lock())


class RaceAudioRequestHandler(BaseHTTPRequestHandler):
    server_version = "MomoRaceAudio/1"

    @property
    def application(self) -> RaceAudioApplication:
        return self.server.application  # type: ignore[attr-defined]

    @property
    def bearer_token(self) -> str:
        return self.server.bearer_token  # type: ignore[attr-defined]

    def do_GET(self) -> None:
        if self.path != "/healthz":
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        self._send_json(
            HTTPStatus.OK,
            {"status": "ok", "version": PROTOCOL_VERSION, "engine": self.application.engine.identity},
        )

    def do_POST(self) -> None:
        if self.path not in {"/v1/prepare", "/v1/synthesize"}:
            self.send_error(HTTPStatus.NOT_FOUND)
            return
        if self.bearer_token:
            expected = f"Bearer {self.bearer_token}"
            if not hmac.compare_digest(self.headers.get("Authorization", ""), expected):
                self._send_json(HTTPStatus.UNAUTHORIZED, {"error": "unauthorized"})
                return
        try:
            content_length = int(self.headers.get("Content-Length", "0"))
        except ValueError:
            content_length = 0
        if content_length <= 0 or content_length > MAX_REQUEST_BYTES:
            self._send_json(HTTPStatus.BAD_REQUEST, {"error": "invalid_content_length"})
            return
        try:
            payload = json.loads(self.rfile.read(content_length))
            if self.path == "/v1/prepare":
                response = self.application.prepare(parse_prompt_request(payload))
            else:
                response = self.application.synthesize(parse_synthesis_request(payload))
        except (ValueError, json.JSONDecodeError) as error:
            self._send_json(HTTPStatus.BAD_REQUEST, {"error": str(error)})
            return
        except (urllib.error.URLError, OSError, RuntimeError) as error:
            self.log_error("synthesis failed: %s", error)
            self._send_json(HTTPStatus.SERVICE_UNAVAILABLE, {"error": "synthesis_failed"})
            return
        self._send_json(HTTPStatus.OK, response)

    def log_message(self, message: str, *args: object) -> None:
        print(f"{self.address_string()} - {message % args}")

    def _send_json(self, status: HTTPStatus, payload: object) -> None:
        body = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Cache-Control", "no-store")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


class RaceAudioHTTPServer(ThreadingHTTPServer):
    request_queue_size = 64
    daemon_threads = True


def create_engine(args: argparse.Namespace) -> AudioEngine:
    match args.engine:
        case "fixture":
            return FixtureEngine()
        case "kokoro":
            return KokoroEngine(
                Path(args.kokoro_model),
                Path(args.kokoro_voices),
                japanese_pronunciations=load_kokoro_japanese_pronunciation_dictionary(
                    Path(args.kokoro_japanese_pronunciation_dictionary)
                ),
            )
        case "voicevox":
            return VoicevoxEngine(args.voicevox_url, args.voicevox_speaker)
        case "piper-plus":
            return PiperPlusEngine(
                Path(args.piper_model),
                Path(args.piper_config),
                Path(args.piper_nltk_data),
                args.piper_length_scale,
            )
        case _:
            raise ValueError(f"unsupported engine: {args.engine}")


def parse_listen(value: str) -> tuple[str, int]:
    host, separator, port_text = value.rpartition(":")
    if not separator or not host:
        raise ValueError("listen must be HOST:PORT")
    port = int(port_text)
    if not 1 <= port <= 65535:
        raise ValueError("listen port is out of range")
    return host, port


def require_token_for_non_loopback(host: str, token: str) -> None:
    try:
        loopback = ipaddress.ip_address(host).is_loopback
    except ValueError:
        loopback = host.lower() == "localhost"
    if not loopback and not token:
        raise ValueError(
            "MOMO_RACE_AUDIO_SERVICE_TOKEN is required for a non-loopback listen address"
        )


def main() -> None:
    parser = argparse.ArgumentParser(description="Momo Relay internal race audio service")
    parser.add_argument("--listen", default="127.0.0.1:18090")
    parser.add_argument(
        "--engine",
        choices=("fixture", "kokoro", "voicevox", "piper-plus"),
        default="kokoro",
    )
    parser.add_argument("--cache-dir", default="cache")
    parser.add_argument("--kokoro-model", default="models/kokoro-v1.0.onnx")
    parser.add_argument("--kokoro-voices", default="models/voices-v1.0.bin")
    parser.add_argument(
        "--kokoro-japanese-pronunciation-dictionary",
        default="japanese_pronunciation_dictionary.json",
    )
    parser.add_argument("--voicevox-url", default="http://127.0.0.1:50021")
    parser.add_argument("--voicevox-speaker", type=int, default=51)
    parser.add_argument("--piper-model", default="models/css10-ja-6lang-fp16.onnx")
    parser.add_argument("--piper-config", default="models/css10-ja-6lang-config.json")
    parser.add_argument("--piper-nltk-data", default="models/nltk_data")
    parser.add_argument("--piper-length-scale", type=float, default=1.2)
    args = parser.parse_args()

    host, port = parse_listen(args.listen)
    bearer_token = os.environ.get("MOMO_RACE_AUDIO_SERVICE_TOKEN", "").strip()
    require_token_for_non_loopback(host, bearer_token)
    application = RaceAudioApplication(create_engine(args), Path(args.cache_dir))
    server = RaceAudioHTTPServer((host, port), RaceAudioRequestHandler)
    server.application = application  # type: ignore[attr-defined]
    server.bearer_token = bearer_token  # type: ignore[attr-defined]
    print(f"race audio service listening on http://{host}:{port} engine={application.engine.identity}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()


if __name__ == "__main__":
    main()
