const HEALTH_MODES = new Set(['healthy', 'damaged', 'critical', 'limp']);
const PIT_SERVICE_STATES = new Set(['outside', 'servicing', 'complete']);

function finiteNumber(value) {
  if (value === null || value === undefined || value === '') return null;
  const number = Number(value);
  return Number.isFinite(number) ? number : null;
}

export function clamp(value, minimum, maximum) {
  return Math.max(minimum, Math.min(maximum, value));
}

export function parseControlCommand(message) {
  const text = String(message || '').trim();
  if (!text || text.length > 128) return null;
  const throttleField = text.split(',').find((field) => /^T:\s*\d+$/.test(field.trim()));
  if (!throttleField) return null;
  const pwm = Number(throttleField.trim().slice(2));
  if (!Number.isInteger(pwm) || pwm < 1000 || pwm > 2000) return null;
  const gearField = text.split(',').find((field) => /^G:\s*\d+$/.test(field.trim()));
  const gear = gearField ? Number(gearField.trim().slice(2)) : null;
  if (gear !== null && (!Number.isInteger(gear) || gear < 1 || gear > 5)) return null;
  const throttleMaximums = [1600, 1700, 1800, 1900, 2000];
  const brakeMinimums = [1300, 1300, 1200, 1100, 1000];
  const throttleMaximum = gear === null ? 2000 : throttleMaximums[gear - 1];
  const brakeMinimum = gear === null ? 1000 : brakeMinimums[gear - 1];
  return {
    pwm,
    gear,
    throttle: clamp((pwm - 1500) / (throttleMaximum - 1500), 0, 1),
    brake: clamp((1500 - pwm) / (1500 - brakeMinimum), 0, 1),
  };
}

export function parseVehicleHealth(message) {
  const fields = String(message || '').trim().split(',');
  if (fields.length !== 4 || fields[0] !== 'VHS:1') return null;
  const hp = finiteNumber(fields[1]);
  const speedCap = finiteNumber(fields[2]);
  const mode = String(fields[3] || '').toLowerCase();
  if (hp === null || speedCap === null || !HEALTH_MODES.has(mode)) return null;
  return {
    hp: clamp(hp, 0, 100),
    speedCap: clamp(speedCap, 0, 1),
    mode,
  };
}

export function parsePitPresence(message) {
  const text = String(message || '').trim();
  if (!text.startsWith('PIT:1,')) return null;
  let payload;
  try {
    payload = JSON.parse(text.slice(6));
  } catch (_) {
    return null;
  }
  if (!payload || typeof payload !== 'object' || Array.isArray(payload)
      || typeof payload.carId !== 'string' || !payload.carId
      || typeof payload.present !== 'boolean'
      || !PIT_SERVICE_STATES.has(payload.serviceState)) {
    return null;
  }
  const hp = finiteNumber(payload.hp);
  const lastAcceptedTick = finiteNumber(payload.lastAcceptedTick);
  if (hp === null || hp < 0 || hp > 100 || lastAcceptedTick === null
      || !Number.isInteger(lastAcceptedTick) || lastAcceptedTick < 0) {
    return null;
  }
  const entryId = typeof payload.entryId === 'string' ? payload.entryId : '';
  const enteredAtUnixMs = finiteNumber(payload.enteredAtUnixMs);
  const exitedAtUnixMs = finiteNumber(payload.exitedAtUnixMs);
  const exitReason = typeof payload.exitReason === 'string' ? payload.exitReason : '';
  if (payload.present && (!entryId || enteredAtUnixMs === null || !Number.isInteger(enteredAtUnixMs) || enteredAtUnixMs <= 0
      || payload.serviceState === 'outside')) {
    return null;
  }
  if (!payload.present && payload.serviceState !== 'outside') return null;
  if ((exitedAtUnixMs === null) !== (exitReason === '')) return null;
  if (exitedAtUnixMs !== null && (!Number.isInteger(exitedAtUnixMs) || exitedAtUnixMs <= 0)) return null;
  return {
    raceRunId: typeof payload.raceRunId === 'string' ? payload.raceRunId : '',
    carId: payload.carId,
    present: payload.present,
    entryId,
    enteredAtUnixMs: enteredAtUnixMs ?? 0,
    exitedAtUnixMs: exitedAtUnixMs ?? 0,
    exitReason,
    lastAcceptedTick,
    serviceState: payload.serviceState,
    hp,
  };
}

