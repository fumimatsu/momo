'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const { createRearAttentionTracker } = require('./web/race-battle.js');

function sample(markerIndex, markerRaceMs, gapMs, overrides = {}) {
  return {
    raceRunId: 'rr_1',
    phaseCode: 'green',
    self: { carId: 'CP-1', lap: 3 },
    behind: {
      carId: 'CP-2',
      driver: 'RIN',
      lap: 3,
      lastMarkerIndex: markerIndex,
      lastMarkerRaceMs: markerRaceMs,
      intervalToAheadMs: gapMs,
    },
    ...overrides,
  };
}

test('rear attention uses the first checkpoint as a baseline', () => {
  const tracker = createRearAttentionTracker();
  const baseline = tracker.evaluate(sample(1, 10_000, 2800));
  assert.equal(baseline.active, false);
  assert.equal(tracker.evaluate(sample(1, 10_000, 1200)).active, false);
});

test('rear attention is immediately visible when the first known gap is close', () => {
  const tracker = createRearAttentionTracker();
  const result = tracker.evaluate(sample(1, 10_000, 1200));
  assert.equal(result.active, true);
  assert.equal(result.severity, 'warning');
  assert.equal(result.trend, 'unknown');
  assert.equal(result.shouldPulse, true);
});

test('rear attention warns when the immediate follower closes inside 2.5 seconds', () => {
  const tracker = createRearAttentionTracker();
  tracker.evaluate(sample(1, 10_000, 2900));
  const result = tracker.evaluate(sample(2, 20_000, 2400));
  assert.equal(result.active, true);
  assert.equal(result.severity, 'warning');
  assert.equal(result.gapMs, 2400);
  assert.equal(result.closingMs, 500);
  assert.equal(result.trend, 'closing');
  assert.equal(result.shouldPulse, true);
});

test('rear attention becomes critical inside one second', () => {
  const tracker = createRearAttentionTracker();
  tracker.evaluate(sample(2, 20_000, 1250));
  const result = tracker.evaluate(sample(0, 30_000, 850));
  assert.equal(result.active, true);
  assert.equal(result.severity, 'critical');
  assert.equal(result.markerIndex, 0);
  assert.equal(result.shouldPulse, true);
});

test('rear attention stays visible while the follower opens a still-close gap', () => {
  const tracker = createRearAttentionTracker();
  tracker.evaluate(sample(1, 10_000, 1800));
  const opening = tracker.evaluate(sample(2, 20_000, 2100));
  assert.equal(opening.active, true);
  assert.equal(opening.trend, 'opening');
  assert.equal(opening.shouldPulse, false);
  const replacement = tracker.evaluate(sample(0, 30_000, 700, {
    behind: { ...sample(0, 30_000, 700).behind, carId: 'CP-3' },
  }));
  assert.equal(replacement.active, true);
  assert.equal(replacement.severity, 'critical');
  assert.equal(replacement.shouldPulse, true);
});

test('rear attention uses a release margin before hiding', () => {
  const tracker = createRearAttentionTracker();
  assert.equal(tracker.evaluate(sample(1, 10_000, 2400)).active, true);
  assert.equal(tracker.evaluate(sample(2, 20_000, 2700)).active, true);
  assert.equal(tracker.evaluate(sample(0, 30_000, 3100)).active, false);
});

test('rear attention resets outside green and requires checkpoint identity', () => {
  const tracker = createRearAttentionTracker();
  tracker.evaluate(sample(1, 10_000, 1800));
  assert.equal(tracker.evaluate(sample(2, 20_000, 900, { phaseCode: 'finished' })).active, false);
  assert.equal(tracker.evaluate(sample(3, 30_000, 600)).active, true);
  assert.equal(tracker.evaluate(sample(0, 40_000, 500, {
    behind: { carId: 'CP-2', lap: 3, intervalToAheadMs: 500 },
  })).active, false);
});

test('rear attention treats normalized null fields as missing', () => {
  const tracker = createRearAttentionTracker();

  assert.equal(tracker.evaluate(sample(0, 40_000, null, {
    behind: {
      carId: 'CP-2',
      driver: 'RIN',
      lap: 3,
      lastMarkerIndex: null,
      lastMarkerRaceMs: null,
      intervalToAheadMs: null,
    },
  })).active, false);
});

test('rear attention does not compare across a checkpoint with a missing gap', () => {
  const tracker = createRearAttentionTracker();

  assert.equal(tracker.evaluate(sample(1, 10_000, 2400)).active, true);
  assert.equal(tracker.evaluate(sample(2, 20_000, null)).active, false);
  const restored = tracker.evaluate(sample(3, 30_000, 800));
  assert.equal(restored.active, true);
  assert.equal(restored.trend, 'unknown');
  assert.equal(restored.shouldPulse, true);
});
