'use strict';

const assert = require('node:assert/strict');
const test = require('node:test');
const telemetry = require('./telemetry.js');

function state(seq, forwardMps2, verticalMps2) {
  return {
    v: 2,
    k: 's',
    src: 'imu0',
    boot: 'test-boot',
    seq,
    t_us: seq * 33333,
    m: { a: [forwardMps2, 0, verticalMps2], y: 0 },
    q: { p: 33333, f: ['flu_axes'] },
  };
}

test('battery voltage uses warning-inclusive and critical-exclusive thresholds', () => {
  const classify = telemetry.classifyLowTelemetryValue;
  assert.equal(classify(7.31, 7.3, 7.0), 'normal');
  assert.equal(classify(7.3, 7.3, 7.0), 'warning');
  assert.equal(classify(7.0, 7.3, 7.0), 'warning');
  assert.equal(classify(6.99, 7.3, 7.0), 'critical');
  assert.equal(classify(Number.NaN, 7.3, 7.0), 'unavailable');
});

test('battery voltage hysteresis prevents warning color flicker', () => {
  const classify = telemetry.classifyLowTelemetryValue;
  assert.equal(classify(7.4, 7.3, 7.0, 'warning', 0.2), 'warning');
  assert.equal(classify(7.51, 7.3, 7.0, 'warning', 0.2), 'normal');
  assert.equal(classify(7.1, 7.3, 7.0, 'critical', 0.2), 'critical');
  assert.equal(classify(7.2, 7.3, 7.0, 'critical', 0.2), 'warning');
});

test('surface roughness stays quiet at rest and reacts independently from impact', () => {
  const extractor = new telemetry.MotionFeatureExtractor();
  let snapshot = null;
  for (let seq = 0; seq < 30; seq += 1) {
    snapshot = extractor.ingest(state(seq, 0.01, seq % 2 ? 0.01 : -0.01), seq * 33.333);
  }
  assert.ok(snapshot.surfaceRoughness < 0.01);

  for (let seq = 30; seq < 50; seq += 1) {
    snapshot = extractor.ingest(state(seq, 0, seq % 2 ? 0.9 : -0.9), seq * 33.333);
  }
  assert.ok(snapshot.surfaceRoughness > 0.3);
  const beforeImpact = snapshot.surfaceRoughness;

  snapshot = extractor.ingest(state(50, 9, 6), 50 * 33.333);
  assert.equal(snapshot.impact, true);
  assert.ok(snapshot.surfaceRoughness < beforeImpact);
});