export function parseRaceState(message) {
  let state;
  try {
    const payload = typeof message === 'string' && message.startsWith('RACE:')
      ? message.slice(5)
      : message;
    state = typeof payload === 'string' ? JSON.parse(payload) : payload;
  } catch (_) {
    return null;
  }
  if (!state || state.type !== 'race_state' || state.version !== 2
      || !Array.isArray(state.standings)) {
    return null;
  }
  if (state.sequence !== undefined) {
    const sequence = finiteNumber(state.sequence);
    if (sequence === null || !Number.isInteger(sequence) || sequence < 0) return null;
  }
  return state;
}

export class RaceStateDeduplicator {
  constructor() {
    this.currentRunId = '';
    this.sequence = -1;
    this.serverTimeMs = -1;
    this.seenRunIds = new Set();
  }

  accept(state) {
    const parsed = parseRaceState(state);
    if (!parsed) return false;
    const runId = typeof parsed.raceRunId === 'string' ? parsed.raceRunId : '';
    const sequence = finiteNumber(parsed.sequence);
    const serverTimeMs = finiteNumber(parsed.serverTimeMs) ?? -1;
    if (this.sequence < 0 && this.serverTimeMs < 0) {
      this.currentRunId = runId;
      this.sequence = sequence ?? -1;
      this.serverTimeMs = serverTimeMs;
      if (runId) this.seenRunIds.add(runId);
      return true;
    }
    if (runId === this.currentRunId) {
      if (sequence !== null) {
        if (sequence < this.sequence) return false;
        if (sequence === this.sequence && serverTimeMs <= this.serverTimeMs) return false;
        this.sequence = sequence;
      } else {
        if (serverTimeMs <= this.serverTimeMs) return false;
      }
      this.serverTimeMs = Math.max(this.serverTimeMs, serverTimeMs);
      return true;
    }
    if (!runId || this.seenRunIds.has(runId)) return false;
    if (serverTimeMs >= 0 && this.serverTimeMs >= 0 && serverTimeMs <= this.serverTimeMs) {
      return false;
    }
    this.currentRunId = runId;
    this.sequence = sequence ?? -1;
    this.serverTimeMs = serverTimeMs;
    this.seenRunIds.add(runId);
    return true;
  }
}

export function displayRacePhase(phase) {
  switch (String(phase || '').toLowerCase()) {
    case 'idle': return 'STANDBY';
    case 'ready': return 'READY';
    case 'countdown': return 'COUNTDOWN';
    case 'green': return 'RUNNING';
    case 'paused': return 'PAUSED';
    case 'finished': return 'FINISHED';
    default: return String(phase || 'STANDBY').toUpperCase().slice(0, 24);
  }
}

export function formatDuration(milliseconds) {
  const value = finiteNumber(milliseconds);
  if (value === null || value < 0) return '--:--.---';
  const total = Math.floor(value);
  const minutes = Math.floor(total / 60000);
  const seconds = Math.floor((total % 60000) / 1000);
  const millis = total % 1000;
  return `${String(minutes).padStart(2, '0')}:${String(seconds).padStart(2, '0')}.${String(millis).padStart(3, '0')}`;
}

export function formatSplitTime(milliseconds) {
  const value = finiteNumber(milliseconds);
  if (value === null || value < 0) return '--.---';
  const total = Math.floor(value);
  const minutes = Math.floor(total / 60000);
  const seconds = Math.floor((total % 60000) / 1000);
  const millis = total % 1000;
  const secondsText = `${String(seconds).padStart(2, '0')}.${String(millis).padStart(3, '0')}`;
  return minutes > 0 ? `${String(minutes).padStart(2, '0')}:${secondsText}` : secondsText;
}

export function abbreviateDriverName(value, fallback = '') {
  const name = String(value || fallback).trim();
  return Array.from(name).slice(0, 3).join('');
}

