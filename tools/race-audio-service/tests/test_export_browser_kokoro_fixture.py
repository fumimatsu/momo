from __future__ import annotations

import json
import wave
from pathlib import Path

import numpy as np

from export_browser_kokoro_fixture import export_fixture
from race_audio_service import (
    apply_kokoro_japanese_pronunciation_dictionary,
    load_kokoro_japanese_pronunciation_dictionary,
    normalize_kokoro_japanese_terminal_phonemes,
)


class FakeKokoroEngine:
    def phoneme_batches(self, text: str, language: str) -> list[str]:
        if language == "ja-JP":
            return ["desu^^__"]
        return [f"normalized-{language}-phonemes:{text}"]

    def model_input_ids(self, phonemes: str) -> list[int]:
        return [0, len(phonemes), 0]

    def synthesize(
        self, text: str, language: str, voice: str, speed: float
    ) -> tuple[np.ndarray, int]:
        assert language in {"ja-JP", "en-US"}
        assert voice in {"jf_alpha", "am_michael"}
        assert speed == 1.04
        return np.linspace(-0.1, 0.1, 240, dtype=np.float32), 24_000

    def synthesize_phonemes(
        self, phonemes: str, voice: str, speed: float
    ) -> tuple[np.ndarray, int]:
        assert phonemes == "desu."
        assert voice == "jf_alpha"
        assert speed == 1.04
        return np.linspace(-0.1, 0.1, 240, dtype=np.float32), 24_000


def test_japanese_terminal_phonemes_drop_misaki_controls_and_keep_boundary() -> None:
    assert normalize_kokoro_japanese_terminal_phonemes("desu^^__") == "desu."
    assert normalize_kokoro_japanese_terminal_phonemes("desu.^^__j") == "desu."
    assert normalize_kokoro_japanese_terminal_phonemes("go,__---^") == "go,"


def test_japanese_pronunciation_dictionary_is_explicit_and_applied_before_terminal_policy(
    tmp_path: Path,
) -> None:
    path = tmp_path / "pronunciations.json"
    path.write_text(
        json.dumps({
            "schemaVersion": 1,
            "entries": [{"surface": "です", "phonemes": "deɕita"}],
        }, ensure_ascii=False),
        encoding="utf-8",
    )
    pronunciations = load_kokoro_japanese_pronunciation_dictionary(path)
    assert pronunciations == {"です": "deɕita"}
    assert apply_kokoro_japanese_pronunciation_dictionary(
        FakeKokoroEngine(), "です", "desu^^__", pronunciations
    ) == "deɕita^^__"


def test_export_fixture_writes_reproducible_manifest_and_wave(tmp_path: Path) -> None:
    model_path = tmp_path / "model.onnx"
    voices_path = tmp_path / "voices.bin"
    model_path.write_bytes(b"model")
    voices_path.write_bytes(b"voices")

    manifest = export_fixture(
        FakeKokoroEngine(),
        tmp_path / "fixture",
        model_path,
        voices_path,
        1.04,
        ["ja-JP", "en-US"],
        {"ja-JP": "jf_alpha", "en-US": "am_michael"},
        1,
    )

    saved = json.loads((tmp_path / "fixture" / "manifest.json").read_text(encoding="utf-8"))
    assert saved == manifest
    assert saved["schemaVersion"] == 2
    assert saved["browserRuntime"] == "kokoro-js@1.2.1"
    assert [profile["language"] for profile in saved["profiles"]] == ["ja-JP", "en-US"]
    assert [profile["voice"] for profile in saved["profiles"]] == ["jf_alpha", "am_michael"]
    for profile in saved["profiles"]:
        prompt = profile["prompts"][0]
        if profile["language"] == "ja-JP":
            assert profile["phonemePolicy"].startswith("strip-misaki-terminal-prosody")
            assert profile["pronunciationOverrides"] == []
            assert prompt["rawPhonemes"] == "desu^^__"
            assert prompt["phonemes"] == "desu."
        else:
            assert prompt["phonemes"].startswith("normalized-en-US-phonemes:")
        assert prompt["modelInputIds"][0] == prompt["modelInputIds"][-1] == 0
        assert len(prompt["pcm16Sha256"]) == 64
        with wave.open(str(tmp_path / "fixture" / prompt["referenceWav"]), "rb") as source:
            assert source.getframerate() == 24_000
            assert source.getnframes() == 240
