(function initImuDriveCalibrationModule(root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.MomoImuDriveCalibration = api;
  }
}(typeof globalThis !== 'undefined' ? globalThis : this, () => {
  'use strict';

  const IMU_SOURCE = 'imu0';
  const FUSION_CAPABILITY = 'imu_fusion_v1';
  const STATES = Object.freeze({
    IDLE: 'idle',
    WAITING: 'waiting',
    CALIBRATING: 'calibrating',
    READY: 'ready',
    DEGRADED: 'degraded',
  });
  const FAILURE_REASONS = Object.freeze({
    1: 'unstable',
    2: 'moving',
    3: 'sensor_invalid',
    4: 'timeout',
  });

  function isFusionPayload(payload) {
    return payload?.v === 2
      && payload?.k === 's'
      && payload?.src === IMU_SOURCE
      && Array.isArray(payload?.q?.f)
      && payload.q.f.includes(FUSION_CAPABILITY)
      && Array.isArray(payload.q.c)
      && payload.q.c.length === 3;
  }

  function defaultCalibrationId() {
    const values = new Uint32Array(1);
    if (typeof crypto !== 'undefined' && typeof crypto.getRandomValues === 'function') {
      crypto.getRandomValues(values);
    } else {
      values[0] = Math.floor(Math.random() * 0x100000000) >>> 0;
    }
    return values[0] || 1;
  }

  function createController(options = {}) {
    const now = typeof options.now === 'function' ? options.now : () => Date.now();
    const sendCommand = typeof options.sendCommand === 'function' ? options.sendCommand : () => false;
    const onChange = typeof options.onChange === 'function' ? options.onChange : () => {};
    const createCalibrationId = typeof options.createCalibrationId === 'function'
      ? options.createCalibrationId
      : defaultCalibrationId;
    const retryMs = Math.max(50, Number(options.retryMs) || 250);
    const timeoutMs = Math.max(retryMs, Number(options.timeoutMs) || 3500);
    const staleFloorMs = Math.max(250, Number(options.staleFloorMs) || 500);
    const staleCeilingMs = Math.max(staleFloorMs, Number(options.staleCeilingMs) || 2000);
    const stalePeriodMultiplier = Math.max(3, Number(options.stalePeriodMultiplier) || 6);

    let current = {
      driveRequested: false,
      state: STATES.IDLE,
      reason: '',
      bootId: '',
      calibrationId: 0,
      startedAtMs: 0,
      lastTelemetryAtMs: 0,
      periodUs: 0,
      nextRetryAtMs: 0,
      commandAttempts: 0,
    };

    function snapshot() {
      return Object.freeze({
        driveRequested: current.driveRequested,
        state: current.state,
        reason: current.reason,
        bootId: current.bootId,
        calibrationId: current.calibrationId,
        commandAttempts: current.commandAttempts,
        driveAllowed: current.driveRequested
          && (current.state === STATES.READY || current.state === STATES.DEGRADED),
        imuEffectsEnabled: current.driveRequested && current.state === STATES.READY,
      });
    }

    function publish(previous) {
      const next = snapshot();
      onChange(next, previous);
      return next;
    }

    function transition(patch) {
      const previous = snapshot();
      current = { ...current, ...patch };
      return publish(previous);
    }

    function degrade(reason) {
      return transition({
        state: STATES.DEGRADED,
        reason: reason || 'unknown',
        nextRetryAtMs: 0,
      });
    }

    function commandLine() {
      return `IMU:LEVEL:${current.bootId}:${current.calibrationId}`;
    }

    function transmitCalibrationCommand(atMs, force = false) {
      if (current.state !== STATES.CALIBRATING || (!force && atMs < current.nextRetryAtMs)) {
        return false;
      }
      sendCommand(commandLine());
      current.commandAttempts += 1;
      current.nextRetryAtMs = atMs + retryMs;
      return true;
    }

    function beginCalibration(payload, arrivalMs) {
      const calibrationId = Number(createCalibrationId()) >>> 0 || 1;
      const atMs = now();
      transition({
        state: STATES.CALIBRATING,
        reason: '',
        bootId: payload.boot,
        calibrationId,
        startedAtMs: atMs,
        lastTelemetryAtMs: arrivalMs,
        periodUs: payload.q.p,
        nextRetryAtMs: atMs,
        commandAttempts: 0,
      });
      transmitCalibrationCommand(atMs, true);
      return snapshot();
    }

    function acceptInitialPayload(payload, arrivalMs) {
      if (!payload || payload.src !== IMU_SOURCE || payload.k !== 's') {
        return snapshot();
      }
      if (!isFusionPayload(payload)) {
        return degrade('unsupported');
      }
      return beginCalibration(payload, arrivalMs);
    }

    function start(initialPayload = null, arrivalMs = now(), startOptions = {}) {
      if (current.driveRequested) return snapshot();
      const atMs = now();
      if (startOptions.bypass === true) {
        return transition({
          driveRequested: true,
          state: STATES.DEGRADED,
          reason: 'test_mode',
          bootId: '',
          calibrationId: 0,
          startedAtMs: atMs,
          lastTelemetryAtMs: 0,
          periodUs: 0,
          nextRetryAtMs: 0,
          commandAttempts: 0,
        });
      }
      transition({
        driveRequested: true,
        state: STATES.WAITING,
        reason: 'telemetry_wait',
        bootId: '',
        calibrationId: 0,
        startedAtMs: atMs,
        lastTelemetryAtMs: 0,
        periodUs: 0,
        nextRetryAtMs: 0,
        commandAttempts: 0,
      });
      return acceptInitialPayload(initialPayload, arrivalMs);
    }

    function stop() {
      if (!current.driveRequested && current.state === STATES.IDLE) return snapshot();
      return transition({
        driveRequested: false,
        state: STATES.IDLE,
        reason: '',
        bootId: '',
        calibrationId: 0,
        startedAtMs: 0,
        lastTelemetryAtMs: 0,
        periodUs: 0,
        nextRetryAtMs: 0,
        commandAttempts: 0,
      });
    }

    function ingest(payload, arrivalMs = now()) {
      if (!current.driveRequested || current.state === STATES.IDLE || current.state === STATES.DEGRADED) {
        return snapshot();
      }
      if (!payload || payload.src !== IMU_SOURCE || payload.k !== 's') return snapshot();
      if (current.state === STATES.WAITING) return acceptInitialPayload(payload, arrivalMs);
      if (!isFusionPayload(payload)) return degrade('capability_lost');
      if (payload.boot !== current.bootId) return degrade('boot_changed');

      current.lastTelemetryAtMs = arrivalMs;
      current.periodUs = payload.q.p;
      const [calibrationState, calibrationId, failureReason] = payload.q.c;
      if (current.state === STATES.CALIBRATING) {
        if (calibrationId !== current.calibrationId) return snapshot();
        if (calibrationState === 3) return degrade(FAILURE_REASONS[failureReason] || 'calibration_failed');
        if (calibrationState === 2) {
          if (!Array.isArray(payload.m?.l) || payload.m.l.length !== 3) {
            return degrade('fused_value_missing');
          }
          return transition({ state: STATES.READY, reason: '', nextRetryAtMs: 0 });
        }
        return snapshot();
      }

      if (current.state === STATES.READY) {
        if (calibrationId !== current.calibrationId || calibrationState !== 2) {
          return degrade('calibration_state_changed');
        }
        if (!Array.isArray(payload.m?.l) || payload.m.l.length !== 3) {
          return degrade('fused_value_missing');
        }
      }
      return snapshot();
    }

    function tick(atMs = now()) {
      if (!current.driveRequested) return snapshot();
      if (current.state === STATES.WAITING) {
        if (atMs - current.startedAtMs >= timeoutMs) return degrade('telemetry_timeout');
        return snapshot();
      }
      if (current.state === STATES.CALIBRATING) {
        if (atMs - current.startedAtMs >= timeoutMs) return degrade('timeout');
        transmitCalibrationCommand(atMs);
        return snapshot();
      }
      if (current.state === STATES.READY) {
        const staleMs = Math.min(
          staleCeilingMs,
          Math.max(staleFloorMs, (current.periodUs / 1000) * stalePeriodMultiplier),
        );
        if (atMs - current.lastTelemetryAtMs > staleMs) return degrade('telemetry_stale');
      }
      return snapshot();
    }

    function retryNow(atMs = now()) {
      if (current.state === STATES.CALIBRATING) transmitCalibrationCommand(atMs, true);
      return snapshot();
    }

    return Object.freeze({ ingest, retryNow, snapshot, start, stop, tick });
  }

  return Object.freeze({
    FAILURE_REASONS,
    FUSION_CAPABILITY,
    IMU_SOURCE,
    STATES,
    createController,
    isFusionPayload,
  });
}));