export function classifyBestTime(value, personalBest, overallBest) {
  const time = finiteNumber(value);
  if (time === null || time <= 0) return '';
  const rounded = Math.round(time);
  const overall = finiteNumber(overallBest);
  if (overall !== null && Math.round(overall) === rounded) return 'overall-best';
  const personal = finiteNumber(personalBest);
  if (personal !== null && Math.round(personal) === rounded) return 'personal-best';
  return 'standard-time';
}

export function expectedSectorDurationMs(standing) {
  const currentSector = finiteNumber(standing?.currentSector);
  if (!Number.isInteger(currentSector) || currentSector < 1 || !Array.isArray(standing?.sectorTimes)) return null;
  const timing = standing.sectorTimes.find((entry) => finiteNumber(entry?.sector) === currentSector);
  if (!timing) return null;
  for (const value of [timing.estimateMs, timing.lastMs, timing.bestMs]) {
    const duration = finiteNumber(value);
    if (Number.isInteger(duration) && duration > 0) return duration;
  }
  return null;
}

export function classSectorDurationMs(standings, sector) {
  const sectorNumber = finiteNumber(sector);
  if (!Number.isInteger(sectorNumber) || sectorNumber < 1 || !Array.isArray(standings)) return null;
  const samples = [];
  for (const standing of standings) {
    const timing = Array.isArray(standing?.sectorTimes)
      ? standing.sectorTimes.find((entry) => finiteNumber(entry?.sector) === sectorNumber)
      : null;
    if (!timing) continue;
    const duration = finiteNumber(timing.lastMs) ?? finiteNumber(timing.bestMs);
    if (Number.isInteger(duration) && duration > 0) samples.push(duration);
  }
  if (samples.length === 0) return null;
  samples.sort((left, right) => left - right);
  const middle = Math.floor(samples.length / 2);
  return samples.length % 2 === 0
    ? Math.round((samples[middle - 1] + samples[middle]) / 2)
    : samples[middle];
}

function median(values) {
  if (!Array.isArray(values) || values.length === 0) return null;
  const sorted = values.slice().sort((left, right) => left - right);
  const middle = Math.floor(sorted.length / 2);
  return sorted.length % 2 === 0
    ? (sorted[middle - 1] + sorted[middle]) / 2
    : sorted[middle];
}

function normalizedBoundaries(boundaries, sectorCount) {
  if (!Array.isArray(boundaries) || boundaries.length !== sectorCount + 1) return null;
  const normalized = boundaries.map(finiteNumber);
  if (normalized.some((value) => value === null || value < 0 || value > 1)
      || normalized[0] !== 0 || normalized.at(-1) !== 1
      || normalized.some((value, index) => index > 0 && value <= normalized[index - 1])) {
    return null;
  }
  return normalized;
}

function positiveInteger(value) {
  const number = finiteNumber(value);
  return Number.isInteger(number) && number > 0 ? number : null;
}

function recentLapSamples(state, carId, maximum = 3, normalizedHistory = null) {
  const history = Array.isArray(normalizedHistory) ? normalizedHistory : normalizeLapHistory(state);
  return history
    .filter((entry) => entry.carId === carId)
    .slice(0, maximum)
    .map((entry) => entry.lapTimeMs);
}

export function blendLapPaceMs(recentDurationMs, bestDurationMs, bestWeight = 0.6) {
  const recent = positiveInteger(recentDurationMs);
  const best = positiveInteger(bestDurationMs);
  if (!recent) return best;
  if (!best || best >= recent) return recent;
  const weight = Math.min(1, Math.max(0, finiteNumber(bestWeight) ?? 0.6));
  return Math.round(Math.min(recent, Math.max(best,
    (recent * (1 - weight)) + (best * weight))));
}

function sectorLapSamples(standings, boundaries, carId = null) {
  const samples = [];
  for (const standing of standings || []) {
    if (carId && standing?.carId !== carId) continue;
    const sectorCount = positiveInteger(standing?.sectorCount);
    const normalized = normalizedBoundaries(boundaries, sectorCount);
    if (!normalized || !Array.isArray(standing?.sectorTimes)) continue;
    for (const timing of standing.sectorTimes) {
      const sector = positiveInteger(timing?.sector);
      if (!sector || sector > sectorCount) continue;
      const duration = positiveInteger(timing.estimateMs) ?? positiveInteger(timing.lastMs);
      const courseShare = normalized[sector] - normalized[sector - 1];
      if (duration && courseShare > 0) samples.push(duration / courseShare);
    }
  }
  return samples;
}

