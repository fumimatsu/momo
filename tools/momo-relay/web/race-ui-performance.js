(function initRaceUiPerformance(root, factory) {
  if (typeof module === 'object' && module.exports) {
    module.exports = factory();
    return;
  }
  root.MomoRaceUiPerformance = factory();
}(typeof globalThis !== 'undefined' ? globalThis : this, () => {
  'use strict';

  const DEFAULT_MAXIMUM_CARS = 100;
  const DEFAULT_SAMPLE_LIMIT = 600;
  const DEFAULT_PATH_SAMPLES = 1024;
  const FIRST_FOUR_OFFSETS = Object.freeze([[-15, -15], [15, -15], [-15, 15], [15, 15]]);

  function clamp(value, minimum, maximum) {
    return Math.max(minimum, Math.min(maximum, value));
  }

  function normalizeFixtureCarCount(value, fallback = 0, maximum = DEFAULT_MAXIMUM_CARS) {
    const fallbackValue = Number.isInteger(fallback) ? fallback : 0;
    const limit = Number.isInteger(maximum) && maximum > 0 ? maximum : DEFAULT_MAXIMUM_CARS;
    const parsed = Number(value);
    return Number.isInteger(parsed) ? clamp(parsed, 0, limit) : clamp(fallbackValue, 0, limit);
  }

  function normalizeSnapshotRate(value, fallback = 0) {
    const supported = new Set([0, 1, 5, 10]);
    const parsed = Number(value);
    if (Number.isInteger(parsed) && supported.has(parsed)) return parsed;
    const fallbackValue = Number(fallback);
    return Number.isInteger(fallbackValue) && supported.has(fallbackValue) ? fallbackValue : 0;
  }

  function createRaceFixture(count, options = {}) {
    const fieldSize = normalizeFixtureCarCount(count, 4);
    if (fieldSize < 1) {
      throw new Error('Race UI fixture requires at least one car.');
    }
    const prefix = String(options.carPrefix || 'FPV-');
    const lapDurationMs = Math.max(3000, Number(options.lapDurationMs) || 24_000);
    const selfPosition = clamp(
      Number.isInteger(options.selfPosition) ? options.selfPosition : Math.ceil(fieldSize / 2),
      1,
      fieldSize,
    );
    const carIdFor = (position) => `${prefix}${String(position).padStart(3, '0')}`;
    const rivals = Array.from({ length: fieldSize }, (_, index) => {
      const position = index + 1;
      const phaseOffset = ((fieldSize - index) / fieldSize + 0.08) % 1;
      return {
        carId: carIdFor(position),
        driver: `D${String(position).padStart(3, '0')}`,
        position,
        status: 'racing',
        lap: 3,
        sectorCount: 3,
        currentSector: Math.min(3, Math.floor(phaseOffset * 3) + 1),
        currentLapMs: Math.floor(phaseOffset * lapDurationMs),
        lapTimeMs: lapDurationMs + ((position % 7) * 70),
        bestLapMs: lapDurationMs - 300 + ((position % 5) * 60),
        intervalToAheadMs: position === 1 ? null : 600 + ((position % 9) * 120),
        lapDeltaToAhead: 0,
      };
    });
    return {
      phase: 'RUNNING',
      phaseCode: 'green',
      carId: carIdFor(selfPosition),
      lap: 3,
      lapCount: 10,
      position: selfPosition,
      fieldSize,
      totalTimeMs: 72_430,
      currentLapMs: rivals[selfPosition - 1].currentLapMs,
      lastLapMs: lapDurationMs + 280,
      bestLapMs: lapDurationMs - 300,
      overallBestLapMs: lapDurationMs - 420,
      clockRunning: false,
      rivals,
    };
  }

  function createObserverCars(count, templates = []) {
    const fieldSize = normalizeFixtureCarCount(count, templates.length || 4);
    const colors = ['cyan', 'yellow', 'green', 'red'];
    return Array.from({ length: fieldSize }, (_, index) => {
      const template = templates[index % Math.max(1, templates.length)] || {};
      if (index < templates.length) {
        return {
          ...template,
          speedProfile: typeof template.speedProfileId === 'string'
            ? template.speedProfileId
            : typeof template.speedProfile === 'string' ? template.speedProfile : '',
        };
      }
      const position = index + 1;
      const suffix = String(position).padStart(3, '0');
      return {
        device: `fixture-${suffix}`,
        sourceId: `fixture-${suffix}`,
        vehicleId: `fixture-vehicle-${suffix}`,
        carId: `CP-${suffix}`,
        displayNumber: suffix,
        driver: `DRIVER ${suffix}`,
        color: colors[index % colors.length],
        flip: false,
        speedProfile: typeof template.speedProfileId === 'string'
          ? template.speedProfileId
          : typeof template.speedProfile === 'string' ? template.speedProfile : '',
      };
    });
  }

  function createSvgPathLookup(path, sampleCount = DEFAULT_PATH_SAMPLES) {
    if (!path || typeof path.getTotalLength !== 'function'
        || typeof path.getPointAtLength !== 'function') {
      return null;
    }
    const length = Number(path.getTotalLength());
    if (!Number.isFinite(length) || length <= 0) {
      return null;
    }
    const samples = clamp(
      Number.isInteger(sampleCount) ? sampleCount : DEFAULT_PATH_SAMPLES,
      32,
      4096,
    );
    const points = new Float32Array((samples + 1) * 2);
    for (let index = 0; index <= samples; index += 1) {
      const point = path.getPointAtLength((index / samples) * length);
      points[index * 2] = Number(point?.x) || 0;
      points[(index * 2) + 1] = Number(point?.y) || 0;
    }
    return Object.freeze({ length, samples, points });
  }

  function pointAtProgress(lookup, progress) {
    if (!lookup?.points || !Number.isInteger(lookup.samples) || lookup.samples < 1) {
      return null;
    }
    const value = Number(progress);
    if (!Number.isFinite(value)) {
      return null;
    }
    let normalized = ((value % 1) + 1) % 1;
    if (value > 0 && Math.abs(normalized) < 0.000001) normalized = 1;
    const scaled = normalized * lookup.samples;
    const lower = Math.min(lookup.samples, Math.floor(scaled));
    const upper = Math.min(lookup.samples, lower + 1);
    const ratio = scaled - lower;
    const lowerOffset = lower * 2;
    const upperOffset = upper * 2;
    return {
      x: lookup.points[lowerOffset]
        + ((lookup.points[upperOffset] - lookup.points[lowerOffset]) * ratio),
      y: lookup.points[lowerOffset + 1]
        + ((lookup.points[upperOffset + 1] - lookup.points[lowerOffset + 1]) * ratio),
    };
  }

  function markerOffset(index, fieldSize) {
    if (fieldSize <= FIRST_FOUR_OFFSETS.length) {
      return FIRST_FOUR_OFFSETS[index] || [0, 0];
    }
    const radiusLimit = fieldSize > 48 ? 8 : 14;
    const ring = Math.floor(index / 12) + 1;
    const radius = Math.min(radiusLimit, 2 + (ring * 2));
    const angle = index * 2.399963229728653;
    return [Math.cos(angle) * radius, Math.sin(angle) * radius];
  }

  function percentile(sorted, ratio) {
    if (!sorted.length) return null;
    const index = clamp(Math.ceil(sorted.length * ratio) - 1, 0, sorted.length - 1);
    return sorted[index];
  }

  function createDurationSampler(limit = DEFAULT_SAMPLE_LIMIT) {
    const maximum = clamp(
      Number.isInteger(limit) ? limit : DEFAULT_SAMPLE_LIMIT,
      16,
      10_000,
    );
    const samples = [];
    return Object.freeze({
      record(value) {
        const duration = Number(value);
        if (!Number.isFinite(duration) || duration < 0) return false;
        samples.push(duration);
        if (samples.length > maximum) samples.splice(0, samples.length - maximum);
        return true;
      },
      reset() {
        samples.length = 0;
      },
      snapshot() {
        const sorted = [...samples].sort((left, right) => left - right);
        const total = sorted.reduce((sum, value) => sum + value, 0);
        return Object.freeze({
          samples: sorted.length,
          meanMs: sorted.length ? total / sorted.length : null,
          p50Ms: percentile(sorted, 0.5),
          p95Ms: percentile(sorted, 0.95),
          maxMs: sorted.length ? sorted[sorted.length - 1] : null,
          lastMs: samples.length ? samples[samples.length - 1] : null,
        });
      },
    });
  }

  function createLongTaskTracker() {
    const entries = [];
    let observer = null;
    return Object.freeze({
      start() {
        if (observer || typeof PerformanceObserver !== 'function') return false;
        try {
          observer = new PerformanceObserver((list) => {
            for (const entry of list.getEntries()) {
              entries.push(Number(entry.duration) || 0);
              if (entries.length > DEFAULT_SAMPLE_LIMIT) entries.shift();
            }
          });
          observer.observe({ type: 'longtask', buffered: true });
          return true;
        } catch (_) {
          observer = null;
          return false;
        }
      },
      stop() {
        observer?.disconnect();
        observer = null;
      },
      reset() {
        entries.length = 0;
      },
      snapshot() {
        return Object.freeze({
          supported: typeof PerformanceObserver === 'function',
          count: entries.length,
          totalMs: entries.reduce((sum, value) => sum + value, 0),
          maxMs: entries.length ? Math.max(...entries) : null,
        });
      },
    });
  }

  return Object.freeze({
    DEFAULT_MAXIMUM_CARS,
    createDurationSampler,
    createLongTaskTracker,
    createObserverCars,
    createRaceFixture,
    createSvgPathLookup,
    markerOffset,
    normalizeFixtureCarCount,
    normalizeSnapshotRate,
    pointAtProgress,
  });
}));
