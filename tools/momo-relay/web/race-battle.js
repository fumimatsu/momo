(function initRaceBattleModule(root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
    return;
  }
  root.MomoRaceBattle = api;
}(typeof globalThis !== 'undefined' ? globalThis : this, () => {
  'use strict';

  const DEFAULT_OPTIONS = Object.freeze({
    warningGapMs: 2500,
    criticalGapMs: 1000,
    releaseGapMs: 3000,
    warningClosingMs: 300,
    criticalClosingMs: 100,
  });

  function finiteNonNegative(value) {
    if (
      value === null ||
      value === undefined ||
      value === '' ||
      typeof value === 'boolean'
    ) {
      return null;
    }
    const number = Number(value);
    return Number.isFinite(number) && number >= 0 ? number : null;
  }

  function integerNonNegative(value) {
    if (
      value === null ||
      value === undefined ||
      value === '' ||
      typeof value === 'boolean'
    ) {
      return null;
    }
    const number = Number(value);
    return Number.isInteger(number) && number >= 0 ? number : null;
  }

  function normalizedOption(value, fallback) {
    const number = finiteNonNegative(value);
    return number === null ? fallback : number;
  }

  function createRearAttentionTracker(options = {}) {
    const config = Object.freeze({
      warningGapMs: normalizedOption(options.warningGapMs, DEFAULT_OPTIONS.warningGapMs),
      criticalGapMs: normalizedOption(options.criticalGapMs, DEFAULT_OPTIONS.criticalGapMs),
      releaseGapMs: Math.max(
        normalizedOption(options.warningGapMs, DEFAULT_OPTIONS.warningGapMs),
        normalizedOption(options.releaseGapMs, DEFAULT_OPTIONS.releaseGapMs),
      ),
      warningClosingMs: normalizedOption(options.warningClosingMs, DEFAULT_OPTIONS.warningClosingMs),
      criticalClosingMs: normalizedOption(options.criticalClosingMs, DEFAULT_OPTIONS.criticalClosingMs),
    });
    let activeIdentity = '';
    let lastMarkerKey = '';
    let previousGapMs = null;
    let currentState = null;

    function reset() {
      activeIdentity = '';
      lastMarkerKey = '';
      previousGapMs = null;
      currentState = null;
    }

    function inactiveState() {
      return {
        active: false,
        severity: '',
        trend: 'unknown',
        shouldPulse: false,
        gapMs: null,
        closingMs: null,
        carId: '',
        driver: '',
        markerIndex: null,
        markerRaceMs: null,
      };
    }

    function buildState(behind, gapMs, markerIndex, markerRaceMs, previous, wasActive) {
      const closingMs = previous === null ? null : previous - gapMs;
      const active = gapMs <= (wasActive ? config.releaseGapMs : config.warningGapMs);
      const severity = active
        ? gapMs <= config.criticalGapMs ? 'critical' : 'warning'
        : '';
      const previousSeverity = currentState?.severity || '';
      const closingThreshold = severity === 'critical'
        ? config.criticalClosingMs
        : config.warningClosingMs;
      const trend = closingMs === null
        ? 'unknown'
        : closingMs >= closingThreshold
          ? 'closing'
          : closingMs <= -closingThreshold ? 'opening' : 'holding';
      const severityIncreased = severity === 'critical' && previousSeverity !== 'critical';
      const shouldPulse = active && (
        !wasActive || severityIncreased || (closingMs !== null && closingMs >= closingThreshold)
      );
      return {
        active,
        severity,
        trend,
        shouldPulse,
        gapMs,
        closingMs,
        carId: behind.carId.trim(),
        driver: typeof behind.driver === 'string' ? behind.driver.trim() : '',
        markerIndex,
        markerRaceMs,
      };
    }

    function evaluate(input = {}) {
      if (String(input.phaseCode || '').trim().toLowerCase() !== 'green') {
        reset();
        return inactiveState();
      }
      const self = input.self;
      const behind = input.behind;
      const behindCarId = typeof behind?.carId === 'string' ? behind.carId.trim() : '';
      const selfLap = integerNonNegative(self?.lap);
      const behindLap = integerNonNegative(behind?.lap);
      if (!behindCarId || (selfLap !== null && behindLap !== null && selfLap !== behindLap)) {
        reset();
        return inactiveState();
      }

      const markerIndex = integerNonNegative(behind.lastMarkerIndex);
      const markerRaceMs = integerNonNegative(behind.lastMarkerRaceMs);
      if (markerIndex === null || markerRaceMs === null) {
        reset();
        return inactiveState();
      }
      const identity = `${String(input.raceRunId || '').trim()}:${behindCarId}`;
      const markerKey = `${behindLap ?? 'lap'}:${markerIndex}:${markerRaceMs}`;
      const gapMs = finiteNonNegative(behind.intervalToAheadMs);
      if (gapMs === null) {
        activeIdentity = identity;
        lastMarkerKey = markerKey;
        previousGapMs = null;
        currentState = inactiveState();
        return currentState;
      }
      if (identity !== activeIdentity) {
        const previous = null;
        activeIdentity = identity;
        lastMarkerKey = markerKey;
        previousGapMs = gapMs;
        currentState = buildState(behind, gapMs, markerIndex, markerRaceMs, previous, false);
        return currentState;
      }
      if (markerKey === lastMarkerKey) {
        return currentState
          ? { ...currentState, shouldPulse: false }
          : inactiveState();
      }
      lastMarkerKey = markerKey;

      const previous = previousGapMs;
      const wasActive = currentState?.active === true;
      previousGapMs = gapMs;
      currentState = buildState(behind, gapMs, markerIndex, markerRaceMs, previous, wasActive);
      return currentState;
    }

    return Object.freeze({ config, evaluate, reset });
  }

  return Object.freeze({ DEFAULT_OPTIONS, createRearAttentionTracker });
}));