export function estimateLapDurationMs(state, standing, boundaries, fallbackLapMs, normalizedHistory = null) {
  const fallback = positiveInteger(fallbackLapMs);
  const carId = typeof standing?.carId === 'string' ? standing.carId : '';
  const history = Array.isArray(normalizedHistory) ? normalizedHistory : normalizeLapHistory(state);
  const personalLaps = recentLapSamples(state, carId, 3, history);
  if (personalLaps.length > 0) {
    const recentDurationMs = Math.round(median(personalLaps));
    const bestDurationMs = positiveInteger(standing?.bestLapMs);
    return {
      durationMs: blendLapPaceMs(recentDurationMs, bestDurationMs),
      source: bestDurationMs && bestDurationMs < recentDurationMs
        ? 'recent-best-blend'
        : 'recent-laps',
    };
  }
  const lastLap = positiveInteger(standing?.lapTimeMs);
  if (lastLap) return { durationMs: lastLap, source: 'last-lap' };
  const personalSectors = sectorLapSamples([standing], boundaries, carId);
  if (personalSectors.length > 0) {
    return { durationMs: Math.round(median(personalSectors)), source: 'personal-sectors' };
  }
  const latestClassLapByCar = new Map();
  for (const entry of history) {
    if (!latestClassLapByCar.has(entry.carId)) latestClassLapByCar.set(entry.carId, entry.lapTimeMs);
  }
  if (latestClassLapByCar.size > 0) {
    return { durationMs: Math.round(median([...latestClassLapByCar.values()])), source: 'class-laps' };
  }
  const classSectors = sectorLapSamples(state?.standings, boundaries);
  if (classSectors.length > 0) {
    return { durationMs: Math.round(median(classSectors)), source: 'class-sectors' };
  }
  return fallback ? { durationMs: fallback, source: 'configured-default' } : null;
}

export function checkpointGraceMs(expectedSegmentMs, options = {}) {
  const expected = positiveInteger(expectedSegmentMs);
  if (!expected) return null;
  const minimum = positiveInteger(options.minimumMs) ?? 1200;
  const maximum = Math.max(minimum, positiveInteger(options.maximumMs) ?? 3000);
  const ratioValue = finiteNumber(options.ratio);
  const ratio = ratioValue !== null && ratioValue > 0 ? ratioValue : 0.25;
  return Math.round(clamp(expected * ratio, minimum, maximum));
}

export function projectCourseProgress(anchorProgress, nextCheckpointProgress, elapsedMs, lapDurationMs, options = {}) {
  const anchor = finiteNumber(anchorProgress);
  const next = finiteNumber(nextCheckpointProgress);
  const elapsed = finiteNumber(elapsedMs);
  const lapDuration = positiveInteger(lapDurationMs);
  if (anchor === null || next === null || elapsed === null || elapsed < 0 || !lapDuration
      || anchor < 0 || anchor >= 1 || next <= anchor || next > 1) return null;
  const expectedSegmentMs = Math.round((next - anchor) * lapDuration);
  const graceMs = checkpointGraceMs(expectedSegmentMs, options);
  const projected = anchor + (elapsed / lapDuration);
  if (elapsed <= expectedSegmentMs) {
    return { courseProgress: projected, state: 'projected', expectedSegmentMs, graceMs };
  }
  if (elapsed <= expectedSegmentMs + graceMs) {
    return { courseProgress: projected, state: 'awaiting-checkpoint', expectedSegmentMs, graceMs };
  }
  return { courseProgress: next, state: 'holding-checkpoint', expectedSegmentMs, graceMs };
}

