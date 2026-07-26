(() => {
  'use strict';

  const FRAME_SAMPLES = 160;
  const SAMPLE_RATE = 8000;
  const PACKET_BYTES = 84;
  const START_BUFFER_FRAMES = 4;
  const MAX_GAP_FRAMES = 12;
  const MAX_SCHEDULE_AHEAD_SECONDS = 0.3;
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
      this.enabled = false;
      this.bootId = '';
      this.lastSequence = null;
      this.pending = [];
      this.nextPlaybackTime = 0;
      this.received = 0;
      this.invalid = 0;
      this.gaps = 0;
      this.resets = 0;
      this.scheduled = 0;
      this.notify();
    }

    snapshot() {
      return { enabled: this.enabled, contextState: this.context?.state || 'none', received: this.received, invalid: this.invalid, gaps: this.gaps, resets: this.resets, scheduled: this.scheduled, pending: this.pending.length };
    }

    getStatus() {
      return `${this.enabled ? (this.context?.state || 'starting') : 'off'} rx:${this.received} gap:${this.gaps} bad:${this.invalid}`;
    }

    notify() { this.onState(this.snapshot(), this.getStatus()); }

    async setEnabled(enabled) {
      if (!enabled) {
        this.enabled = false;
        this.pending = [];
        this.nextPlaybackTime = 0;
        this.notify();
        return true;
      }
      const AudioContextCtor = window.AudioContext || window.webkitAudioContext;
      if (!AudioContextCtor) return false;
      this.context = this.context || new AudioContextCtor({ sampleRate: SAMPLE_RATE });
      try {
        await this.context.resume();
      } catch (_) {
        this.notify();
        return false;
      }
      this.enabled = this.context.state === 'running';
      this.pending = [];
      this.nextPlaybackTime = 0;
      this.notify();
      return this.enabled;
    }

    handle(message) {
      const frame = parseFrame(message);
      if (!frame) return false;
      if (frame.invalid) {
        this.invalid += 1;
        this.notify();
        return true;
      }
      this.received += 1;
      if (this.bootId !== frame.bootId || (this.lastSequence !== null && frame.sequence <= this.lastSequence)) {
        this.bootId = frame.bootId;
        this.lastSequence = null;
        this.pending = [];
        this.nextPlaybackTime = 0;
        this.resets += 1;
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
        this.nextPlaybackTime = 0;
        this.resets += 1;
      }
      if (!this.nextPlaybackTime && this.pending.length < START_BUFFER_FRAMES) return;
      if (this.nextPlaybackTime && this.nextPlaybackTime - this.context.currentTime > MAX_SCHEDULE_AHEAD_SECONDS) {
        this.pending = [];
        this.nextPlaybackTime = 0;
        this.resets += 1;
        return;
      }
      let startAt = this.nextPlaybackTime || this.context.currentTime + (START_BUFFER_FRAMES * FRAME_SAMPLES / SAMPLE_RATE);
      while (this.pending.length > 0) {
        const samples = this.pending.shift();
        const buffer = this.context.createBuffer(1, FRAME_SAMPLES, SAMPLE_RATE);
        if (samples) {
          const output = buffer.getChannelData(0);
          for (let index = 0; index < FRAME_SAMPLES; index += 1) output[index] = samples[index] / 32768;
        }
        const source = this.context.createBufferSource();
        source.buffer = buffer;
        source.connect(this.context.destination);
        source.start(startAt);
        startAt += FRAME_SAMPLES / SAMPLE_RATE;
        this.scheduled += 1;
      }
      this.nextPlaybackTime = startAt;
    }
  }

  window.MomoM5Audio = Object.freeze({ createPlayer: (options) => new Player(options), toText });
})();
