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
      warningClosingMs: normalizedOption(options.warningClosingMs, DEFAULT_OPTIONS.warningClosingMs),
      criticalClosingMs: normalizedOption(options.criticalClosingMs, DEFAULT_OPTIONS.criticalClosingMs),
    });
    let activeIdentity = '';
    let lastMarkerKey = '';
    let previousGapMs = null;

    function reset() {
      activeIdentity = '';
      lastMarkerKey = '';
      previousGapMs = null;
    }

    function evaluate(input = {}) {
      if (String(input.phaseCode || '').trim().toLowerCase() !== 'green') {
        reset();
        return null;
      }
      const self = input.self;
      const behind = input.behind;
      const behindCarId = typeof behind?.carId === 'string' ? behind.carId.trim() : '';
      const selfLap = integerNonNegative(self?.lap);
      const behindLap = integerNonNegative(behind?.lap);
      if (!behindCarId || (selfLap !== null && behindLap !== null && selfLap !== behindLap)) {
        reset();
        return null;
      }

      const markerIndex = integerNonNegative(behind.lastMarkerIndex);
      const markerRaceMs = integerNonNegative(behind.lastMarkerRaceMs);
      if (markerIndex === null || markerRaceMs === null) {
        return null;
      }
      const identity = `${String(input.raceRunId || '').trim()}:${behindCarId}`;
      const markerKey = `${behindLap ?? 'lap'}:${markerIndex}:${markerRaceMs}`;
      const gapMs = finiteNonNegative(behind.intervalToAheadMs);
      if (identity !== activeIdentity) {
        activeIdentity = identity;
        lastMarkerKey = markerKey;
        previousGapMs = gapMs;
        return null;
      }
      if (markerKey === lastMarkerKey) {
        return null;
      }
      lastMarkerKey = markerKey;
      if (gapMs === null) {
        previousGapMs = null;
        return null;
      }

      const previous = previousGapMs;
      previousGapMs = gapMs;
      if (previous === null) {
        return null;
      }
      const closingMs = previous - gapMs;
      let severity = '';
      if (gapMs <= config.criticalGapMs && closingMs >= config.criticalClosingMs) {
        severity = 'critical';
      } else if (gapMs <= config.warningGapMs && closingMs >= config.warningClosingMs) {
        severity = 'warning';
      }
      if (!severity) {
        return null;
      }
      return {
        severity,
        gapMs,
        closingMs,
        carId: behindCarId,
        driver: typeof behind.driver === 'string' ? behind.driver.trim() : '',
        markerIndex,
        markerRaceMs,
      };
    }

    return Object.freeze({ config, evaluate, reset });
  }

  return Object.freeze({ DEFAULT_OPTIONS, createRearAttentionTracker });
}));