export function estimateLapPacedProgress(
  standing,
  raceElapsedMs,
  currentLapElapsedMs,
  boundaries,
  lapDurationMs,
  options = {},
) {
  const sectorCount = positiveInteger(standing?.sectorCount);
  const normalized = normalizedBoundaries(boundaries, sectorCount);
  const lapElapsed = finiteNumber(currentLapElapsedMs);
  if (!normalized || lapElapsed === null || lapElapsed < 0 || !positiveInteger(lapDurationMs)) return null;

  const markerIndex = finiteNumber(standing?.lastMarkerIndex);
  const markerRaceMs = finiteNumber(standing?.lastMarkerRaceMs);
  const raceElapsed = finiteNumber(raceElapsedMs);
  const clockToleranceValue = finiteNumber(options.clockToleranceMs);
  const clockToleranceMs = clamp(clockToleranceValue ?? 500, 0, 5000);
  const markerValid = Number.isInteger(markerIndex) && markerIndex >= 0 && markerIndex < sectorCount
    && Number.isInteger(markerRaceMs) && markerRaceMs >= 0
    && raceElapsed !== null && raceElapsed + clockToleranceMs >= markerRaceMs;
  const anchorIndex = markerValid ? markerIndex : 0;
  const elapsedSinceAnchorMs = markerValid ? Math.max(0, raceElapsed - markerRaceMs) : lapElapsed;
  const projection = projectCourseProgress(
    normalized[anchorIndex],
    normalized[anchorIndex + 1],
    elapsedSinceAnchorMs,
    lapDurationMs,
    options,
  );
  if (!projection) return null;
  return {
    ...projection,
    anchorIndex,
    nextMarkerIndex: (anchorIndex + 1) % sectorCount,
    elapsedSinceAnchorMs,
    markerConfirmed: markerValid,
  };
}

export function normalizeLapHistory(state, maximumEntries = 64) {
  if (!Array.isArray(state?.lapHistory)) return [];
  const limit = Math.max(0, Math.min(64, Math.floor(finiteNumber(maximumEntries) ?? 64)));
  const sectorCountByCar = new Map((Array.isArray(state?.standings) ? state.standings : [])
    .map((standing) => [
      typeof standing?.carId === 'string' ? standing.carId.trim() : '',
      finiteNumber(standing?.sectorCount),
    ])
    .filter(([carId, sectorCount]) => carId && Number.isInteger(sectorCount) && sectorCount > 0));
  return state.lapHistory
    .map((entry) => {
      const carId = typeof entry?.carId === 'string' ? entry.carId.trim() : '';
      const lap = finiteNumber(entry?.lap);
      const completedAtRaceMs = finiteNumber(entry?.completedAtRaceMs);
      const lapTimeMs = finiteNumber(entry?.lapTimeMs);
      const sectorCount = sectorCountByCar.get(carId);
      const sectors = new Set();
      const sectorTimes = Array.isArray(entry?.sectorTimes)
        ? entry.sectorTimes.map((timing) => {
          const sector = finiteNumber(timing?.sector);
          const timeMs = finiteNumber(timing?.timeMs);
          if (!Number.isInteger(sector) || sector < 1 || sector > sectorCount
              || sectors.has(sector) || !Number.isInteger(timeMs) || timeMs <= 0) {
            return null;
          }
          sectors.add(sector);
          return { sector, timeMs };
        })
        : null;
      if (!carId || !Number.isInteger(lap) || lap < 1
          || !Number.isInteger(completedAtRaceMs) || completedAtRaceMs < 0
          || !Number.isInteger(lapTimeMs) || lapTimeMs <= 0
          || !Number.isInteger(sectorCount) || sectorCount < 1
          || !sectorTimes || sectorTimes.length > sectorCount || sectorTimes.some((timing) => !timing)) {
        return null;
      }
      sectorTimes.sort((left, right) => left.sector - right.sector);
      return { carId, lap, completedAtRaceMs, lapTimeMs, sectorTimes };
    })
    .filter(Boolean)
    .sort((left, right) => right.completedAtRaceMs - left.completedAtRaceMs
      || left.carId.localeCompare(right.carId)
      || right.lap - left.lap)
    .slice(0, limit);
}

export function formatGap(milliseconds) {
  const value = finiteNumber(milliseconds);
  if (value === null) return '--';
  if (value === 0) return 'LEADER';
  return `+${(Math.max(0, value) / 1000).toFixed(3)}`;
}

