(function initPilotCalloutPlanner(root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.MomoPilotCalloutPlanner = api;
  }
})(typeof globalThis !== 'undefined' ? globalThis : this, () => {
  'use strict';

  const CALLOUT_KINDS = Object.freeze(['gap_ahead', 'gap_behind']);

  function finiteNumber(value) {
    const number = Number(value);
    return Number.isFinite(number) ? number : null;
  }

  function normalizeCarNumber(rival) {
    const explicit = finiteNumber(rival?.carNumber);
    if (explicit !== null && Number.isInteger(explicit) && explicit >= 1 && explicit <= 999) {
      return explicit;
    }
    const match = String(rival?.carId || '').trim().match(/(\d+)$/);
    if (!match) return null;
    const fallback = Number.parseInt(match[1], 10);
    return Number.isInteger(fallback) && fallback >= 1 && fallback <= 999 ? fallback : null;
  }

  function normalizeGapMs(value, maximumGapMs) {
    const gapMs = finiteNumber(value);
    if (gapMs === null || gapMs <= 0 || gapMs > maximumGapMs) return null;
    return Math.max(100, Math.round(gapMs / 100) * 100);
  }

  function markerKey(rival) {
    const index = finiteNumber(rival?.lastMarkerIndex);
    const raceMs = finiteNumber(rival?.lastMarkerRaceMs);
    if (index === null && raceMs === null) return '';
    return `${index === null ? 'x' : Math.round(index)}:${raceMs === null ? 'x' : Math.round(raceMs)}`;
  }

  function createPlanner(options = {}) {
    const now = typeof options.now === 'function' ? options.now : () => Date.now();
    const warningGapMs = Math.max(100, finiteNumber(options.warningGapMs) ?? 2500);
    const criticalGapMs = Math.min(
      warningGapMs,
      Math.max(100, finiteNumber(options.criticalGapMs) ?? 1000),
    );
    const maximumGapMs = Math.max(warningGapMs, finiteNumber(options.maximumGapMs) ?? 5000);
    const significantDeltaMs = Math.max(100, finiteNumber(options.significantDeltaMs) ?? 500);
    const directionCooldownMs = Math.max(0, finiteNumber(options.directionCooldownMs) ?? 8000);
    const globalCooldownMs = Math.max(0, finiteNumber(options.globalCooldownMs) ?? 3000);
    let runId = '';
    let initialized = false;
    let serial = 0;
    let lastCalloutAt = Number.NEGATIVE_INFINITY;
    const directions = {
      gap_ahead: { observed: null, lastCalloutAt: Number.NEGATIVE_INFINITY },
      gap_behind: { observed: null, lastCalloutAt: Number.NEGATIVE_INFINITY },
    };

    function clear(nextRunId = '') {
      runId = String(nextRunId || '').trim();
      initialized = false;
      lastCalloutAt = Number.NEGATIVE_INFINITY;
      directions.gap_ahead.observed = null;
      directions.gap_ahead.lastCalloutAt = Number.NEGATIVE_INFINITY;
      directions.gap_behind.observed = null;
      directions.gap_behind.lastCalloutAt = Number.NEGATIVE_INFINITY;
    }

    function observe(kind, rival, gapSource, markerSource = rival) {
      const carNumber = normalizeCarNumber(rival);
      const gapMs = normalizeGapMs(gapSource, maximumGapMs);
      if (!rival || carNumber === null || gapMs === null) return null;
      return Object.freeze({
        kind,
        carId: String(rival.carId || '').trim(),
        carNumber,
        gapMs,
        markerKey: markerKey(markerSource),
      });
    }

    function candidateFor(kind, current, timestamp) {
      const direction = directions[kind];
      const previous = direction.observed;
      direction.observed = current;
      if (!initialized || !current || !previous) return null;

      const rivalChanged = current.carId !== previous.carId || current.carNumber !== previous.carNumber;
      const enteredWarning = previous.gapMs > warningGapMs && current.gapMs <= warningGapMs;
      const enteredCritical = kind === 'gap_behind'
        && previous.gapMs > criticalGapMs && current.gapMs <= criticalGapMs;
      const markerChanged = Boolean(current.markerKey && current.markerKey !== previous.markerKey);
      const changedEnough = Math.abs(current.gapMs - previous.gapMs) >= significantDeltaMs;
      if (!rivalChanged && !enteredWarning && !enteredCritical && !(markerChanged && changedEnough)) {
        return null;
      }
      if (timestamp - lastCalloutAt < globalCooldownMs) return null;
      if (!enteredCritical && timestamp - direction.lastCalloutAt < directionCooldownMs) return null;
      return {
        ...current,
        priority: kind === 'gap_behind' ? (enteredCritical ? 80 : 60) : 30,
        reason: rivalChanged ? 'rival_changed'
          : enteredCritical ? 'entered_critical'
            : enteredWarning ? 'entered_warning' : 'gap_changed',
      };
    }

    function evaluate(input = {}) {
      const nextRunId = String(input.raceRunId || '').trim();
      if (nextRunId !== runId) clear(nextRunId);
      if (String(input.phaseCode || '').trim().toLowerCase() !== 'green') {
        initialized = false;
        directions.gap_ahead.observed = null;
        directions.gap_behind.observed = null;
        return null;
      }

      const battle = input.battle || {};
      const ahead = battle.ahead
        ? observe('gap_ahead', battle.ahead, battle.self?.intervalToAheadMs, battle.self)
        : null;
      const behind = input.suppressGapBehind === true
        ? null
        : battle.behind
          ? observe('gap_behind', battle.behind, battle.behind.intervalToAheadMs)
          : null;
      const timestamp = now();
      const candidates = [
        candidateFor('gap_behind', behind, timestamp),
        candidateFor('gap_ahead', ahead, timestamp),
      ].filter(Boolean).sort((left, right) => right.priority - left.priority);

      if (!initialized) {
        initialized = true;
        return null;
      }
      const selected = candidates[0];
      if (!selected) return null;
      lastCalloutAt = timestamp;
      directions[selected.kind].lastCalloutAt = timestamp;
      return Object.freeze({
        requestId: `${selected.kind}-${++serial}`,
        kind: selected.kind,
        carNumber: selected.carNumber,
        gapMs: selected.gapMs,
        priority: selected.priority,
        reason: selected.reason,
      });
    }

    return Object.freeze({ clear, evaluate });
  }

  return Object.freeze({ CALLOUT_KINDS, createPlanner, normalizeCarNumber, normalizeGapMs });
});
