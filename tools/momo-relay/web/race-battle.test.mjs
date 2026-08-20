import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import test from 'node:test';

const require = createRequire(import.meta.url);
const battle = require('./race-battle.js');

test('race map uses elapsed all-time during the first lap', () => {
  assert.equal(battle.resolveRaceMapElapsedMs({
    allTimeMs: 6400,
    lastMarkerRaceMs: 4000,
  }, [], { allTimeMode: 'elapsed' }), 6400);
});

test('race map does not treat countdown all-time as elapsed time', () => {
  assert.equal(battle.resolveRaceMapElapsedMs({
    allTimeMs: 58000,
    lastMarkerRaceMs: 4000,
  }, [], { allTimeMode: 'countdown' }), 4000);
});

test('explicit race elapsed time remains authoritative', () => {
  assert.equal(battle.resolveRaceMapElapsedMs({
    raceElapsedMs: 7200,
    allTimeMs: 6500,
    lastMarkerRaceMs: 4000,
  }, [], { allTimeMode: 'elapsed' }), 7200);
});