export function formatStandingGap(standing) {
  const lapDelta = finiteNumber(standing?.lapDeltaToAhead);
  if (lapDelta !== null && lapDelta > 0) return `+${lapDelta} LAP`;
  return standing?.position === 1 ? 'LEADER' : formatGap(standing?.intervalToAheadMs);
}

export function displayRaceStatus(state) {
  const phase = displayRacePhase(state?.phase);
  const flag = String(state?.flag || 'none').toLowerCase();
  if (flag === 'yellow') return 'YELLOW FLAG';
  if (flag === 'red') return 'RED FLAG';
  if (flag === 'finish') return 'FINISHED';
  return phase;
}

export function raceClockValue(state, elapsedSinceSnapshotMs = 0, lapHistory = null) {
  if (!state) return null;
  const elapsed = Math.max(0, finiteNumber(elapsedSinceSnapshotMs) ?? 0);
  if (state.phase === 'countdown') {
    const startAtMs = finiteNumber(state.startAtMs);
    const serverTimeMs = finiteNumber(state.serverTimeMs);
    if (startAtMs !== null && serverTimeMs !== null) {
      return Math.max(0, startAtMs - serverTimeMs - elapsed);
    }
    const countdown = finiteNumber(state.countdown);
    return countdown === null ? null : Math.max(0, countdown * 1000 - elapsed);
  }
  const leader = (state.standings || []).find((standing) => standing.position === 1)
    || state.standings?.[0];
  const base = finiteNumber(leader?.allTimeMs);
  if (state.allTimeMode !== 'countdown') {
    const currentLap = currentLapClockValue(leader, state, elapsed);
    const reconstructed = reconstructRaceElapsedMs(leader, lapHistory, currentLap);
    if (reconstructed !== null) return reconstructed;
  }
  if (base === null) return null;
  if (state.phase !== 'green' || leader?.status !== 'racing') return base;
  return state.allTimeMode === 'countdown' ? Math.max(0, base - elapsed) : base + elapsed;
}

export function currentLapClockValue(standing, state, elapsedSinceSnapshotMs = 0) {
  const base = finiteNumber(standing?.currentLapMs);
  if (base === null) return null;
  if (state?.phase !== 'green' || standing?.status !== 'racing') return base;
  return base + Math.max(0, finiteNumber(elapsedSinceSnapshotMs) ?? 0);
}

export function reconstructRaceElapsedMs(standing, lapHistory, currentLapElapsedMs) {
  const carId = typeof standing?.carId === 'string' ? standing.carId.trim() : '';
  const currentLap = finiteNumber(currentLapElapsedMs);
  if (!carId || currentLap === null || currentLap < 0 || !Array.isArray(lapHistory)) return null;
  const latestCompletedLap = lapHistory.find((entry) => entry?.carId === carId
    && Number.isInteger(finiteNumber(entry?.completedAtRaceMs))
    && finiteNumber(entry.completedAtRaceMs) >= 0);
  if (!latestCompletedLap) return null;
  return latestCompletedLap.completedAtRaceMs + currentLap;
}

export function elapsedSinceRaceMarkerMs(standing, raceElapsedMs) {
  const raceElapsed = finiteNumber(raceElapsedMs);
  const markerRaceMs = finiteNumber(standing?.lastMarkerRaceMs);
  if (raceElapsed === null || markerRaceMs === null || markerRaceMs < 0) return null;
  return Math.max(0, raceElapsed - markerRaceMs);
}

