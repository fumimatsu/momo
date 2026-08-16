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
  const STANDARD_GRAVITY_MPS2 = 9.80665;
  const VEHICLE_FLU_AXES_FLAG = 'flu_axes';
  const RELAY_EVENT_HISTORY_LIMIT = 32;
  const DEFAULT_MOTION_OPTIONS = Object.freeze({
    cornerLateralStartMps2: 1.5,
    cornerLateralFullMps2: 6.0,
    cornerYawStartRadPerSec: 0.25,
    cornerYawFullRadPerSec: 1.2,
    cornerAttackSeconds: 0.08,
    cornerReleaseSeconds: 0.16,
    surfaceBaselineSeconds: 0.35,
    surfaceForwardWeight: 0.25,
    surfaceRoughnessStartMps2: 0.25,
    surfaceRoughnessFullMps2: 1.80,
    surfaceAttackSeconds: 0.06,
    surfaceReleaseSeconds: 0.22,
    impactForwardMps2: 8.5,
    impactVerticalMps2: 5.5,
    impactLateralMps2: 10.0,
    impactLateralYawMaxRadPerSec: 0.35,
    impactJerkMps3: 80.0,
    // M5 の impact_candidate は生の候補であり、通常走行中にも小さな値が混ざる。
    // このローカル段階化は診断・将来の路面表現用。公式HUD/FFB pulseはRelay確定イベントを使う。
    impactWeakMagnitudeMps2: 10.0,
    impactStrongMagnitudeMps2: 12.0,
    impactStrongJerkMps3: 250.0,
    impactSevereMagnitudeMps2: 15.0,
    // 大きな加速度ピークだけでは軽い壁接触も重衝撃に見えるため、HEAVY は急峻さも要求する。
    impactSevereJerkMps3: 750.0,
    impactRearmMagnitudeMps2: 5.0,
    impactRearmHoldMs: 500,
    // weak はグラベルや縁石の連続入力を表せるよう、完全な静止待ちをせず短い間隔で再通知する。
    impactWeakRepeatMs: 180,
    impactDisplayMs: 1800,
  });

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

  function validateStateV1(payload) {
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

  function validateEventV1(payload) {
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

  function validateStateV2(payload) {
    if (!isPlainObject(payload.q)) {
      return 'compact_state_fields';
    }
    if (hasOwn(payload, 'e')) {
      return 'compact_state_event_mix';
    }
    if (!isIntegerInRange(payload.q.p, 10000, 1000000)) {
      return 'compact_period';
    }
    if (!Array.isArray(payload.q.f)
        || payload.q.f.length > 8
        || new Set(payload.q.f).size !== payload.q.f.length
        || !payload.q.f.every((flag) => typeof flag === 'string'
          && /^[a-z0-9_]{1,24}$/.test(flag))) {
      return 'compact_quality_flags';
    }

    const hasMotion = isPlainObject(payload.m);
    const hasEsc = isPlainObject(payload.esc);
    if (hasMotion === hasEsc) {
      return 'compact_state_variant';
    }
    if (hasMotion) {
      if (hasOwn(payload.q, 'ok') || hasOwn(payload.q, 'age')) {
        return 'compact_motion_quality';
      }
      if (!isVector(payload.m.a, 3, -1000, 1000)) {
        return 'compact_acceleration';
      }
      if (!isNumberInRange(payload.m.y, -100, 100)) {
        return 'compact_yaw_rate';
      }
      return '';
    }

    if (typeof payload.q.ok !== 'boolean'
        || !isIntegerInRange(payload.q.age, 0, 65535)) {
      return 'compact_esc_quality';
    }
    const escFields = Object.keys(payload.esc);
    if (escFields.length === 0
        || escFields.some((field) => !['rpm', 'max', 'v', 'tc', 'tm', 'out'].includes(field))) {
      return 'compact_esc_fields';
    }
    if ((hasOwn(payload.esc, 'rpm') && !isIntegerInRange(payload.esc.rpm, 0, 2000000))
        || (hasOwn(payload.esc, 'max') && !isIntegerInRange(payload.esc.max, 0, 2000000))
        || (hasOwn(payload.esc, 'v') && !isNumberInRange(payload.esc.v, 0, 100))
        || (hasOwn(payload.esc, 'tc') && !isNumberInRange(payload.esc.tc, -100, 300))
        || (hasOwn(payload.esc, 'tm') && !isNumberInRange(payload.esc.tm, -100, 300))
        || (hasOwn(payload.esc, 'out') && !isIntegerInRange(payload.esc.out, 0, 1000))) {
      return 'compact_esc_value';
    }
    return '';
  }

  function validateEventV2(payload) {
    if (hasOwn(payload, 'm') || hasOwn(payload, 'q')) {
      return 'compact_event_state_mix';
    }
    if (!isPlainObject(payload.e)
        || typeof payload.e.n !== 'string'
        || !/^[a-z][a-z0-9_]{0,31}$/.test(payload.e.n)
        || !isNumberInRange(payload.e.m, 0, 1000)
        || !isVector(payload.e.a, 3, -1, 1)
        || !hasUnitNorm(payload.e.a)
        || !isNumberInRange(payload.e.j, 0, 100000)) {
      return 'compact_event_fields';
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
    if (![1, 2].includes(payload.v)) {
      return { status: 'unknown_version', version: payload.v ?? null };
    }

    const commonError = validateCommon(payload);
    if (commonError) {
      return { status: 'invalid', reason: commonError };
    }
    const bodyError = payload.v === 1
      ? (payload.k === 's' ? validateStateV1(payload) : validateEventV1(payload))
      : (payload.k === 's' ? validateStateV2(payload) : validateEventV2(payload));
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

  function clamp(value, min, max) {
    return Math.max(min, Math.min(max, value));
  }

  function approach(current, target, elapsedSeconds, attackSeconds, releaseSeconds) {
    const duration = target > current ? attackSeconds : releaseSeconds;
    if (!Number.isFinite(elapsedSeconds) || elapsedSeconds <= 0 || duration <= 0) {
      return target;
    }
    const alpha = 1 - Math.exp(-elapsedSeconds / duration);
    return current + ((target - current) * alpha);
  }

  function classifyImpact(magnitudeMps2, jerkMps3, options) {
    if (!Number.isFinite(magnitudeMps2) || magnitudeMps2 < options.impactWeakMagnitudeMps2) {
      return '';
    }
    if (magnitudeMps2 >= options.impactSevereMagnitudeMps2
        && Number.isFinite(jerkMps3)
        && jerkMps3 >= options.impactSevereJerkMps3) {
      return 'severe';
    }
    if (magnitudeMps2 >= options.impactStrongMagnitudeMps2
        && Number.isFinite(jerkMps3)
        && jerkMps3 >= options.impactStrongJerkMps3) {
      return 'strong';
    }
    return 'weak';
  }

  function impactClassRank(impactClass) {
    return ({ weak: 1, strong: 2, severe: 3 })[impactClass] || 0;
  }

  function impactClassLevel(impactClass) {
    return ({ weak: 0.34, strong: 0.68, severe: 1 })[impactClass] || 0;
  }

  function hasVehicleFluAxes(payload) {
    const flags = payload?.v === 2 ? payload?.q?.f : payload?.qual?.flags;
    return Array.isArray(flags) && flags.includes(VEHICLE_FLU_AXES_FLAG);
  }

  function getStatePeriodUs(payload) {
    return payload.v === 2 ? payload.q.p : payload.qual.period_us;
  }

  // M5StickS3 の現行取付: 車体 FLU = [sensor Z, sensor X, sensor Y]。
  // telemetry v1 の imu.a / imu.g は sensor 軸のままなので、Viewer 内で車体軸へ変換する。
  function deriveVehicleMotion(payload) {
    if (!payload || payload.k !== 's' || !hasVehicleFluAxes(payload)) {
      return null;
    }
    if (payload.v === 2) {
      const [forwardMps2, lateralMps2, verticalMps2] = payload.m.a;
      return { forwardMps2, lateralMps2, verticalMps2, yawRateRadPerSec: payload.m.y };
    }
    const [sensorX, sensorY, sensorZ] = payload.imu.a;
    const [, sensorGyroY] = payload.imu.g;
    return {
      forwardMps2: sensorZ,
      lateralMps2: sensorX,
      verticalMps2: sensorY - STANDARD_GRAVITY_MPS2,
      yawRateRadPerSec: sensorGyroY,
    };
  }

  function parseVehicleImpactEvent(value) {
    if (!isPlainObject(value)
        || value.type !== 'vehicle_event'
        || value.version !== 1
        || typeof value.eventId !== 'string' || !value.eventId
        || typeof value.raceRunId !== 'string'
        || typeof value.carId !== 'string' || !value.carId
        || !['weak', 'strong', 'severe'].includes(value.impactClass)
        || !isNumberInRange(value.magnitudeMps2, 0, 1000)
        || !isNumberInRange(value.jerkMps3, 0, 1000000)
        || !isVector(value.axis, 3, -2, 2)
        || typeof value.damageApplied !== 'boolean'
        || !isNumberInRange(value.damage, 0, 100)
        || !isNumberInRange(value.hpBefore, 0, 100)
        || !isNumberInRange(value.hpAfter, 0, 100)
        || !Number.isSafeInteger(value.serverTimeMs) || value.serverTimeMs < 0) {
      return null;
    }
    const suppressionReason = value.suppressionReason === undefined
      ? ''
      : String(value.suppressionReason);
    if (!['', 'cooldown', 'below_damage_threshold'].includes(suppressionReason)
        || (value.damageApplied && suppressionReason)
        || (!value.damageApplied && value.damage !== 0)) {
      return null;
    }
    return Object.freeze({
      type: 'vehicle_event',
      version: 1,
      eventId: value.eventId,
      raceRunId: value.raceRunId,
      carId: value.carId,
      impactClass: value.impactClass,
      magnitudeMps2: value.magnitudeMps2,
      jerkMps3: value.jerkMps3,
      axis: Object.freeze(value.axis.slice()),
      damageApplied: value.damageApplied,
      damage: value.damage,
      suppressionReason,
      hpBefore: value.hpBefore,
      hpAfter: value.hpAfter,
      serverTimeMs: value.serverTimeMs,
    });
  }

  function parseRelayEventMessage(message) {
    if (typeof message !== 'string') return null;
    let payload;
    try {
      payload = JSON.parse(message);
    } catch (_) {
      return null;
    }
    if (payload?.type === 'vehicle_event') {
      const event = parseVehicleImpactEvent(payload);
      return event ? { kind: 'live', event } : null;
    }
    if (!isPlainObject(payload)
        || payload.type !== 'vehicle_event_snapshot'
        || payload.version !== 1
        || typeof payload.raceRunId !== 'string'
        || !Array.isArray(payload.events)
        || payload.events.length > RELAY_EVENT_HISTORY_LIMIT) {
      return null;
    }
    const events = payload.events.map(parseVehicleImpactEvent);
    if (events.some((event) => !event)
        || events.some((event) => event.raceRunId !== payload.raceRunId)
        || new Set(events.map((event) => event.eventId)).size !== events.length) {
      return null;
    }
    return {
      kind: 'snapshot',
      raceRunId: payload.raceRunId,
      events: Object.freeze(events),
    };
  }

  class RelayEventInbox {
    constructor() {
      this.raceRunId = '';
      this.history = [];
      this.seen = new Set();
    }

    reset(raceRunId) {
      this.raceRunId = raceRunId;
      this.history = [];
      this.seen.clear();
    }

    ingest(message) {
      const parsed = parseRelayEventMessage(message);
      if (!parsed) return { status: 'invalid', transient: false };
      if (parsed.kind === 'snapshot') {
        this.reset(parsed.raceRunId);
        this.history = parsed.events.slice(-RELAY_EVENT_HISTORY_LIMIT);
        this.seen = new Set(this.history.map((event) => event.eventId));
        return {
          status: 'snapshot',
          transient: false,
          raceRunId: this.raceRunId,
          events: this.history.slice(),
        };
      }
      const event = parsed.event;
      if (event.raceRunId !== this.raceRunId) this.reset(event.raceRunId);
      if (this.seen.has(event.eventId)) {
        return { status: 'duplicate', transient: false, event };
      }
      this.seen.add(event.eventId);
      this.history.push(event);
      if (this.history.length > RELAY_EVENT_HISTORY_LIMIT) {
        this.history.shift();
      }
      return {
        status: 'live',
        transient: true,
        event,
        raceRunId: this.raceRunId,
        events: this.history.slice(),
      };
    }
  }

  class MotionFeatureExtractor {
    constructor(options = {}) {
      this.options = { ...DEFAULT_MOTION_OPTIONS, ...options };
      this.streams = new Map();
    }

    ingest(payload, arrivalMs) {
      if (payload?.k === 'e') {
        return this.ingestEvent(payload, arrivalMs);
      }
      const motion = deriveVehicleMotion(payload);
      if (!motion || !Number.isFinite(arrivalMs)) {
        return null;
      }

      const stored = this.streams.get(payload.src) || null;
      const previous = stored?.boot === payload.boot ? stored : null;
      const elapsedSeconds = previous
        ? clamp((payload.t_us - previous.tUs) / 1000000, 0.001, 0.25)
        : getStatePeriodUs(payload) / 1000000;
      const lateralMagnitude = Math.abs(motion.lateralMps2);
      const yawMagnitude = Math.abs(motion.yawRateRadPerSec);
      const sameTurnDirection = Math.sign(motion.lateralMps2) === Math.sign(motion.yawRateRadPerSec)
        && lateralMagnitude > 0
        && yawMagnitude > 0;
      const cornerRaw = sameTurnDirection
        ? clamp((lateralMagnitude - this.options.cornerLateralStartMps2)
            / (this.options.cornerLateralFullMps2 - this.options.cornerLateralStartMps2), 0, 1)
          * clamp((yawMagnitude - this.options.cornerYawStartRadPerSec)
            / (this.options.cornerYawFullRadPerSec - this.options.cornerYawStartRadPerSec), 0, 1)
        : 0;
      const cornerLoad = approach(
        previous?.cornerLoad || 0,
        cornerRaw,
        elapsedSeconds,
        this.options.cornerAttackSeconds,
        this.options.cornerReleaseSeconds,
      );

      const jerkMps3 = previous
        ? Math.hypot(
          motion.forwardMps2 - previous.motion.forwardMps2,
          motion.lateralMps2 - previous.motion.lateralMps2,
          motion.verticalMps2 - previous.motion.verticalMps2,
        ) / elapsedSeconds
        : 0;
      // 大きな横 G は通常の高速旋回でも起きる。横方向だけの候補はヨーがほぼ無い時に限定する。
      const lateralImpact = lateralMagnitude >= this.options.impactLateralMps2
        && yawMagnitude <= this.options.impactLateralYawMaxRadPerSec;
      const impactRaw = (Math.abs(motion.forwardMps2) >= this.options.impactForwardMps2
          || Math.abs(motion.verticalMps2) >= this.options.impactVerticalMps2
          || lateralImpact)
        && jerkMps3 >= this.options.impactJerkMps3;
      const dynamicMagnitudeMps2 = Math.hypot(
        motion.forwardMps2,
        motion.lateralMps2,
        motion.verticalMps2,
      );
      const surfaceForwardBaselineMps2 = approach(
        previous?.surfaceForwardBaselineMps2 ?? motion.forwardMps2,
        motion.forwardMps2,
        elapsedSeconds,
        this.options.surfaceBaselineSeconds,
        this.options.surfaceBaselineSeconds,
      );
      const surfaceVerticalBaselineMps2 = approach(
        previous?.surfaceVerticalBaselineMps2 ?? motion.verticalMps2,
        motion.verticalMps2,
        elapsedSeconds,
        this.options.surfaceBaselineSeconds,
        this.options.surfaceBaselineSeconds,
      );
      const surfaceDynamicMps2 = Math.hypot(
        motion.verticalMps2 - surfaceVerticalBaselineMps2,
        (motion.forwardMps2 - surfaceForwardBaselineMps2) * this.options.surfaceForwardWeight,
      );
      // 路面入力は衝突とは独立させる。衝突フレームでは路面側を減衰させ、二重出力を避ける。
      const surfaceRaw = impactRaw ? 0 : clamp(
        (surfaceDynamicMps2 - this.options.surfaceRoughnessStartMps2)
          / (this.options.surfaceRoughnessFullMps2 - this.options.surfaceRoughnessStartMps2),
        0,
        1,
      );
      const surfaceRoughness = approach(
        previous?.surfaceRoughness || 0,
        surfaceRaw,
        elapsedSeconds,
        this.options.surfaceAttackSeconds,
        this.options.surfaceReleaseSeconds,
      );
      let impactArmed = previous?.impactArmed !== false;
      let impactQuietSinceMs = previous?.impactQuietSinceMs ?? null;
      if (dynamicMagnitudeMps2 < this.options.impactRearmMagnitudeMps2) {
        impactQuietSinceMs ??= arrivalMs;
        if (!impactArmed
            && arrivalMs - impactQuietSinceMs >= this.options.impactRearmHoldMs) {
          impactArmed = true;
        }
      } else {
        impactQuietSinceMs = null;
      }

      const rawImpactClass = impactRaw
        ? classifyImpact(dynamicMagnitudeMps2, jerkMps3, this.options)
        : '';
      const previousImpactClass = previous?.lastImpactEvent?.impactClass || '';
      const isEscalation = impactClassRank(rawImpactClass) > impactClassRank(previousImpactClass);
      const weakRepeatReady = rawImpactClass === 'weak'
        && previousImpactClass === 'weak'
        && Number.isFinite(previous?.lastImpactAtMs)
        && arrivalMs - previous.lastImpactAtMs >= this.options.impactWeakRepeatMs;
      const impact = Boolean(rawImpactClass) && (impactArmed || isEscalation || weakRepeatReady);
      const impactLevel = impact ? impactClassLevel(rawImpactClass) : 0;
      const rawImpactEvent = impact ? {
        impactClass: rawImpactClass,
        magnitudeMps2: dynamicMagnitudeMps2,
        jerkMps3,
        axis: null,
        source: 'viewer_raw',
      } : null;
      const snapshot = {
        src: payload.src,
        boot: payload.boot,
        seq: payload.seq,
        tUs: payload.t_us,
        periodUs: getStatePeriodUs(payload),
        lastArrivalMs: arrivalMs,
        staleThresholdMs: getStaleThresholdMs(getStatePeriodUs(payload)),
        motion,
        jerkMps3,
        cornerLoad,
        surfaceForwardBaselineMps2,
        surfaceVerticalBaselineMps2,
        surfaceDynamicMps2,
        surfaceRoughness,
        impact,
        impactLevel,
        impactArmed: impact ? false : impactArmed,
        impactQuietSinceMs: impact ? null : impactQuietSinceMs,
        lastImpactAtMs: impact ? arrivalMs : (previous?.lastImpactAtMs || -Infinity),
        lastImpactLevel: impact ? impactLevel : (previous?.lastImpactLevel || 0),
        lastImpactEvent: impact ? rawImpactEvent : (previous?.lastImpactEvent || null),
      };
      this.streams.set(payload.src, snapshot);
      return this.getSnapshot(payload.src, arrivalMs);
    }

    ingestEvent(payload, arrivalMs) {
      if (!Number.isFinite(arrivalMs)) return null;
      const name = payload.v === 2 ? payload?.e?.n : payload?.evt?.name;
      if (!['impact', 'impact_candidate'].includes(name)) {
        return this.getSnapshot(payload.src, arrivalMs);
      }
      const magnitude = payload.v === 2 ? payload.e.m : payload.evt.data.mag_mps2;
      const stored = this.streams.get(payload.src);
      if (!stored || stored.boot !== payload.boot || !Number.isFinite(magnitude)) {
        return null;
      }
      const jerkMps3 = payload.v === 2 ? payload.e.j : 0;
      const impactClass = classifyImpact(magnitude, jerkMps3, this.options);
      if (!impactClass) {
        return this.getSnapshot(payload.src, arrivalMs);
      }
      const previousImpactClass = stored.lastImpactEvent?.impactClass || '';
      const isEscalation = impactClassRank(impactClass) > impactClassRank(previousImpactClass);
      const weakRepeatReady = impactClass === 'weak'
        && previousImpactClass === 'weak'
        && Number.isFinite(stored.lastImpactAtMs)
        && arrivalMs - stored.lastImpactAtMs >= this.options.impactWeakRepeatMs;
      if (!stored.impactArmed && !isEscalation && !weakRepeatReady) {
        return this.getSnapshot(payload.src, arrivalMs);
      }
      const axis = payload.v === 2 ? payload.e.a : payload.evt.data.axis;
      const snapshot = {
        ...stored,
        seq: payload.seq,
        tUs: payload.t_us,
        impact: true,
        impactLevel: impactClassLevel(impactClass),
        impactArmed: false,
        impactQuietSinceMs: null,
        lastImpactAtMs: arrivalMs,
        lastImpactLevel: impactClassLevel(impactClass),
        lastImpactEvent: {
          impactClass,
          magnitudeMps2: magnitude,
          jerkMps3,
          axis,
          source: payload.v === 2 ? 'm5_v2' : 'm5_v1',
        },
      };
      this.streams.set(payload.src, snapshot);
      return this.getSnapshot(payload.src, arrivalMs);
    }

    getSnapshot(src, nowMs) {
      const snapshot = this.streams.get(src);
      if (!snapshot) return null;
      const impactAgeMs = nowMs - snapshot.lastImpactAtMs;
      const impactRecent = impactAgeMs >= 0 && impactAgeMs <= this.options.impactDisplayMs;
      return {
        ...snapshot,
        stale: nowMs - snapshot.lastArrivalMs > snapshot.staleThresholdMs,
        impactRecent,
        impactLevel: impactRecent ? snapshot.lastImpactLevel : 0,
      };
    }

    reset() {
      this.streams.clear();
    }
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
        stream.periodUs = getStatePeriodUs(payload);
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
        const escQuality = stream.state?.v === 2 && isPlainObject(stream.state.esc)
          ? stream.state.q
          : null;
        const sourceStale = escQuality
          ? escQuality.ok !== true || escQuality.age > staleThresholdMs
          : false;
        return {
          ...stream,
          stateAgeMs,
          staleThresholdMs,
          stale: !stream.state || stateAgeMs > staleThresholdMs || sourceStale,
        };
      });
      const primaryMotion = streams
        .filter((stream) => isPlainObject(stream.state?.m) || isPlainObject(stream.state?.imu))
        .sort((left, right) => right.lastStateAt - left.lastStateAt)[0] || null;
      const primaryEsc = streams
        .filter((stream) => isPlainObject(stream.state?.esc))
        .sort((left, right) => right.lastStateAt - left.lastStateAt)[0] || null;
      return {
        counters: { ...this.counters },
        streams,
        primary: primaryMotion,
        primaryMotion,
        primaryEsc,
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

  return {
    MAX_WIRE_BYTES,
    TELEMETRY_PREFIX,
    VEHICLE_FLU_AXES_FLAG,
    MotionFeatureExtractor,
    RelayEventInbox,
    TelemetryTracker,
    classifySequence,
    deriveVehicleMotion,
    encodeTelemetry,
    getStaleThresholdMs,
    parseTelemetryMessage,
    parseRelayEventMessage,
  };
}));
