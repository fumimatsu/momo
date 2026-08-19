# Qwen3-TTS live runtime validation

- Status: open

## Context

Qwen3-TTS 0.6B CustomVoice can generate the 20 Japanese and 20 English race-commentary samples on the
RTX 5070 12 GB, but the upstream Windows BF16 / SDPA path is batch-only and too slow. The faster-qwen3-tts
1.7B CUDA Graph path subsequently reached about 0.3 second warm TTFA and 2.5x playback speed in both
languages. Production queue behavior, subjective quality, and Relay isolation still require validation.

## Goal

Determine whether a supported local runtime can deliver high-quality Japanese and English Qwen3-TTS audio
with bounded latency and output length, without delaying Relay control, video, telemetry, or race-state paths.

## Acceptance Criteria

- Warm streaming TTFA P95 is at most 500 ms for both Japanese and English on the target production host.
- Warm generation speed P50 is at least 1.5x playback speed for both languages.
- Four simultaneous arrivals have a documented queue policy and bounded high-priority wait time.
- No output exceeds twice the sequential audio duration for the same prompt without being rejected or retried.
- Pilot names, decimal lap times, PIT, BOOST, car numbers, and mixed abbreviations pass human listening review.
- A generation timeout, maximum audio duration, single retry, and Kokoro fallback are specified and tested.
- GPU memory and command/video/telemetry latency are measured while TTS is active.

## Verification

- Re-run the shared 20-prompt corpus with the faster-qwen3-tts CUDA Graph streaming backend on the target host.
- Repeat the four-request burst and compare wall time, client P50/P95, RTF, and duration-ratio flags.
- Listen to every generated sample in `comparison.html`; ASR back-check may assist but does not replace listening.
- Run the Relay race-audio E2E while monitoring RC command delay, video FPS, telemetry loss, and GPU utilization.

## Notes

- Keep Kokoro `am_michael` as the production default until all acceptance criteria pass.
- The RTX 5070 comparison passed the TTFA and generation-speed criteria with 1.7B; production integration is
  still blocked by the remaining listening, queue-policy, fallback, and Relay E2E criteria.
- Keep commentary-text generation independent from TTS synthesis so cache, priority, queueing, and fallback remain
  deterministic even if the text is produced by ChatGPT or another remote model.