export function normalizeObserverConfig(config) {
  if (!config || !Array.isArray(config.cars) || config.cars.length < 1 || config.cars.length > 4) {
    throw new Error('observer config requires 1 to 4 cars');
  }
  const devices = new Set();
  const carIds = new Set();
  const colors = ['green', 'yellow', 'cyan', 'red'];
  const cars = config.cars.map((car, index) => {
    const device = String(car?.device || '').trim();
    const carId = String(car?.carId || '').trim();
    if (!device || !carId || devices.has(device) || carIds.has(carId)) {
      throw new Error(`invalid or duplicate observer car at index ${index}`);
    }
    devices.add(device);
    carIds.add(carId);
    const displayNumber = String(car.displayNumber || index + 1).padStart(2, '0');
    return {
      device,
      carId,
      displayNumber,
      driver: String(car.driver || '').trim(),
      initials: String(car.initials || displayNumber).trim().slice(0, 3),
      portraitUrl: String(car.portraitUrl || '').trim(),
      flip: car.flip !== false,
      color: colors[index],
    };
  });
  const motion = config.motion || {};
  const motionNumber = (name, fallback, minimum, maximum) => {
    const value = motion[name] === undefined ? fallback : finiteNumber(motion[name]);
    if (value === null || value < minimum || value > maximum) {
      throw new Error(`observer motion.${name} must be ${minimum}..${maximum}`);
    }
    return value;
  };
  const checkpointGraceMinMs = motionNumber('checkpointGraceMinMs', 1200, 100, 10000);
  const checkpointGraceMaxMs = motionNumber('checkpointGraceMaxMs', 3000, checkpointGraceMinMs, 15000);
  return {
    trackName: String(config.trackName || 'RACE CONTROL').trim(),
    cars,
    motion: {
      defaultLapMs: motionNumber('defaultLapMs', 20000, 3000, 300000),
      checkpointGraceMinMs,
      checkpointGraceMaxMs,
      checkpointGraceRatio: motionNumber('checkpointGraceRatio', 0.25, 0.01, 1),
      markerClockToleranceMs: motionNumber('markerClockToleranceMs', 500, 0, 5000),
      markerCorrectionMs: motionNumber('markerCorrectionMs', 360, 0, 3000),
      pitEntryMs: motionNumber('pitEntryMs', 650, 0, 5000),
      pitExitMs: motionNumber('pitExitMs', 1800, 100, 10000),
      pitServiceProgress: motionNumber('pitServiceProgress', 0.556, 0, 1),
      pitRejoinProgress: motionNumber('pitRejoinProgress', 0.184, 0, 1),
    },
  };
}

export function standingsByConfiguredCar(configCars, raceState) {
  const byCarId = new Map((raceState?.standings || []).map((standing) => [standing.carId, standing]));
  return configCars
    .map((car, index) => ({ car, standing: byCarId.get(car.carId) || null, index }))
    .sort((left, right) => {
      const leftPosition = finiteNumber(left.standing?.position) ?? Number.MAX_SAFE_INTEGER;
      const rightPosition = finiteNumber(right.standing?.position) ?? Number.MAX_SAFE_INTEGER;
      return leftPosition - rightPosition || left.index - right.index;
    });
}

export function countActiveVideos(connectionByCar) {
  return Array.from(connectionByCar.values()).filter((connection) => connection.videoActive === true).length;
}

