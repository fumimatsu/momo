import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const source = await readFile(new URL('./observer-core.js', import.meta.url), 'utf8');
const observerSource = await readFile(new URL('./observer.js', import.meta.url), 'utf8');
const raceFixture = JSON.parse(await readFile(new URL('../contracts/sector-progress.race-state-v2.json', import.meta.url), 'utf8'));
const observerCore = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`);
const { classifyCompletedSectorTime, parseRaceState, selectVideoDevices } = observerCore;

test('video device selection defaults to all and accepts device or car ID', () => {
  const cars = [
    { device: '11.3', carId: 'CP-1' },
    { device: '11.4', carId: 'CP-2' },
    { device: '11.5', carId: 'CP-3' },
  ];
  assert.deepEqual([...selectVideoDevices(cars)], ['11.3', '11.4', '11.5']);
  assert.deepEqual([...selectVideoDevices(cars, '11.3,CP-3')], ['11.3', '11.5']);
  assert.deepEqual([...selectVideoDevices(cars, 'none')], []);
  assert.throws(() => selectVideoDevices(cars, '11.9'), /unknown video device/);
});

test('web observer parses the canonical in-progress sector fixture', () => {
  const parsed = parseRaceState(raceFixture);
  assert.ok(parsed);
  const sector2 = parsed.standings[0].sectorTimes.find((entry) => entry.sector === 2);
  assert.equal(sector2.lastMs, 4700);
  assert.equal(sector2.bestMs, undefined);
});

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

test('team observer keeps selected per-car WebRTC video-only and uses one global Race WebSocket', () => {
  assert.doesNotMatch(observerSource, /\.createDataChannel\(/);
  assert.match(observerSource, /class RaceStateStream/);
  assert.match(observerSource, /new WebSocket\(createRaceStateWebSocketUrl\(this\.relayHost\)\)/);
  assert.match(observerSource, /url\.searchParams\.set\('client', 'web-observer'\)/);
  assert.match(observerSource, /raceClient = new RaceStateStream\(activeRelayHost, handleRaceState/);
  assert.match(observerSource, /message\.type === 'telemetry'/);
  assert.match(observerSource, /message\.type === 'command'/);
  assert.match(observerSource, /message\.type === 'vehicle-event'/);
  assert.match(observerSource, /DATA WS/);
  assert.match(observerSource, /RACE WS/);
  assert.doesNotMatch(observerSource, /RACE DC/);
  assert.match(observerSource, /params\.get\('videoDevices'\)/);
  assert.match(observerSource, /function syncSelectedTeamPeers\(\)/);
  assert.match(observerSource, /for \(const car of selectedTeamCars\(\)\)/);
  assert.match(observerSource, /syncSelectedTeamPeers\(\)/);
});

test('automatic HTTP Race fallback polls only while the Race WebSocket is unhealthy', () => {
  assert.match(observerSource, /raceFallbackEnabled = params\.get\('raceFallback'\) !== 'off'/);
  assert.match(observerSource, /RACE_STREAM_STALE_MS = 15_000/);
  assert.match(observerSource, /message\.type === 'race-heartbeat'/);
  assert.match(observerSource, /this\.activityTimer = window\.setTimeout\(\(\) => this\.scheduleReconnect\(generation\), RACE_STREAM_STALE_MS\)/);
  assert.match(observerSource, /if \(!raceFallbackEnabled \|\| raceStreamOpen\) \{\s*stopRaceStatePolling\(\)/);
  assert.match(observerSource, /syncRaceStateFallback\(relayHost\)/);
  assert.doesNotMatch(observerSource, /params\.get\('raceFallback'\) === 'http'/);
});

test('race rendering skips unchanged leaderboard, sector, and timing DOM trees', () => {
  assert.match(observerSource, /if \(signature === leaderboardSignature\) return/);
  assert.match(observerSource, /if \(signature === sectorRowsSignature\) return/);
  assert.match(observerSource, /if \(signature === timingRowsSignature\) return/);
});
