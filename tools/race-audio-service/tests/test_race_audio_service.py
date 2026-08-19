from __future__ import annotations

import base64
import json
import threading
from pathlib import Path

import numpy as np
import pytest

from race_audio_service import (
    FRAME_DURATION_MS,
    FixtureEngine,
    KokoroEngine,
    RaceAudioApplication,
    RaceAudioHTTPServer,
    _normalize_piper_text,
    encode_opus_packets,
    parse_prompt_request,
    parse_synthesis_request,
    require_token_for_non_loopback,
)


def test_http_server_accepts_a_full_pilot_field_burst() -> None:
    assert RaceAudioHTTPServer.request_queue_size >= 32
    assert RaceAudioHTTPServer.daemon_threads is True


@pytest.mark.parametrize(
    ("language", "source", "expected"),
    [
        (
            "en-US",
            "Lap 1 complete. 13.444 seconds. Position 2.",
            "Lap one complete. thirteen point four four four seconds. Position two.",
        ),
        (
            "ja-JP",
            "1 周目。13.444 秒。現在 2 位。",
            "一 周目。十三点四四四 秒。現在 二 位。",
        ),
        ("en-US", "Car 24, lap 101.", "Car twenty four, lap one hundred one."),
        ("ja-JP", "24 周、101 秒。", "二十四 周、百一 秒。"),
    ],
)
def test_normalize_piper_text_expands_race_numbers(
    language: str, source: str, expected: str
) -> None:
    assert _normalize_piper_text(source, language) == expected


def test_piper_plus_japanese_g2p_provides_accent_features() -> None:
    from piper_plus_g2p import get_phonemizer

    tokens, prosody = get_phonemizer("ja").phonemize_with_prosody(
        "今日は良い天気です。"
    )

    assert tokens
    assert len(tokens) == len(prosody)
    assert any(
        item is not None and (item.a1 != 0 or item.a2 != 0 or item.a3 != 0)
        for item in prosody
    )


def valid_request() -> dict[str, object]:
    return {
        "eventKey": "run-1:CP-1:lap:1:13444",
        "language": "en-US",
        "voice": "af_heart",
        "text": "Lap one complete. Thirteen point four seconds.",
        "speed": 1.04,
        "codec": "opus",
        "frameDurationMs": FRAME_DURATION_MS,
    }


def test_prompt_request_does_not_require_media_encoding_fields() -> None:
    payload = valid_request()
    payload.pop("codec")
    payload.pop("frameDurationMs")
    request = parse_prompt_request(payload)
    assert request.event_key == "run-1:CP-1:lap:1:13444"
    assert request.language == "en-US"