export function deriveSituations(
  configCars,
  raceState,
  healthByCar,
  connectionByCar,
  telemetryByCar = new Map(),
  vehicleEventByCar = new Map(),
  pitByCar = new Map(),
  nowUnixMs = Date.now(),
) {
  const situations = [];
  const configById = new Map(configCars.map((car) => [car.carId, car]));
  const flag = String(raceState?.flag || 'none').toLowerCase();
  if (flag === 'yellow' || flag === 'red') {
    situations.push({
      type: 'flag',
      label: 'RACE CONTROL',
      primary: `${flag.toUpperCase()} FLAG`,
      detail: String(raceState?.message || displayRacePhase(raceState?.phase)).toUpperCase(),
      tone: flag === 'red' ? 'limited' : 'battle',
      priority: 200,
    });
  } else if (raceState?.message) {
    situations.push({
      type: 'message',
      label: 'RACE CONTROL',
      primary: String(raceState.message).slice(0, 48),
      detail: displayRacePhase(raceState.phase),
      tone: 'watch',
      priority: 120,
    });
  }
  for (const car of configCars) {
    const health = healthByCar.get(car.carId);
    const connection = connectionByCar.get(car.carId);
    const vehicleEvent = vehicleEventByCar.get(car.carId);
    const pit = pitByCar.get(car.carId);
    if (pit?.present) {
      const complete = pit.serviceState === 'complete';
      situations.push({
        type: complete ? 'pit-complete' : pit.lastAcceptedTick > 0 ? 'pit-service' : 'pit-in',
        label: complete ? 'SERVICE COMPLETE' : pit.lastAcceptedTick > 0 ? 'PIT SERVICE' : 'PIT IN',
        primary: `CAR ${car.displayNumber} · HP ${Math.round(pit.hp)}`,
        detail: pit.lastAcceptedTick > 0 ? `RECOVERY TICK ${pit.lastAcceptedTick}` : 'MARKER CONFIRMED',
        tone: 'recovery',
        priority: complete ? 175 : 165,
      });
    } else if (pit?.exitedAtUnixMs > 0
      && ['marker_lost', 'observation_stale', 'video_invalid'].includes(pit.exitReason)
      && nowUnixMs - pit.exitedAtUnixMs <= 4000) {
      situations.push({
        type: 'pit-out',
        label: 'PIT OUT',
        primary: `CAR ${car.displayNumber} · HP ${Math.round(pit.hp)}`,
        detail: String(pit.exitReason || 'MARKER LOST').replaceAll('_', ' ').toUpperCase(),
        tone: 'recovery',
        priority: 155,
      });
    }
    if (health && health.speedCap < 0.999) {
      situations.push({
        type: 'limited',
        label: 'DAMAGE LIMIT',
        primary: `CAR ${car.displayNumber} · HP ${Math.round(health.hp)}`,
        detail: `MAX SPEED ${Math.round(health.speedCap * 100)}%`,
        tone: health.mode === 'healthy' ? 'watch' : 'limited',
        priority: 100 + (100 - health.hp),
      });
    }
    if (connection && !connection.videoActive) {
      situations.push({
        type: 'connection',
        label: 'VIDEO STATUS',
        primary: `CAR ${car.displayNumber} · ${connection.state}`,
        detail: connection.detail || 'WAITING FOR VIDEO',
        tone: 'watch',
        priority: 30,
      });
    }
    if (vehicleEvent) {
      const event = vehicleEvent;
      const detail = event.damageApplied
        ? `DAMAGE -${Math.round(event.damage)} · HP ${Math.round(event.hpAfter)}`
        : event.suppressionReason === 'cooldown'
          ? `DAMAGE COOLDOWN · HP ${Math.round(event.hpAfter)}`
          : `NO DAMAGE · JERK ${Math.round(event.jerkMps3)}`;
      situations.push({
        type: 'impact',
        label: event.impactClass === 'severe' ? 'HEAVY IMPACT' : event.impactClass === 'strong' ? 'IMPACT' : 'GRAVEL',
        primary: `CAR ${car.displayNumber} · ${event.magnitudeMps2.toFixed(1)} m/s²`,
        detail,
        tone: event.impactClass === 'weak' ? 'watch' : 'limited',
        priority: event.impactClass === 'severe' ? 180 : event.impactClass === 'strong' ? 150 : 80,
      });
    }
  }
  for (const standing of raceState?.standings || []) {
    const status = String(standing.status || '').toLowerCase();
    const statusCar = configById.get(standing.carId);
    if (statusCar && (status === 'finished' || status === 'retired')) {
      situations.push({
        type: status,
        label: status === 'finished' ? 'FINISH' : 'RETIRED',
        primary: `CAR ${statusCar.displayNumber} · ${status.toUpperCase()}`,
        detail: formatDuration(standing.allTimeMs),
        tone: status === 'finished' ? 'recovery' : 'watch',
        priority: status === 'finished' ? 160 : 90,
      });
    }
    const gap = finiteNumber(standing.intervalToAheadMs);
    if (gap === null || gap <= 0 || gap > 1000) continue;
    const car = configById.get(standing.carId);
    const ahead = (raceState.standings || []).find((candidate) => candidate.position === standing.position - 1);
    const aheadCar = configById.get(ahead?.carId);
    if (!car || !aheadCar) continue;
    situations.push({
      type: 'battle',
      label: `BATTLE FOR P${standing.position - 1}`,
      primary: `CAR ${aheadCar.displayNumber} / CAR ${car.displayNumber}`,
      detail: `${(gap / 1000).toFixed(3)}s`,
      tone: 'battle',
      priority: 70,
    });
  }
  return situations.sort((left, right) => right.priority - left.priority).slice(0, 4);
}
