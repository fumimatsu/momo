from __future__ import annotations

import argparse
import html
import json
import wave
from dataclasses import dataclass
from pathlib import Path

import av
import numpy as np


SAMPLE_RATE = 48_000


@dataclass(frozen=True)
class Candidate:
    file_name: str
    title: str
    description: str
    samples: np.ndarray


def read_audio(path: Path) -> np.ndarray:
    if not path.is_file():
        raise FileNotFoundError(f"source audio not found: {path}")
    chunks: list[np.ndarray] = []
    resampler = av.AudioResampler(format="fltp", layout="stereo", rate=SAMPLE_RATE)
    with av.open(str(path)) as container:
        if not container.streams.audio:
            raise ValueError(f"source has no audio stream: {path}")
        for frame in container.decode(container.streams.audio[0]):
            for converted in resampler.resample(frame):
                chunks.append(converted.to_ndarray().astype(np.float32, copy=False).T)
        for converted in resampler.resample(None):
            chunks.append(converted.to_ndarray().astype(np.float32, copy=False).T)
    if not chunks:
        raise ValueError(f"source decoded to no samples: {path}")
    return np.concatenate(chunks, axis=0)


def trim(samples: np.ndarray, start_ms: float, end_ms: float) -> np.ndarray:
    start = max(0, round(start_ms * SAMPLE_RATE / 1000))
    end = min(len(samples), round(end_ms * SAMPLE_RATE / 1000))
    return samples[start:end].copy()


