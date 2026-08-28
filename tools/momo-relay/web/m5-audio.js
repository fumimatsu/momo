(() => {
  'use strict';

  const FRAME_SAMPLES = 160;
  const SAMPLE_RATE = 8000;
  const PACKET_BYTES = 84;
  const FRAME_DURATION_SECONDS = FRAME_SAMPLES / SAMPLE_RATE;
  // クロック補正を維持したまま Pilot の操作音遅延を減らすため、140 ms を先読みする。
  // 実機では underrun、到着周期の p95/p99、映像との同期を200 ms基準と比較する。
  const START_BUFFER_FRAMES = 7;
  const TARGET_BUFFER_SECONDS = START_BUFFER_FRAMES * FRAME_DURATION_SECONDS;
  const RESTART_SAFETY_SECONDS = 0.03;
  const MAX_GAP_FRAMES = 12;
  const MAX_SCHEDULE_AHEAD_SECONDS = 0.6;
  const STATE_NOTIFY_INTERVAL_MS = 250;
  const ARRIVAL_WINDOW_SAMPLES = 256;
  // M5 のサンプリングクロックと AudioContext のクロックは完全には一致しない。
  // 到着周期とバッファ残量から再生速度を小さく補正し、長時間走行時の周期的な
  // underrun を防ぐ。補正幅は音程差が目立たない ±1.5% に制限する。
  const PLAYBACK_RATE_MIN = 0.985;
  const PLAYBACK_RATE_MAX = 1.015;
  const ARRIVAL_RATE_SMOOTHING = 0.02;
  const MIN_RATE_SAMPLE_MS = FRAME_DURATION_SECONDS * 1000 * 0.5;
  const MAX_RATE_SAMPLE_MS = FRAME_DURATION_SECONDS * 1000 * 1.5;
  const BUFFER_RATE_GAIN = 0.05;
  const MAX_BUFFER_RATE_CORRECTION = 0.005;
  const IMA_INDEX_TABLE = [-1, -1, -1, -1, 2, 4, 6, 8, -1, -1, -1, -1, 2, 4, 6, 8];
  const IMA_STEP_TABLE = [
    7, 8, 9, 10, 11, 12, 13, 14, 16, 17, 19, 21, 23, 25, 28, 31, 34, 37, 41, 45, 50,
    55, 60, 66, 73, 80, 88, 97, 107, 118, 130, 143, 157, 173, 190, 209, 230, 253, 279,
    307, 337, 371, 408, 449, 494, 544, 598, 658, 724, 796, 876, 963, 1060, 1166, 1282,
    1411, 1552, 1707, 1878, 2066, 2272, 2499, 2749, 3024, 3327, 3660, 4026, 4428, 4871,
    5358, 5894, 6484, 7132, 7845, 8630, 9493, 10442, 11487, 12635, 13899, 15289, 16818,
    18500, 20350, 22385, 24623, 27086, 29794, 32767,
  ];
  const textDecoder = new TextDecoder('utf-8', { fatal: true });

  const clamp = (value, min, max) => Math.max(min, Math.min(max, value));
  const nowMs = () => globalThis.performance?.now?.() ?? Date.now();

  function toText(message) {
    if (typeof message === 'string') return message;
    try {
      if (message instanceof ArrayBuffer) return textDecoder.decode(new Uint8Array(message));
      if (ArrayBuffer.isView(message)) return textDecoder.decode(new Uint8Array(message.buffer, message.byteOffset, message.byteLength));
    } catch (_) {
      return null;
    }
    return null;
  }

  function decodeImaFrame(packet) {
    if (packet.length !== PACKET_BYTES || packet[2] > 88) return null;
    let predictor = packet[0] | (packet[1] << 8);
    if (predictor & 0x8000) predictor -= 0x10000;
    let stepIndex = packet[2];
    const decodeNibble = (nibble) => {
      const step = IMA_STEP_TABLE[stepIndex];
      let difference = step >> 3;
      if (nibble & 4) difference += step;
      if (nibble & 2) difference += step >> 1;
      if (nibble & 1) difference += step >> 2;
      predictor = clamp(predictor + ((nibble & 8) ? -difference : difference), -32768, 32767);
      stepIndex = clamp(stepIndex + IMA_INDEX_TABLE[nibble & 0x0f], 0, 88);
      return predictor;
    };
    const samples = new Int16Array(FRAME_SAMPLES);
    samples[0] = predictor;
    let sampleIndex = 1;
    for (let packetIndex = 4; packetIndex < packet.length && sampleIndex < FRAME_SAMPLES; packetIndex += 1) {
      const packed = packet[packetIndex];
      samples[sampleIndex++] = decodeNibble(packed & 0x0f);
      if (sampleIndex < FRAME_SAMPLES) samples[sampleIndex++] = decodeNibble((packed >> 4) & 0x0f);
    }
    return sampleIndex === FRAME_SAMPLES ? samples : null;
  }

  function parseFrame(message) {
    const text = toText(message);
    if (!text || !text.startsWith('AUD:')) return null;
    const match = /^AUD:1,([0-9a-f]{8}),(\d+),8,ima,([A-Za-z0-9+/]+={0,2})$/i.exec(text.trim());
    if (!match) return { invalid: true };
    try {
      const raw = atob(match[3]);
      const packet = Uint8Array.from(raw, (value) => value.charCodeAt(0));
      const samples = decodeImaFrame(packet);
      return samples ? { bootId: match[1].toLowerCase(), sequence: Number(match[2]), samples } : { invalid: true };
    } catch (_) {
      return { invalid: true };
    }
  }

  class Player {
    constructor(options = {}) {
      this.onState = typeof options.onState === 'function' ? options.onState : () => {};
      this.context = null;
      this.outputGain = null;
      this.outputLevel = 1;
      this.enabled = false;
      this.bootId = '';
      this.lastSequence = null;
      this.pending = [];
      this.nextPlaybackTime = 0;
      this.received = 0;
      this.invalid = 0;
      this.gaps = 0;
      this.resets = 0;
      this.underruns = 0;
      this.overruns = 0;
      this.scheduled = 0;
      this.cancelledSources = 0;
      this.scheduledSources = new Map();
      this.lastFrameAt = 0;
      this.lastInterArrivalMs = 0;
      this.maxInterArrivalMs = 0;
      this.interArrivalWindow = new Float64Array(ARRIVAL_WINDOW_SAMPLES);
      this.interArrivalWindowCount = 0;
      this.interArrivalWindowIndex = 0;
      this.lastNotifyAt = Number.NEGATIVE_INFINITY;
      this.arrivalPlaybackRate = 1;
      this.playbackRate = 1;
      this.bufferLeadSeconds = 0;
      this.notify(true);
    }

    snapshot() {
      return {
        enabled: this.enabled,
        contextState: this.context?.state || 'none',
        outputLevel: this.outputLevel,
        received: this.received,
        invalid: this.invalid,
        gaps: this.gaps,
        resets: this.resets,
        underruns: this.underruns,
        overruns: this.overruns,
        scheduled: this.scheduled,
        cancelledSources: this.cancelledSources,
        activeScheduledSources: this.scheduledSources.size,
        pending: this.pending.length,
        lastInterArrivalMs: this.lastInterArrivalMs,
        maxInterArrivalMs: this.maxInterArrivalMs,
        interArrivalP95Ms: this.getInterArrivalPercentile(0.95),
        interArrivalP99Ms: this.getInterArrivalPercentile(0.99),
        arrivalPlaybackRate: this.arrivalPlaybackRate,
        playbackRate: this.playbackRate,
        targetBufferMs: TARGET_BUFFER_SECONDS * 1000,
        bufferLeadMs: this.bufferLeadSeconds * 1000,
      };
    }

    getStatus() {
      return `${this.enabled ? (this.context?.state || 'starting') : 'off'} rx:${this.received} gap:${this.gaps} under:${this.underruns} bad:${this.invalid}`;
    }

    notify(force = false) {
      const now = globalThis.performance?.now?.() ?? Date.now();
      if (!force && now - this.lastNotifyAt < STATE_NOTIFY_INTERVAL_MS) return;
      this.lastNotifyAt = now;
      this.onState(this.snapshot(), this.getStatus());
    }

    async setEnabled(enabled) {
      if (!enabled) {
        this.enabled = false;
        this.cancelScheduledAudio();
        this.pending = [];
        this.nextPlaybackTime = 0;
        this.bufferLeadSeconds = 0;
        this.notify(true);
        return true;
      }
      const AudioContextCtor = window.AudioContext || window.webkitAudioContext;
      if (!AudioContextCtor) return false;
      this.context = this.context || new AudioContextCtor({ sampleRate: SAMPLE_RATE });
      if (!this.outputGain) {
        this.outputGain = this.context.createGain();
        this.outputGain.gain.value = this.outputLevel;
        this.outputGain.connect(this.context.destination);
      }
      try {
        await this.context.resume();
      } catch (_) {
        this.notify(true);
        return false;
      }
      this.enabled = this.context.state === 'running';
      this.cancelScheduledAudio();
      this.pending = [];
      this.nextPlaybackTime = 0;
      this.bufferLeadSeconds = 0;
      this.notify(true);
      return this.enabled;
    }

    setOutputGain(value, rampMs = 0) {
      this.outputLevel = clamp(Number(value) || 0, 0, 1);
      if (!this.outputGain || !this.context) {
        this.notify(true);
        return;
      }
      const now = this.context.currentTime;
      this.outputGain.gain.cancelScheduledValues(now);
      this.outputGain.gain.setValueAtTime(this.outputGain.gain.value, now);
      this.outputGain.gain.linearRampToValueAtTime(
        this.outputLevel,
        now + Math.max(0, Number(rampMs) || 0) / 1000,
      );
      this.notify(true);
    }

    resetArrivalClock(receivedAt) {
      this.lastFrameAt = receivedAt;
      this.lastInterArrivalMs = 0;
      this.interArrivalWindowCount = 0;
      this.interArrivalWindowIndex = 0;
      this.arrivalPlaybackRate = 1;
      this.playbackRate = 1;
    }

    recordInterArrival(perFrameMs) {
      this.interArrivalWindow[this.interArrivalWindowIndex] = perFrameMs;
      this.interArrivalWindowIndex = (this.interArrivalWindowIndex + 1) % ARRIVAL_WINDOW_SAMPLES;
      this.interArrivalWindowCount = Math.min(
        ARRIVAL_WINDOW_SAMPLES,
        this.interArrivalWindowCount + 1,
      );
    }

    getInterArrivalPercentile(percentile) {
      if (this.interArrivalWindowCount === 0) return 0;
      const samples = Array.from(
        this.interArrivalWindow.subarray(0, this.interArrivalWindowCount),
      ).sort((left, right) => left - right);
      const index = Math.min(
        samples.length - 1,
        Math.max(0, Math.ceil(samples.length * percentile) - 1),
      );
      return samples[index];
    }

    updateArrivalClock(receivedAt, sequenceDelta) {
      if (this.lastFrameAt <= 0 || sequenceDelta <= 0) {
        this.lastFrameAt = receivedAt;
        return;
      }
      const interArrivalMs = Math.max(0, receivedAt - this.lastFrameAt);
      this.lastFrameAt = receivedAt;
      this.lastInterArrivalMs = interArrivalMs;
      this.maxInterArrivalMs = Math.max(this.maxInterArrivalMs, interArrivalMs);
      const perFrameMs = interArrivalMs / sequenceDelta;
      this.recordInterArrival(perFrameMs);
      // 長い停止と、その直後のバースト到着はクロック推定へ混ぜない。
      if (perFrameMs < MIN_RATE_SAMPLE_MS || perFrameMs > MAX_RATE_SAMPLE_MS) return;
      const observedRate = clamp(
        (FRAME_DURATION_SECONDS * 1000) / perFrameMs,
        PLAYBACK_RATE_MIN,
        PLAYBACK_RATE_MAX,
      );
      this.arrivalPlaybackRate += ARRIVAL_RATE_SMOOTHING
        * (observedRate - this.arrivalPlaybackRate);
    }

    getAdaptivePlaybackRate() {
      if (!this.context || !this.nextPlaybackTime) {
        this.playbackRate = this.arrivalPlaybackRate;
        return this.playbackRate;
      }
      const lead = Math.max(0, this.nextPlaybackTime - this.context.currentTime);
      const correction = clamp(
        (lead - TARGET_BUFFER_SECONDS) * BUFFER_RATE_GAIN,
        -MAX_BUFFER_RATE_CORRECTION,
        MAX_BUFFER_RATE_CORRECTION,
      );
      this.playbackRate = clamp(
        this.arrivalPlaybackRate + correction,
        PLAYBACK_RATE_MIN,
        PLAYBACK_RATE_MAX,
      );
      return this.playbackRate;
    }

    cancelScheduledAudio() {
      for (const source of this.scheduledSources.keys()) {
        try {
          source.stop();
        } catch (_) {
          // ended source or a browser that already released the node
        }
        try {
          source.disconnect();
        } catch (_) {
          // disconnect is best-effort during reset
        }
        this.scheduledSources.delete(source);
        this.cancelledSources += 1;
      }
    }

    handle(message) {
      const frame = parseFrame(message);
      if (!frame) return false;
      if (frame.invalid) {
        this.invalid += 1;
        this.notify();
        return true;
      }
      const receivedAt = nowMs();
      this.received += 1;
      const resetClock = this.bootId !== frame.bootId
        || (this.lastSequence !== null && frame.sequence <= this.lastSequence);
      const sequenceDelta = this.lastSequence === null ? 0 : frame.sequence - this.lastSequence;
      if (resetClock) {
        this.bootId = frame.bootId;
        this.lastSequence = null;
        this.cancelScheduledAudio();
        this.pending = [];
        this.nextPlaybackTime = 0;
        this.bufferLeadSeconds = 0;
        this.resetArrivalClock(receivedAt);
        this.resets += 1;
      } else {
        this.updateArrivalClock(receivedAt, sequenceDelta);
      }
      if (this.lastSequence !== null && frame.sequence > this.lastSequence + 1) {
        const missing = Math.min(MAX_GAP_FRAMES, frame.sequence - this.lastSequence - 1);
        this.gaps += missing;
        if (this.enabled) this.pending.push(...Array(missing).fill(null));
      }
      this.lastSequence = frame.sequence;
      if (this.enabled) {
        this.pending.push(frame.samples);
        this.flush();
      }
      this.notify();
      return true;
    }

    flush() {
      if (!this.enabled || !this.context || this.context.state !== 'running') return;
      if (this.nextPlaybackTime && this.nextPlaybackTime <= this.context.currentTime) {
        // 音声途切れ後に過去の AudioContext 時刻へ source を積まない。
        // 次の数フレームをためてから再開する。
        this.cancelScheduledAudio();
        this.nextPlaybackTime = 0;
        this.bufferLeadSeconds = 0;
        this.resets += 1;
        this.underruns += 1;
      }
      if (!this.nextPlaybackTime && this.pending.length < START_BUFFER_FRAMES) return;
      if (this.nextPlaybackTime && this.nextPlaybackTime - this.context.currentTime > MAX_SCHEDULE_AHEAD_SECONDS) {
        this.cancelScheduledAudio();
        this.pending = [];
        this.nextPlaybackTime = 0;
        this.bufferLeadSeconds = 0;
        this.resets += 1;
        this.overruns += 1;
        return;
      }
      // START_BUFFER_FRAMES はすでに pending に貯まっているため、復帰時にさらに
      // 140 ms 待たせない。AudioContext の安全余裕だけ先から再生を再開する。
      let startAt = this.nextPlaybackTime || this.context.currentTime + RESTART_SAFETY_SECONDS;
      while (this.pending.length > 0) {
        const samples = this.pending.shift();
        const buffer = this.context.createBuffer(1, FRAME_SAMPLES, SAMPLE_RATE);
        if (samples) {
          const output = buffer.getChannelData(0);
          for (let index = 0; index < FRAME_SAMPLES; index += 1) output[index] = samples[index] / 32768;
        }
        const source = this.context.createBufferSource();
        source.buffer = buffer;
        const playbackRate = this.getAdaptivePlaybackRate();
        source.playbackRate.value = playbackRate;
        source.connect(this.outputGain || this.context.destination);
        this.scheduledSources.set(source, startAt);
        source.onended = () => this.scheduledSources.delete(source);
        source.start(startAt);
        startAt += FRAME_DURATION_SECONDS / playbackRate;
        this.nextPlaybackTime = startAt;
        this.scheduled += 1;
      }
      this.nextPlaybackTime = startAt;
      this.bufferLeadSeconds = Math.max(0, startAt - this.context.currentTime);
    }
  }

  window.MomoM5Audio = Object.freeze({ createPlayer: (options) => new Player(options), toText });
})();
