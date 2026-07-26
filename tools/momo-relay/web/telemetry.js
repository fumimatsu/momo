(function initTelemetryModule(root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
    return;
  }
  root.FpvTelemetry = api;
}(typeof globalThis !== 'undefined' ? globalThis : this, () => {
  'use strict';

  const TELEMETRY_PREFIX = 'TEL:';
  const MAX_WIRE_BYTES = 256;
  const UINT32_HALF = 0x80000000;
  const UNIT_NORM_MIN = 0.98;
  const UNIT_NORM_MAX = 1.02;

  function isPlainObject(value) {
    return Boolean(value) && typeof value === 'object' && !Array.isArray(value);
  }

  function hasOwn(value, key) {
    return Object.prototype.hasOwnProperty.call(value, key);
  }

  function isNumberInRange(value, min, max) {
    return Number.isFinite(value) && value >= min && value <= max;
  }

  function isIntegerInRange(value, min, max) {
    return Number.isInteger(value) && value >= min && value <= max;
  }

  function isVector(value, length, min, max) {
    return Array.isArray(value)
      && value.length === length
      && value.every((item) => isNumberInRange(item, min, max));
  }

  function hasUnitNorm(value) {
    const norm = Math.hypot(...value);
    return norm >= UNIT_NORM_MIN && norm <= UNIT_NORM_MAX;
  }

  function getUtf8ByteLength(value) {
    if (typeof TextEncoder === 'function') {
      return new TextEncoder().encode(value).byteLength;
    }
    return value.length;
  }

  function validateCommon(payload) {
    if (!['s', 'e'].includes(payload.k)) {
      return 'kind';
    }
    if (typeof payload.src !== 'string' || !/^[A-Za-z0-9._-]{1,16}$/.test(payload.src)) {
      return 'source';
    }
    if (typeof payload.boot !== 'string' || !/^[0-9a-fA-F]{8}$/.test(payload.boot)) {
      return 'boot';
    }
    if (!isIntegerInRange(payload.seq, 0, 0xffffffff)) {
      return 'sequence';
    }
    if (!Number.isSafeInteger(payload.t_us) || payload.t_us < 0) {
      return 'timestamp';
    }
    return '';
  }

  function validateState(payload) {
    if (!isPlainObject(payload.imu) || !isPlainObject(payload.att) || !isPlainObject(payload.qual)) {
      return 'state_fields';
    }
    if (hasOwn(payload, 'evt')) {
      return 'state_event_mix';
    }
    if (!isVector(payload.imu.a, 3, -1000, 1000)) {
      return 'acceleration';
    }
    if (!isVector(payload.imu.g, 3, -100, 100)) {
      return 'angular_velocity';
    }
    if (!isVector(payload.att.q, 4, -1, 1) || !hasUnitNorm(payload.att.q)) {
      return 'quaternion';
    }
    if (!isVector(payload.att.rpy, 3, -Math.PI, Math.PI)) {
      return 'attitude';
    }
    if (!isIntegerInRange(payload.qual.period_us, 10000, 1000000)) {
      return 'period';
    }
    if (!isIntegerInRange(payload.qual.cal, 0, 3)) {
      return 'calibration';
    }
    if (!Array.isArray(payload.qual.flags)
        || payload.qual.flags.length > 8
        || new Set(payload.qual.flags).size !== payload.qual.flags.length
        || !payload.qual.flags.every((flag) => typeof flag === 'string'
          && /^[a-z0-9_]{1,24}$/.test(flag))) {
      return 'quality_flags';
    }
    return '';
  }

  function validateEvent(payload) {
    if (hasOwn(payload, 'imu') || hasOwn(payload, 'att') || hasOwn(payload, 'qual')) {
      return 'event_state_mix';
    }
    if (!isPlainObject(payload.evt)
        || typeof payload.evt.name !== 'string'
        || !/^[a-z][a-z0-9_]{0,31}$/.test(payload.evt.name)
        || !isPlainObject(payload.evt.data)) {
      return 'event_fields';
    }
    if (payload.evt.name === 'impact') {
      if (!isNumberInRange(payload.evt.data.mag_mps2, 0, 1000)) {
        return 'impact_magnitude';
      }
      if (!isVector(payload.evt.data.axis, 3, -1, 1) || !hasUnitNorm(payload.evt.data.axis)) {
        return 'impact_axis';
      }
    }
    return '';
  }

  function parseTelemetryMessage(message) {
    if (typeof message !== 'string' || !message.startsWith(TELEMETRY_PREFIX)) {
      return { status: 'not_telemetry' };
    }
    if (getUtf8ByteLength(message) > MAX_WIRE_BYTES) {
      return { status: 'invalid', reason: 'size' };
    }

    const body = message.slice(TELEMETRY_PREFIX.length).replace(/\r$/, '');
    if (!body.startsWith('{')) {
      return { status: 'legacy', raw: message };
    }

    let payload;
    try {
      payload = JSON.parse(body);
    } catch (_error) {
      return { status: 'invalid', reason: 'json' };
    }
    if (!isPlainObject(payload)) {
      return { status: 'invalid', reason: 'payload' };
    }
    if (payload.v !== 1) {
      return { status: 'unknown_version', version: payload.v ?? null };
    }

    const commonError = validateCommon(payload);
    if (commonError) {
      return { status: 'invalid', reason: commonError };
    }
    const bodyError = payload.k === 's' ? validateState(payload) : validateEvent(payload);
    if (bodyError) {
      return { status: 'invalid', reason: bodyError };
    }
    return { status: 'valid', payload };
  }

  function classifySequence(previous, current) {
    if (!previous) {
      return { status: 'initial', missing: 0 };
    }
    if (previous.boot !== current.boot) {
      return { status: 'new_boot', missing: 0 };
    }

    const delta = (current.seq - previous.seq) >>> 0;
    if (delta === 0) {
      return { status: 'duplicate', missing: 0 };
    }
    if (delta >= UINT32_HALF) {
      return { status: 'out_of_order', missing: 0 };
    }
    if (current.t_us < previous.t_us) {
      return { status: 'time_fault', missing: 0 };
    }
    if (delta === 1) {
      return { status: 'in_order', missing: 0 };
    }
    return { status: 'gap', missing: delta - 1 };
  }

  function getStaleThresholdMs(periodUs) {
    return Math.max(250, (periodUs / 1000) * 3);
  }

  function createCounters() {
    return {
      valid: 0,
      state: 0,
      event: 0,
      legacy: 0,
      invalid: 0,
      unknownVersion: 0,
      duplicate: 0,
      outOfOrder: 0,
      timeFault: 0,
      gaps: 0,
      missing: 0,
      newBoot: 0,
    };
  }

  class TelemetryTracker {
    constructor(options = {}) {
      this.now = options.now || (() => performance.now());
      this.streams = new Map();
      this.counters = createCounters();
      this.lastResult = { status: 'none' };
    }

    ingest(message, arrivalMs = this.now()) {
      const parsed = parseTelemetryMessage(message);
      if (parsed.status !== 'valid') {
        if (parsed.status === 'legacy') {
          this.counters.legacy += 1;
        } else if (parsed.status === 'unknown_version') {
          this.counters.unknownVersion += 1;
        } else if (parsed.status === 'invalid') {
          this.counters.invalid += 1;
        }
        this.lastResult = { ...parsed, accepted: false, arrivalMs };
        return this.lastResult;
      }

      const payload = parsed.payload;
      const previous = this.streams.get(payload.src) || null;
      const sequence = classifySequence(previous, payload);
      if (['duplicate', 'out_of_order', 'time_fault'].includes(sequence.status)) {
        if (sequence.status === 'duplicate') {
          this.counters.duplicate += 1;
        } else if (sequence.status === 'out_of_order') {
          this.counters.outOfOrder += 1;
        } else {
          this.counters.timeFault += 1;
        }
        this.lastResult = {
          status: sequence.status,
          accepted: false,
          payload,
          missing: 0,
          arrivalMs,
        };
        return this.lastResult;
      }

      const stream = sequence.status === 'new_boot' || !previous
        ? { src: payload.src, state: null, event: null, lastStateAt: 0, periodUs: null }
        : { ...previous };
      stream.boot = payload.boot;
      stream.seq = payload.seq;
      stream.t_us = payload.t_us;
      stream.lastMessageAt = arrivalMs;
      if (payload.k === 's') {
        stream.state = payload;
        stream.lastStateAt = arrivalMs;
        stream.periodUs = payload.qual.period_us;
        this.counters.state += 1;
      } else {
        stream.event = payload;
        this.counters.event += 1;
      }
      this.streams.set(payload.src, stream);

      this.counters.valid += 1;
      if (sequence.status === 'gap') {
        this.counters.gaps += 1;
        this.counters.missing += sequence.missing;
      } else if (sequence.status === 'new_boot') {
        this.counters.newBoot += 1;
      }

      this.lastResult = {
        status: 'accepted',
        accepted: true,
        sequenceStatus: sequence.status,
        missing: sequence.missing,
        payload,
        arrivalMs,
      };
      return this.lastResult;
    }

    getSnapshot(nowMs = this.now()) {
      const streams = Array.from(this.streams.values()).map((stream) => {
        const stateAgeMs = stream.state ? Math.max(0, nowMs - stream.lastStateAt) : null;
        const staleThresholdMs = stream.state ? getStaleThresholdMs(stream.periodUs) : null;
        return {
          ...stream,
          stateAgeMs,
          staleThresholdMs,
          stale: !stream.state || stateAgeMs > staleThresholdMs,
        };
      });
      const primary = streams
        .filter((stream) => stream.state)
        .sort((left, right) => right.lastStateAt - left.lastStateAt)[0] || null;
      return {
        counters: { ...this.counters },
        streams,
        primary,
        lastResult: this.lastResult,
      };
    }

    reset() {
      this.streams.clear();
      this.counters = createCounters();
      this.lastResult = { status: 'none' };
    }
  }

  function encodeTelemetry(payload) {
    const message = `${TELEMETRY_PREFIX}${JSON.stringify(payload)}`;
    if (getUtf8ByteLength(message) > MAX_WIRE_BYTES) {
      throw new RangeError('telemetry message exceeds 256 bytes');
    }
    return message;
  }

  class TelemetryMockGenerator {
    constructor(options = {}) {
      this.src = options.src || 'imu0';
      this.boot = options.boot || 'c0de0001';
      this.periodMs = Math.max(10, Math.min(1000, options.periodMs || 50));
      this.seq = options.seq || 0;
      this.startedAt = null;
    }

    getTimestampUs(nowMs) {
      if (this.startedAt === null) {
        this.startedAt = nowMs;
      }
      return Math.max(0, Math.round((nowMs - this.startedAt) * 1000));
    }

    takeSequence() {
      const current = this.seq >>> 0;
      this.seq = (current + 1) >>> 0;
      return current;
    }

    nextState(nowMs) {
      const tUs = this.getTimestampUs(nowMs);
      const phase = tUs / 1000000;
      const yaw = Math.sin(phase * 0.7) * 0.4;
      const halfYaw = yaw / 2;
      return encodeTelemetry({
        v: 1,
        k: 's',
        src: this.src,
        boot: this.boot,
        seq: this.takeSequence(),
        t_us: tUs,
        imu: {
          a: [0.1, Number((Math.sin(phase) * 3.5).toFixed(3)), 9.807],
          g: [0, 0, Number((Math.cos(phase * 0.7) * 0.28).toFixed(3))],
        },
        att: {
          q: [Number(Math.cos(halfYaw).toFixed(6)), 0, 0, Number(Math.sin(halfYaw).toFixed(6))],
          rpy: [0, 0, Number(yaw.toFixed(6))],
        },
        qual: {
          period_us: Math.round(this.periodMs * 1000),
          cal: 3,
          flags: [],
        },
      });
    }

    nextImpact(nowMs, magnitude = 24.8) {
      return encodeTelemetry({
        v: 1,
        k: 'e',
        src: this.src,
        boot: this.boot,
        seq: this.takeSequence(),
        t_us: this.getTimestampUs(nowMs),
        evt: {
          name: 'impact',
          data: { mag_mps2: magnitude, axis: [1, 0, 0] },
        },
      });
    }
  }

  return {
    MAX_WIRE_BYTES,
    TELEMETRY_PREFIX,
    TelemetryMockGenerator,
    TelemetryTracker,
    classifySequence,
    encodeTelemetry,
    getStaleThresholdMs,
    parseTelemetryMessage,
  };
}));