def fade(samples: np.ndarray, attack_ms: float, release_ms: float) -> np.ndarray:
    output = samples.copy()
    attack = min(len(output) // 3, round(attack_ms * SAMPLE_RATE / 1000))
    release = min(len(output) // 2, round(release_ms * SAMPLE_RATE / 1000))
    if attack:
        phase = np.linspace(0, np.pi / 2, attack, endpoint=False, dtype=np.float32)
        output[:attack] *= (np.sin(phase) ** 2)[:, None]
    if release:
        phase = np.linspace(np.pi / 2, 0, release, endpoint=True, dtype=np.float32)
        output[-release:] *= (np.sin(phase) ** 2)[:, None]
    return output


def bandpass(samples: np.ndarray, low_hz: float, high_hz: float) -> np.ndarray:
    spectrum = np.fft.rfft(samples, axis=0)
    frequencies = np.fft.rfftfreq(len(samples), 1 / SAMPLE_RATE)
    spectrum[(frequencies < low_hz) | (frequencies > high_hz)] = 0
    return np.fft.irfft(spectrum, n=len(samples), axis=0).astype(np.float32)


def room(samples: np.ndarray, amount: float) -> np.ndarray:
    dry = samples.copy()
    output = dry.copy()
    for delay_ms, gain, cross in ((31, 0.12, True), (67, 0.07, False), (109, 0.035, True)):
        delay = round(delay_ms * SAMPLE_RATE / 1000)
        if delay >= len(output):
            continue
        delayed = dry[:-delay, ::-1] if cross else dry[:-delay]
        output[delay:] += delayed * gain * amount
    return output


def master(samples: np.ndarray, target_peak: float = 0.78) -> np.ndarray:
    output = samples - np.mean(samples, axis=0, keepdims=True)
    output = np.tanh(output * 1.18).astype(np.float32)
    peak = float(np.max(np.abs(output)))
    if peak:
        output *= target_peak / peak
    return output


def blank(duration_ms: float) -> np.ndarray:
    return np.zeros((round(duration_ms * SAMPLE_RATE / 1000), 2), dtype=np.float32)


def place(output: np.ndarray, samples: np.ndarray, start_ms: float, gain: float = 1.0) -> None:
    start = round(start_ms * SAMPLE_RATE / 1000)
    length = min(len(samples), len(output) - start)
    if start < 0 or length <= 0:
        return
    output[start : start + length] += samples[:length] * gain


def build_candidates(source_dir: Path) -> list[Candidate]:
    click = read_audio(source_dir / "radio-click.mp3")
    buzz = read_audio(source_dir / "radio-buzz-squelch.mp3")
    squelch = read_audio(source_dir / "radio-signoff-squelch.mp3")
    walkie = read_audio(source_dir / "walkie-talkie-beep.mp3")

    raw_walkie = master(fade(trim(walkie, 72, 735), 3, 24), 0.74)

    pitwall = blank(620)
    place(pitwall, trim(click, 0, 58), 0, 0.78)
    place(pitwall, fade(trim(squelch[::-1], 1010, 1214), 2, 24), 12, 0.34)
    place(pitwall, fade(trim(walkie, 92, 560), 3, 35), 94, 0.82)
    pitwall = master(room(bandpass(pitwall, 170, 8_500), 0.22), 0.76)

    race_radio = blank(420)
    place(race_radio, trim(click, 0, 58), 0, 0.95)
    place(race_radio, fade(trim(buzz, 0, 112), 1, 28), 18, 0.72)
    place(race_radio, fade(trim(squelch[::-1], 1008, 1214), 2, 34), 86, 0.40)
    race_radio = master(bandpass(race_radio, 190, 7_600), 0.76)

    tactical = blank(760)
    place(tactical, trim(click, 0, 58), 0, 0.72)
    place(tactical, fade(trim(buzz, 0, 105), 2, 20), 12, 0.28)
    place(tactical, fade(trim(walkie, 72, 735), 3, 34), 72, 0.94)
    tactical = master(room(bandpass(tactical, 150, 9_000), 0.38), 0.78)

    return [
        Candidate(
            "01-original-walkie-call.wav",
            "Original walkie call",
            "License-cleared walkie-talkie source with only trimming and level matching.",
            raw_walkie,
        ),
        Candidate(
            "02-pitwall-call.wav",
            "Pit-wall call",
            "PTT click, short channel-open texture, and a restrained tactical call.",
            pitwall,
        ),
        Candidate(
            "03-race-radio-open.wav",
            "Race radio open",
            "Non-melodic PTT and squelch cue for the most realistic race-radio behavior.",
            race_radio,
        ),
        Candidate(
            "04-tactical-comms-call.wav",
            "Tactical comms call",
            "A fuller game-comms cue using a non-copied call pattern and real radio texture.",
            tactical,
        ),
    ]


def write_html(output_dir: Path, candidates: list[Candidate]) -> None:
    rows = []
    for index, candidate in enumerate(candidates, start=1):
        duration_ms = round(len(candidate.samples) * 1000 / SAMPLE_RATE)
        rows.append(
            "<article>"
            f"<span>{index:02d}</span>"
            "<div>"
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
<title>Sample-based radio cue candidates</title>
<style>
:root { color-scheme: dark; font-family: Segoe UI, Arial, sans-serif; background: #070a0c; color: #edf5f7; }
body { margin: 0 auto; padding: 28px; max-width: 1080px; }
header { border-bottom: 1px solid #334149; padding-bottom: 18px; margin-bottom: 18px; }
h1 { font-size: 26px; margin: 0 0 8px; }
header p, article p { color: #a8bac2; margin: 0; line-height: 1.45; }
article { display: grid; grid-template-columns: 48px minmax(260px, 1fr) minmax(280px, 440px); align-items: center; gap: 18px; padding: 20px 0; border-bottom: 1px solid #253139; }
article > span { font: 700 18px Consolas, monospace; color: #77d8eb; }
h2 { margin: 0 0 5px; font-size: 18px; }
small { display: block; margin-top: 8px; color: #748b94; }
audio { width: 100%; height: 42px; }
@media (max-width: 760px) { body { padding: 18px; } article { grid-template-columns: 40px 1fr; } audio { grid-column: 2; } }
</style>
</head>
<body>
<header><h1>Sample-based radio cue candidates</h1><p>Provide independently license-cleared source recordings. Candidate 01 is the minimally processed control.</p></header>
""" + "\n".join(rows) + """
</body>
</html>
"""
    (output_dir / "comparison.html").write_text(page, encoding="utf-8")


def main() -> None:
    parser = argparse.ArgumentParser(description="Build sample-based radio cue candidates")
    parser.add_argument("source_dir", type=Path)
    parser.add_argument("output_dir", type=Path)
    args = parser.parse_args()
    output_dir = args.output_dir.resolve()
    output_dir.mkdir(parents=True, exist_ok=True)

    candidates = build_candidates(args.source_dir.resolve())
    manifest = []
    for candidate in candidates:
        pcm = np.round(np.clip(candidate.samples, -1, 1) * 32767).astype("<i2")
        with wave.open(str(output_dir / candidate.file_name), "wb") as target:
            target.setnchannels(2)
            target.setsampwidth(2)
            target.setframerate(SAMPLE_RATE)
            target.writeframes(pcm.tobytes())
        manifest.append({
            "file": candidate.file_name,
            "title": candidate.title,
            "description": candidate.description,
            "durationMs": round(len(candidate.samples) * 1000 / SAMPLE_RATE),
        })
    (output_dir / "manifest.json").write_text(
        json.dumps(manifest, indent=2, ensure_ascii=True) + "\n",
        encoding="ascii",
    )
    write_html(output_dir, candidates)


if __name__ == "__main__":
    main()
