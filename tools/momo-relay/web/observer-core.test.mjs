import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const source = await readFile(new URL('./observer-core.js', import.meta.url), 'utf8');
const observerSource = await readFile(new URL('./observer.js', import.meta.url), 'utf8');
const observerCore = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`);
const { classifyCompletedSectorTime } = observerCore;

test('completed sector classifies an in-progress overall best before bestMs is published', () => {
  assert.equal(classifyCompletedSectorTime(4900, 5200, 5000), 'overall-best');
  assert.equal(classifyCompletedSectorTime(5000, 5200, 5000), 'overall-best');
});

test('completed sector classifies an in-progress personal best before bestMs is published', () => {
  assert.equal(classifyCompletedSectorTime(5000, 5200, 4500), 'personal-best');
  assert.equal(classifyCompletedSectorTime(5200, 5200, 4500), 'personal-best');
});

test('completed sector keeps standard and first-record classifications', () => {
  assert.equal(classifyCompletedSectorTime(5400, 5200, 4500), 'standard-time');
  assert.equal(classifyCompletedSectorTime(5400, null, 5000), 'personal-best');
  assert.equal(classifyCompletedSectorTime(5400, null, null), 'overall-best');
  assert.equal(classifyCompletedSectorTime(null, 5200, 4500), '');
});

test('web observer keeps WebRTC video-only and receives downstream data over signaling WebSocket', () => {
  assert.doesNotMatch(observerSource, /\.createDataChannel\(/);
  assert.match(observerSource, /message\.type === 'race-state'/);
  assert.match(observerSource, /message\.type === 'telemetry'/);
  assert.match(observerSource, /message\.type === 'command'/);
  assert.match(observerSource, /message\.type === 'vehicle-event'/);
  assert.match(observerSource, /DATA WS/);
  assert.doesNotMatch(observerSource, /RACE DC/);
});
