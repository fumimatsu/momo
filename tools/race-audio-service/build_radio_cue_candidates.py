from __future__ import annotations

import argparse
import html
import json
import wave
from dataclasses import dataclass
from pathlib import Path

import numpy as np


SAMPLE_RATE = 48_000


@dataclass(frozen=True)
class Candidate:
    file_name: str
    title: str
    description: str
    samples: np.ndarray


def envelope(length: int, attack_ms: float = 4, release_ms: float = 18) -> np.ndarray:
    result = np.ones(length, dtype=np.float32)
    attack = min(length // 3, round(SAMPLE_RATE * attack_ms / 1000))
    release = min(length // 2, round(SAMPLE_RATE * release_ms / 1000))
    if attack > 0:
        phase = np.linspace(0, np.pi / 2, attack, endpoint=False, dtype=np.float32)
        result[:attack] = np.sin(phase) ** 2
    if release > 0:
        phase = np.linspace(np.pi / 2, 0, release, endpoint=True, dtype=np.float32)
        result[-release:] = np.sin(phase) ** 2
    return result


def add_tone(
    output: np.ndarray,
    start_ms: float,
    duration_ms: float,
    start_hz: float,
    end_hz: float,
    amplitude: float,
    waveform: str = "sine",
    attack_ms: float = 4,
    release_ms: float = 18,
) -> None:
    start = round(SAMPLE_RATE * start_ms / 1000)
    length = min(len(output) - start, round(SAMPLE_RATE * duration_ms / 1000))
    if start < 0 or length <= 0:
        return
    frequencies = np.linspace(start_hz, end_hz, length, dtype=np.float32)
    phase = np.cumsum(frequencies, dtype=np.float64) * (2 * np.pi / SAMPLE_RATE)
    sine = np.sin(phase)
    match waveform:
        case "square":
            signal = np.tanh(sine * 3.2)
        case "triangle":
            signal = (2 / np.pi) * np.arcsin(sine)
        case _:
            signal = sine
    output[start : start + length] += (
        signal.astype(np.float32)
        * envelope(length, attack_ms, release_ms)
        * amplitude
    )


def add_radio_noise(
    output: np.ndarray,
    rng: np.random.Generator,
    start_ms: float,
    duration_ms: float,
    amplitude: float,
    attack_ms: float = 2,
    release_ms: float = 24,
) -> None:
    start = round(SAMPLE_RATE * start_ms / 1000)
    length = min(len(output) - start, round(SAMPLE_RATE * duration_ms / 1000))
    if start < 0 or length <= 0:
        return
    noise = rng.standard_normal(length + 24).astype(np.float32)
    low = np.convolve(noise, np.ones(17, dtype=np.float32) / 17, mode="same")
    high = noise - low
    high = high[12 : 12 + length]
    peak = float(np.max(np.abs(high)))
    if peak > 0:
        high /= peak
    output[start : start + length] += (
        high * envelope(length, attack_ms, release_ms) * amplitude
    )


def normalize(samples: np.ndarray) -> np.ndarray:
    samples = np.tanh(samples * 1.12).astype(np.float32)
    peak = float(np.max(np.abs(samples)))
    if peak > 0:
        samples *= 0.72 / peak
    return samples


def blank(duration_ms: int) -> np.ndarray:
    return np.zeros(round(SAMPLE_RATE * duration_ms / 1000), dtype=np.float32)


def build_candidates() -> list[Candidate]:
    rng = np.random.default_rng(20260816)
    candidates: list[Candidate] = []

    clean = blank(250)
    add_tone(clean, 0, 104, 520, 650, 0.34, "triangle", release_ms=24)
    add_tone(clean, 82, 126, 470, 390, 0.31, "sine", release_ms=38)
    candidates.append(Candidate(
        "01-warm-call.wav",
        "Warm call",
        "A restrained rising and falling call with no high-frequency edge.",
        normalize(clean),
    ))

    minimal = blank(190)
    add_tone(minimal, 0, 142, 610, 500, 0.36, "sine", release_ms=52)
    add_tone(minimal, 12, 86, 305, 250, 0.13, "triangle", release_ms=32)
    candidates.append(Candidate(
        "02-soft-keyup.wav",
        "Soft key-up",
        "The shortest option. A single warm cue that stays behind the race audio.",
        normalize(minimal),
    ))

    double = blank(310)
    add_tone(double, 0, 104, 460, 540, 0.31, "sine", release_ms=30)
    add_tone(double, 142, 116, 570, 480, 0.34, "sine", release_ms=42)
    add_tone(double, 150, 92, 285, 240, 0.11, "triangle", release_ms=36)
    candidates.append(Candidate(
        "03-calm-double.wav",
        "Calm double",
        "Two soft pulses. Noticeable without sounding like an alarm.",
        normalize(double),
    ))

    squelch = blank(280)
    add_radio_noise(squelch, rng, 0, 38, 0.08, release_ms=14)
    add_tone(squelch, 12, 184, 680, 440, 0.35, "triangle", release_ms=62)
    add_tone(squelch, 32, 142, 340, 280, 0.12, "sine", release_ms=54)
    candidates.append(Candidate(
        "04-muted-radio.wav",
        "Muted radio",
        "A very small key-up texture followed by a low, rounded radio cue.",
        normalize(squelch),
    ))

    pitwall = blank(350)
    add_tone(pitwall, 0, 132, 390, 470, 0.29, "triangle", release_ms=36)
    add_tone(pitwall, 0, 132, 585, 705, 0.10, "sine", release_ms=36)
    add_tone(pitwall, 158, 142, 470, 360, 0.32, "triangle", release_ms=54)
    add_tone(pitwall, 158, 142, 705, 540, 0.09, "sine", release_ms=54)
    candidates.append(Candidate(
        "05-team-radio.wav",
        "Team radio",
        "The fullest option, but kept below 710 Hz for a calmer character.",
        normalize(pitwall),
    ))

    return candidates


def write_wave(path: Path, samples: np.ndarray) -> None:
    pcm = np.clip(samples, -1, 1)
    pcm = np.round(pcm * 32767).astype("<i2")
    with wave.open(str(path), "wb") as target:
        target.setnchannels(1)
        target.setsampwidth(2)
        target.setframerate(SAMPLE_RATE)
        target.writeframes(pcm.tobytes())


def write_html(output_dir: Path, candidates: list[Candidate]) -> None:
    rows = []
    for index, candidate in enumerate(candidates, start=1):
        duration_ms = round(len(candidate.samples) * 1000 / SAMPLE_RATE)
        rows.append(
            "<article>"
            f"<div class=\"index\">{index:02d}</div>"
            "<div class=\"copy\">"
            f"<h2>{html.escape(candidate.title)}</h2>"
            f"<p>{html.escape(candidate.description)}</p>"
            f"<small>{duration_ms} ms / mono / 48 kHz</small>"
            "</div>"
            f"<audio controls preload=\"metadata\" src=\"{html.escape(candidate.file_name)}\"></audio>"
            "</article>"
        )
    page = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Momo radio cue candidates</title>
<style>
:root { color-scheme: dark; font-family: Segoe UI, Arial, sans-serif; background: #080b0d; color: #eef5f7; }
body { margin: 0; padding: 28px; max-width: 1080px; margin-inline: auto; }
header { border-bottom: 1px solid #34434a; padding-bottom: 18px; margin-bottom: 18px; }
h1 { font-size: 26px; margin: 0 0 8px; letter-spacing: 0; }
header p, article p { color: #a7bac2; margin: 0; line-height: 1.45; }
article { display: grid; grid-template-columns: 48px minmax(260px, 1fr) minmax(280px, 440px); align-items: center; gap: 18px; padding: 18px 0; border-bottom: 1px solid #253138; }
.index { font: 700 18px Consolas, monospace; color: #72d9ef; }
h2 { margin: 0 0 5px; font-size: 18px; }
small { display: block; margin-top: 8px; color: #718991; }
audio { width: 100%; height: 42px; }
@media (max-width: 760px) { body { padding: 18px; } article { grid-template-columns: 40px 1fr; } audio { grid-column: 2; } }
</style>
</head>
<body>
<header><h1>Momo radio cue candidates</h1><p>Original synthetic cues for the short delay before race announcements.</p></header>
""" + "\n".join(rows) + """
</body>
</html>
"""
    (output_dir / "comparison.html").write_text(page, encoding="utf-8")


def main() -> None:
    parser = argparse.ArgumentParser(description="Build original radio cue WAV candidates")
    parser.add_argument("output_dir", type=Path)
    args = parser.parse_args()
    output_dir = args.output_dir.resolve()
    output_dir.mkdir(parents=True, exist_ok=True)

    candidates = build_candidates()
    manifest = []
    combined: list[np.ndarray] = []
    for candidate in candidates:
        write_wave(output_dir / candidate.file_name, candidate.samples)
        duration_ms = round(len(candidate.samples) * 1000 / SAMPLE_RATE)
        manifest.append({
            "file": candidate.file_name,
            "title": candidate.title,
            "description": candidate.description,
            "durationMs": duration_ms,
            "sampleRate": SAMPLE_RATE,
        })
        combined.extend((candidate.samples, blank(650)))
    write_wave(output_dir / "all-candidates.wav", np.concatenate(combined))
    (output_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, ensure_ascii=True) + "\n",
        encoding="ascii",
    )
    write_html(output_dir, candidates)
    print(output_dir / "comparison.html")


if __name__ == "__main__":
    main()
