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


def fade(samples: np.ndarray, attack_ms: float = 5, release_ms: float = 45) -> np.ndarray:
    result = samples.astype(np.float32, copy=True)
    attack = min(len(result) // 3, round(SAMPLE_RATE * attack_ms / 1000))
    release = min(len(result) // 2, round(SAMPLE_RATE * release_ms / 1000))
    if attack > 0:
        phase = np.linspace(0, np.pi / 2, attack, endpoint=False, dtype=np.float32)
        result[:attack] *= np.sin(phase) ** 2
    if release > 0:
        phase = np.linspace(np.pi / 2, 0, release, endpoint=True, dtype=np.float32)
        result[-release:] *= np.sin(phase) ** 2
    return result


def tone(
    duration_ms: float,
    start_hz: float,
    end_hz: float,
    amplitude: float,
    waveform: str = "sine",
    vibrato_hz: float = 0,
    vibrato_depth: float = 0,
    attack_ms: float = 5,
    release_ms: float = 45,
) -> np.ndarray:
    length = round(SAMPLE_RATE * duration_ms / 1000)
    frequencies = np.linspace(start_hz, end_hz, length, dtype=np.float32)
    if vibrato_hz > 0 and vibrato_depth > 0:
        time_axis = np.arange(length, dtype=np.float32) / SAMPLE_RATE
        frequencies *= 1 + np.sin(2 * np.pi * vibrato_hz * time_axis) * vibrato_depth
    phase = np.cumsum(frequencies, dtype=np.float64) * (2 * np.pi / SAMPLE_RATE)
    sine = np.sin(phase)
    if waveform == "triangle":
        signal = (2 / np.pi) * np.arcsin(sine)
    else:
        signal = sine
    return fade(signal * amplitude, attack_ms, release_ms)


def filtered_noise(
    rng: np.random.Generator,
    duration_ms: float,
    center_hz: float,
    width_hz: float,
    amplitude: float,
    attack_ms: float = 2,
    release_ms: float = 55,
) -> np.ndarray:
    length = round(SAMPLE_RATE * duration_ms / 1000)
    fft_length = 1 << max(1, length - 1).bit_length()
    source = rng.standard_normal(fft_length).astype(np.float32)
    spectrum = np.fft.rfft(source)
    frequencies = np.fft.rfftfreq(fft_length, 1 / SAMPLE_RATE)
    sigma = max(80, width_hz / 2.355)
    response = np.exp(-0.5 * ((frequencies - center_hz) / sigma) ** 2)
    output = np.fft.irfft(spectrum * response, fft_length)[:length].astype(np.float32)
    peak = float(np.max(np.abs(output)))
    if peak > 0:
        output /= peak
    return fade(output * amplitude, attack_ms, release_ms)


def stereo_blank(duration_ms: int) -> np.ndarray:
    return np.zeros((round(SAMPLE_RATE * duration_ms / 1000), 2), dtype=np.float32)


def place(output: np.ndarray, samples: np.ndarray, start_ms: float, pan: float = 0) -> None:
    start = round(SAMPLE_RATE * start_ms / 1000)
    length = min(len(samples), len(output) - start)
    if start < 0 or length <= 0:
        return
    normalized_pan = max(-1, min(1, pan))
    angle = (normalized_pan + 1) * np.pi / 4
    output[start : start + length, 0] += samples[:length] * np.cos(angle)
    output[start : start + length, 1] += samples[:length] * np.sin(angle)


def add_room(samples: np.ndarray, amount: float = 1) -> np.ndarray:
    dry = samples.copy()
    result = dry.copy()
    for delay_ms, gain, cross in (
        (47, 0.15, False),
        (83, 0.10, True),
        (131, 0.055, False),
    ):
        delay = round(SAMPLE_RATE * delay_ms / 1000)
        delayed = dry[:-delay]
        if cross:
            delayed = delayed[:, ::-1]
        result[delay:] += delayed * gain * amount
    return result


def master(samples: np.ndarray, peak_target: float = 0.7) -> np.ndarray:
    result = np.tanh(samples * 1.35).astype(np.float32)
    peak = float(np.max(np.abs(result)))
    if peak > 0:
        result *= peak_target / peak
    return result


def build_candidates() -> list[Candidate]:
    rng = np.random.default_rng(20260816)
    candidates: list[Candidate] = []

    modern = stereo_blank(430)
    place(modern, filtered_noise(rng, 72, 2600, 2400, 0.13, release_ms=22), 0, -0.08)
    place(modern, tone(118, 540, 690, 0.31, "triangle", release_ms=34), 12, -0.12)
    place(modern, tone(112, 810, 1035, 0.09, vibrato_hz=5.1, vibrato_depth=0.003), 16, 0.2)
    place(modern, tone(178, 500, 370, 0.29, release_ms=70), 105, 0.06)
    place(modern, tone(168, 250, 185, 0.12, release_ms=76), 108, -0.04)
    candidates.append(Candidate(
        "01-modern-radio-open.wav",
        "Modern radio open",
        "Balanced transient, warm body, and a short stereo room tail.",
        master(add_room(modern, 0.85)),
    ))

    comms = stereo_blank(390)
    place(comms, filtered_noise(rng, 35, 2100, 3000, 0.08, release_ms=12), 0, 0)
    place(comms, tone(102, 430, 510, 0.30, release_ms=34), 8, -0.18)
    place(comms, tone(102, 645, 765, 0.10, release_ms=34), 8, 0.18)
    place(comms, tone(136, 560, 465, 0.31, "triangle", release_ms=58), 132, 0.1)
    place(comms, tone(126, 280, 232, 0.10, release_ms=62), 136, -0.1)
    candidates.append(Candidate(
        "02-soft-comms-pulse.wav",
        "Soft comms pulse",
        "Two rounded pulses with a subtle harmonic layer and restrained ambience.",
        master(add_room(comms, 0.6), 0.67),
    ))

    link = stereo_blank(470)
    place(link, filtered_noise(rng, 115, 1450, 2100, 0.12, release_ms=68), 0, -0.2)
    place(link, filtered_noise(rng, 115, 1800, 2400, 0.09, release_ms=68), 4, 0.2)
    place(link, tone(205, 720, 430, 0.31, "triangle", vibrato_hz=4.3, vibrato_depth=0.004, release_ms=92), 24, 0)
    place(link, tone(198, 360, 215, 0.12, release_ms=96), 30, 0)
    candidates.append(Candidate(
        "03-radio-link.wav",
        "Radio link",
        "A soft filtered link-open texture with less musical pitch movement.",
        master(add_room(link, 0.72), 0.68),
    ))

    pitwall = stereo_blank(530)
    place(pitwall, filtered_noise(rng, 48, 2400, 2600, 0.07, release_ms=20), 0, 0)
    place(pitwall, tone(142, 385, 465, 0.25, "triangle", release_ms=50), 12, -0.16)
    place(pitwall, tone(142, 578, 698, 0.085, release_ms=50), 12, 0.16)
    place(pitwall, tone(168, 485, 355, 0.28, "triangle", release_ms=80), 190, 0.12)
    place(pitwall, tone(168, 728, 533, 0.075, release_ms=80), 190, -0.12)
    candidates.append(Candidate(
        "04-pitwall-link.wav",
        "Pit-wall link",
        "The fullest option, with two warm stages and a polished game-radio tail.",
        master(add_room(pitwall, 1.0), 0.68),
    ))

    return candidates


def write_wave(path: Path, samples: np.ndarray) -> None:
    pcm = np.round(np.clip(samples, -1, 1) * 32767).astype("<i2")
    with wave.open(str(path), "wb") as target:
        target.setnchannels(2)
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
            f"<small>{duration_ms} ms / stereo / 48 kHz</small>"
            "</div>"
            f"<audio controls preload=\"metadata\" src=\"{html.escape(candidate.file_name)}\"></audio>"
            "</article>"
        )
    page = """<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Modern Momo radio cue candidates</title>
<style>
:root { color-scheme: dark; font-family: Segoe UI, Arial, sans-serif; background: #070a0c; color: #edf5f7; }
body { margin: 0; padding: 28px; max-width: 1080px; margin-inline: auto; }
header { border-bottom: 1px solid #334149; padding-bottom: 18px; margin-bottom: 18px; }
h1 { font-size: 26px; margin: 0 0 8px; letter-spacing: 0; }
header p, article p { color: #a8bac2; margin: 0; line-height: 1.45; }
article { display: grid; grid-template-columns: 48px minmax(260px, 1fr) minmax(280px, 440px); align-items: center; gap: 18px; padding: 20px 0; border-bottom: 1px solid #253139; }
.index { font: 700 18px Consolas, monospace; color: #77d8eb; }
h2 { margin: 0 0 5px; font-size: 18px; }
small { display: block; margin-top: 8px; color: #748b94; }
audio { width: 100%; height: 42px; }
@media (max-width: 760px) { body { padding: 18px; } article { grid-template-columns: 40px 1fr; } audio { grid-column: 2; } }
</style>
</head>
<body>
<header><h1>Modern radio cue candidates</h1><p>Pre-rendered stereo cues with filtered texture, warm layers, and short room tails.</p></header>
""" + "\n".join(rows) + """
</body>
</html>
"""
    (output_dir / "comparison.html").write_text(page, encoding="utf-8")


def main() -> None:
    parser = argparse.ArgumentParser(description="Build modern radio cue WAV candidates")
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
            "channels": 2,
        })
        combined.extend((candidate.samples, stereo_blank(650)))
    write_wave(output_dir / "all-candidates.wav", np.concatenate(combined, axis=0))
    (output_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, ensure_ascii=True) + "\n",
        encoding="ascii",
    )
    write_html(output_dir, candidates)
    print(output_dir / "comparison.html")


if __name__ == "__main__":
    main()
