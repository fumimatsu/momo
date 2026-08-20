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
  const DEFAULT_BLUE_FLAG_OPTIONS = Object.freeze({
    warningGapMs: 3000,
    releaseGapMs: 4000,
  });
  const DEFAULT_SECTOR_OPTIONS = Object.freeze({
    completedLapHoldMs: 2000,
    maximumSeenAchievements: 64,
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

  function resolveRaceMapElapsedMs(standing, lapHistory = []) {
    const markerRaceMs = finiteNonNegative(standing?.lastMarkerRaceMs);
    const explicitRaceElapsedMs = finiteNonNegative(standing?.raceElapsedMs);
    if (explicitRaceElapsedMs !== null
      && (markerRaceMs === null || explicitRaceElapsedMs >= markerRaceMs)) {
      return explicitRaceElapsedMs;
    }

    const carId = typeof standing?.carId === 'string' ? standing.carId.trim() : '';
    const currentLapMs = finiteNonNegative(standing?.currentLapMs);
    let latestCompletedAtRaceMs = null;
    if (carId && currentLapMs !== null && Array.isArray(lapHistory)) {
      for (const entry of lapHistory) {
        if (typeof entry?.carId !== 'string' || entry.carId.trim() !== carId) {
          continue;
        }
        const completedAtRaceMs = finiteNonNegative(entry.completedAtRaceMs);
        if (completedAtRaceMs !== null
          && (latestCompletedAtRaceMs === null || completedAtRaceMs > latestCompletedAtRaceMs)) {
          latestCompletedAtRaceMs = completedAtRaceMs;
        }
      }
    }
    if (latestCompletedAtRaceMs !== null) {
      const reconstructed = latestCompletedAtRaceMs + currentLapMs;
      return markerRaceMs === null ? reconstructed : Math.max(markerRaceMs, reconstructed);
    }
    if (markerRaceMs !== null) {
      return markerRaceMs;
    }
    return finiteNonNegative(standing?.allTimeMs);
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
      // Race start immediately after standings initialization can briefly report 0 ms
      // for every car. Treat it as an uninitialized interval, not a real proximity alert.
      if (gapMs === 0) {
        reset();
        return inactiveState();
      }
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

  function createBlueFlagTracker(options = {}) {
    const config = Object.freeze({
      warningGapMs: normalizedOption(options.warningGapMs, DEFAULT_BLUE_FLAG_OPTIONS.warningGapMs),
      releaseGapMs: Math.max(
        normalizedOption(options.warningGapMs, DEFAULT_BLUE_FLAG_OPTIONS.warningGapMs),
        normalizedOption(options.releaseGapMs, DEFAULT_BLUE_FLAG_OPTIONS.releaseGapMs),
      ),
    });
    let activeIdentity = '';
    let lastMarkerKey = '';
    let currentState = null;

    function inactiveState() {
      return {
        active: false,
        shouldPulse: false,
        gapMs: null,
        carId: '',
        driver: '',
        markerIndex: null,
        markerRaceMs: null,
      };
    }

    function reset() {
      activeIdentity = '';
      lastMarkerKey = '';
      currentState = null;
    }

    function evaluate(input = {}) {
      if (String(input.phaseCode || '').trim().toLowerCase() !== 'green') {
        reset();
        return inactiveState();
      }

      const self = input.self;
      const lapping = input.lapping;
      const selfCarId = typeof self?.carId === 'string' ? self.carId.trim() : '';
      const lappingCarId = typeof lapping?.carId === 'string' ? lapping.carId.trim() : '';
      const advisoryCarId = typeof self?.lappingCarBehindId === 'string'
        ? self.lappingCarBehindId.trim()
        : '';
      const selfLap = integerNonNegative(self?.lap);
      const lappingLap = integerNonNegative(lapping?.lap);
      const selfPosition = integerNonNegative(self?.position);
      const lappingPosition = integerNonNegative(lapping?.position);
      if (!selfCarId
        || !lappingCarId
        || advisoryCarId !== lappingCarId
        || String(self?.status || '').trim().toLowerCase() !== 'racing'
        || String(lapping?.status || '').trim().toLowerCase() !== 'racing'
        || selfLap === null
        || lappingLap === null
        || lappingLap <= selfLap
        || selfPosition === null
        || lappingPosition === null
        || lappingPosition >= selfPosition) {
        reset();
        return inactiveState();
      }

      const markerIndex = integerNonNegative(lapping.lastMarkerIndex);
      const markerRaceMs = integerNonNegative(lapping.lastMarkerRaceMs);
      const gapMs = finiteNonNegative(self.lappingGapMs);
      if (markerIndex === null || markerRaceMs === null || gapMs === null || gapMs === 0) {
        reset();
        return inactiveState();
      }

      const identity = `${String(input.raceRunId || '').trim()}:${selfCarId}:${lappingCarId}`;
      const markerKey = `${lappingLap}:${markerIndex}:${markerRaceMs}`;
      if (identity === activeIdentity && markerKey === lastMarkerKey) {
        return currentState
          ? { ...currentState, shouldPulse: false }
          : inactiveState();
      }

      const wasActive = identity === activeIdentity && currentState?.active === true;
      const active = gapMs <= (wasActive ? config.releaseGapMs : config.warningGapMs);
      currentState = {
        active,
        shouldPulse: active && !wasActive,
        gapMs,
        carId: lappingCarId,
        driver: typeof lapping.driver === 'string' ? lapping.driver.trim() : '',
        markerIndex,
        markerRaceMs,
      };
      activeIdentity = identity;
      lastMarkerKey = markerKey;
      return currentState;
    }

    return Object.freeze({ config, evaluate, reset });
  }

  function createSectorStatusTracker(options = {}) {
    const config = Object.freeze({
      completedLapHoldMs: normalizedOption(
        options.completedLapHoldMs,
        DEFAULT_SECTOR_OPTIONS.completedLapHoldMs,
      ),
      maximumSeenAchievements: Math.max(1, Math.floor(normalizedOption(
        options.maximumSeenAchievements,
        DEFAULT_SECTOR_OPTIONS.maximumSeenAchievements,
      ))),
    });
    const now = typeof options.now === 'function' ? options.now : () => Date.now();
    const seenAchievements = new Set();
    let activeRunId = '';
    let heldCompletedLap = null;
    let heldUntil = 0;
    let lastCompletedLapKey = '';

    function reset(runId = '') {
      activeRunId = String(runId || '').trim();
      heldCompletedLap = null;
      heldUntil = 0;
      lastCompletedLapKey = '';
      seenAchievements.clear();
    }

    function normalizeSectorTimes(standing) {
      if (!Array.isArray(standing?.sectorTimes)) {
        return [];
      }
      return standing.sectorTimes
        .map((item) => {
          const sector = integerNonNegative(item?.sector);
          const sampleLap = integerNonNegative(item?.sampleLap);
          const normalizedLastMs = finiteNonNegative(item?.lastMs);
          const normalizedBestMs = finiteNonNegative(item?.bestMs);
          const lastMs = normalizedLastMs !== null && normalizedLastMs > 0 ? normalizedLastMs : null;
          const bestMs = normalizedBestMs !== null && normalizedBestMs > 0 ? normalizedBestMs : null;
          const achievement = item?.achievement === 'personal_best'
            || item?.achievement === 'overall_best'
            ? item.achievement
            : '';
          return sector !== null && sector > 0
            ? { sector, sampleLap, lastMs, bestMs, achievement }
            : null;
        })
        .filter((item) => item !== null);
    }

    function rememberAchievement(key) {
      if (seenAchievements.has(key)) {
        return false;
      }
      seenAchievements.add(key);
      while (seenAchievements.size > config.maximumSeenAchievements) {
        seenAchievements.delete(seenAchievements.values().next().value);
      }
      return true;
    }

    function emptyResult(sectorCount = 3, displayLap = null) {
      return {
        displayLap,
        sectors: Array.from({ length: sectorCount }, (_, index) => ({
          sector: index + 1,
          sampleLap: null,
          lastMs: null,
          status: 'missing',
          achievement: '',
          isNewAchievement: false,
        })),
      };
    }

    function evaluate(input = {}) {
      const runId = String(input.raceRunId || '').trim();
      if (runId !== activeRunId) {
        reset(runId);
      }
      const carId = String(input.carId || '').trim();
      const standings = Array.isArray(input.standings) ? input.standings : [];
      const standing = standings.find((item) => String(item?.carId || '').trim() === carId);
      const standingLap = integerNonNegative(standing?.lap);
      const sectorCount = Math.min(3, Math.max(1, integerNonNegative(standing?.sectorCount) || 3));
      if (!standing || standingLap === null) {
        return emptyResult(sectorCount);
      }

      const phaseCode = String(input.phaseCode || '').trim().toLowerCase();
      const ownSectors = normalizeSectorTimes(standing);
      const finalSector = ownSectors.find((item) => item.sector === sectorCount);
      const completedLapKey = finalSector?.sampleLap === standingLap && finalSector.lastMs !== null
        ? `${standingLap}:${finalSector.lastMs}`
        : '';
      if (completedLapKey && completedLapKey !== lastCompletedLapKey && phaseCode === 'green') {
        lastCompletedLapKey = completedLapKey;
        heldCompletedLap = standingLap;
        heldUntil = now() + config.completedLapHoldMs;
      }
      if (heldCompletedLap !== null && now() >= heldUntil) {
        heldCompletedLap = null;
      }

      const displayLap = phaseCode === 'finished'
        ? standingLap
        : heldCompletedLap ?? standingLap + 1;
      const overallBestBySector = new Map();
      for (const peer of standings) {
        for (const item of normalizeSectorTimes(peer)) {
          if (item.bestMs === null) continue;
          const previous = overallBestBySector.get(item.sector);
          if (previous === undefined || item.bestMs < previous) {
            overallBestBySector.set(item.sector, item.bestMs);
          }
        }
      }

      const sectors = Array.from({ length: sectorCount }, (_, index) => {
        const sector = index + 1;
        const item = ownSectors.find((candidate) => candidate.sector === sector
          && candidate.sampleLap === displayLap);
        if (!item || item.lastMs === null) {
          return {
            sector,
            sampleLap: null,
            lastMs: null,
            status: 'missing',
            achievement: '',
            isNewAchievement: false,
          };
        }
        const overallBest = overallBestBySector.get(sector);
        const status = overallBest !== undefined && item.lastMs === overallBest
          ? 'overall_best'
          : item.bestMs !== null && item.lastMs === item.bestMs
            ? 'personal_best'
            : 'valid';
        const achievementKey = item.achievement
          ? `${runId}:${carId}:${sector}:${displayLap}:${item.lastMs}:${item.achievement}`
          : '';
        return {
          sector,
          sampleLap: displayLap,
          lastMs: item.lastMs,
          status,
          achievement: item.achievement,
          isNewAchievement: Boolean(achievementKey && rememberAchievement(achievementKey)),
        };
      });
      return { displayLap, sectors };
    }

    return Object.freeze({ config, evaluate, reset });
  }

  return Object.freeze({
    DEFAULT_OPTIONS,
    DEFAULT_BLUE_FLAG_OPTIONS,
    DEFAULT_SECTOR_OPTIONS,
    createRearAttentionTracker,
    createBlueFlagTracker,
    createSectorStatusTracker,
    resolveRaceMapElapsedMs,
  });
}));