def test_encode_opus_packets_returns_twenty_millisecond_packets() -> None:
    sample_rate = 24_000
    points = np.arange(sample_rate // 2, dtype=np.float32)
    samples = np.sin(points * (2 * np.pi * 440 / sample_rate)).astype(np.float32) * 0.1
    packets, duration_ms = encode_opus_packets(samples, sample_rate)
    assert len(packets) >= 25
    assert duration_ms <= len(packets) * FRAME_DURATION_MS
    assert all(0 < len(packet) <= 1500 for packet in packets)


def test_application_caches_identical_request(tmp_path: Path) -> None:
    application = RaceAudioApplication(FixtureEngine(), tmp_path)
    request = parse_synthesis_request(valid_request())
    first = application.synthesize(request)
    second = application.synthesize(request)
    assert {
        key: value for key, value in first.items() if key not in {"cacheHit", "serviceElapsedMs"}
    } == {
        key: value for key, value in second.items() if key not in {"cacheHit", "serviceElapsedMs"}
    }
    assert len(list(tmp_path.glob("*.json"))) == 1
    assert first["codec"] == "opus"
    assert all(base64.b64decode(packet) for packet in first["packets"])


def test_application_reports_cache_and_service_time(tmp_path: Path) -> None:
    application = RaceAudioApplication(FixtureEngine(), tmp_path)
    request = parse_synthesis_request(valid_request())
    first = application.synthesize(request)
    second = application.synthesize(request)
    assert first["cacheHit"] is False
    assert second["cacheHit"] is True
    assert first["serviceElapsedMs"] >= 0
    assert second["serviceElapsedMs"] >= 0


def test_kokoro_uses_misaki_phonemes_for_japanese() -> None:
    class FakeTokenizer:
        @staticmethod
        def tokenize(phonemes: str) -> list[int]:
            assert phonemes == "normalized:nihongo."
            return [41, 42]

    class FakeModel:
        tokenizer = FakeTokenizer()

        def __init__(self) -> None:
            self.arguments = None

        def create(self, text, **arguments):
            self.arguments = (text, arguments)
            return np.zeros(240, dtype=np.float32), 24_000

    engine = KokoroEngine.__new__(KokoroEngine)
    engine._lock = threading.Lock()
    engine._model = FakeModel()
    engine._model._split_phonemes = lambda phonemes: [f"normalized:{phonemes}"]
    engine._japanese_g2p = lambda text: ("nihongo^___", [text])
    engine._japanese_pronunciations = {}

    assert engine.phonemize_japanese("日本語の実況です。") == "nihongo^___"
    assert engine.japanese_phoneme_batches("日本語の実況です。") == [
        "normalized:nihongo^___"
    ]
    assert engine.prepare_japanese_phonemes("日本語の実況です。") == "nihongo."
    samples, sample_rate = engine.synthesize("日本語の実況です。", "ja-JP", "jf_alpha", 1.0)

    assert samples.shape == (240,)
    assert sample_rate == 24_000
    assert engine._model.arguments == (
        "nihongo.",
        {"voice": "jf_alpha", "speed": 1.0, "is_phonemes": True},
    )

    prompt = engine.prepare_browser_prompt(
        "日本語の実況です。", "ja-JP", "jf_alpha", 1.0
    )
    assert prompt["modelId"] == "onnx-community/Kokoro-82M-v1.0-ONNX"
    assert prompt["phonemes"] == "normalized:nihongo."
    assert prompt["modelInputIds"] == [0, 41, 42, 0]
    assert prompt["phonemePolicy"].startswith("strip-misaki-terminal-prosody")

    samples, sample_rate = engine.synthesize_phonemes("nihongo.", "jf_alpha", 1.0)
    assert samples.shape == (240,)
    assert sample_rate == 24_000
    assert engine._model.arguments == (
        "nihongo.",
        {"voice": "jf_alpha", "speed": 1.0, "is_phonemes": True},
    )


def test_kokoro_exports_normalized_english_phonemes() -> None:
    class FakeTokenizer:
        def phonemize(self, text: str, language: str) -> str:
            assert language == "en-us"
            return f"english:{text}"

        @staticmethod
        def tokenize(phonemes: str) -> list[int]:
            assert phonemes == "normalized:english:Lap four complete."
            return [11, 12, 13]

    class FakeModel:
        tokenizer = FakeTokenizer()

        @staticmethod
        def _split_phonemes(phonemes: str) -> list[str]:
            return [f"normalized:{phonemes}"]

    engine = KokoroEngine.__new__(KokoroEngine)
    engine._model = FakeModel()

    assert engine.english_phoneme_batches("Lap four complete.") == [
        "normalized:english:Lap four complete."
    ]
    assert engine.phoneme_batches("Lap four complete.", "en-US") == [
        "normalized:english:Lap four complete."
    ]
    assert engine.model_input_ids("normalized:english:Lap four complete.") == [0, 11, 12, 13, 0]
    with pytest.raises(ValueError, match="unsupported Kokoro phoneme language"):
        engine.phoneme_batches("Bonjour", "fr-FR")


def test_cache_reuses_identical_audio_across_event_ids(tmp_path: Path) -> None:
    application = RaceAudioApplication(FixtureEngine(), tmp_path)
    first_payload = valid_request()
    second_payload = valid_request()
    second_payload["eventKey"] = "run-2:CP-1:lap:1"
    first = application.synthesize(parse_synthesis_request(first_payload))
    second = application.synthesize(parse_synthesis_request(second_payload))
    assert {
        key: value for key, value in first.items() if key not in {"cacheHit", "serviceElapsedMs"}
    } == {
        key: value for key, value in second.items() if key not in {"cacheHit", "serviceElapsedMs"}
    }
    assert first["cacheHit"] is False
    assert second["cacheHit"] is True
    assert len(list(tmp_path.glob("*.json"))) == 1


@pytest.mark.parametrize(
    ("field", "value"),
    [
        ("language", "fr-FR"),
        ("voice", ""),
        ("text", ""),
        ("speed", 3.0),
        ("codec", "pcm"),
        ("frameDurationMs", 60),
    ],
)
def test_request_rejects_invalid_contract(field: str, value: object) -> None:
    payload = valid_request()
    payload[field] = value
    with pytest.raises(ValueError):
        parse_synthesis_request(payload)


def test_cached_response_is_json_serializable(tmp_path: Path) -> None:
    response = RaceAudioApplication(FixtureEngine(), tmp_path).synthesize(
        parse_synthesis_request(valid_request())
    )
    assert json.loads(json.dumps(response))["version"] == 1


@pytest.mark.parametrize("host", ["0.0.0.0", "192.168.11.105"])
def test_non_loopback_listener_requires_token(host: str) -> None:
    with pytest.raises(ValueError, match="TOKEN is required"):
        require_token_for_non_loopback(host, "")
    require_token_for_non_loopback(host, "secret")


@pytest.mark.parametrize("host", ["127.0.0.1", "localhost"])
def test_loopback_listener_allows_fixture_without_token(host: str) -> None:
    require_token_for_non_loopback(host, "")
