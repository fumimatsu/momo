import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const source = await readFile(new URL('./pilot.js', import.meta.url), 'utf8');
const html = await readFile(new URL('./pilot.html', import.meta.url), 'utf8');

test('local Relay Pilot uses WebSocket downlink and control-only DataChannels', () => {
  assert.match(source, /function usesWebSocketDownlink\(\) \{\s*return usesRelayTransport\(\) && isRelaySignaling\(\);/);
  assert.match(source, /createDataChannel\(usesRelayTransport\(\) \? 'momo-command' : 'serial'/);
  assert.match(source, /attachDriveChannel\(peer\.createDataChannel\('momo-drive'/);
  assert.match(source, /if \(!usesWebSocketDownlink\(\)\) \{[\s\S]*createDataChannel\('momo-telemetry'[\s\S]*createDataChannel\('momo-race'[\s\S]*createDataChannel\('momo-events'/);
  assert.match(source, /case 'telemetry':[\s\S]*handleDownlinkMessage\(message\.data, 'websocket'\)/);
  assert.match(source, /case 'race-state':[\s\S]*handleRaceStateMessage\(message\.data\)/);
  assert.match(source, /case 'vehicle-event':[\s\S]*handleVehicleEventMessage\(message\.data\)/);
});

test('retired local SCTP downlink probe is not part of the Pilot contract', () => {
  assert.doesNotMatch(source, /dcTextProbe|datachannel-probe|DC_TEXT_PROBE/);
  assert.match(source, /downlink_transport: usesWebSocketDownlink\(\) \? 'websocket' : 'datachannel'/);
});

test('ESC vitals remain visible after Drive Off and use the 2S voltage thresholds', () => {
  assert.match(source, /VEHICLE_BATTERY_CELLS \* 3\.65/);
  assert.match(source, /VEHICLE_BATTERY_CELLS \* 3\.5/);
  assert.match(source, /const hasEscTelemetry = Boolean\(snapshot\?\.state\?\.esc\)/);
  assert.match(source, /const hidden = !\(driveUiVisible \|\| hasEscTelemetry\)/);
  assert.match(source, /if \(vehicleStatusCluster\.hidden !== hidden\) \{\s*vehicleStatusCluster\.hidden = hidden;/);
  assert.match(source, /const snapshot = getCurrentEscSnapshot\(nowMs\);\s*updateVehicleStatusClusterVisibility\(snapshot\);/);
});

test('race banners keep the rival identity in the prominent heading', () => {
  assert.match(source, /`\$\{critical \? 'REAR ATTACK' : 'REAR PRESSURE'\} · \$\{rivalName\}`/);
  assert.match(source, /`BLUE FLAG · \$\{rivalName\}`/);
  assert.match(html, /\.rear-attention \{[\s\S]*?background: rgba\(46, 29, 2, 0\.5\);/);
  assert.match(html, /\.rear-attention output \{[\s\S]*?font-size: 36px;/);
  assert.match(html, /\.race-milestone \{[\s\S]*?background: rgba\(2, 24, 33, 0\.52\);/);
  assert.match(html, /\.race-milestone output \{[\s\S]*?font-size: 38px;/);
});

test('race map projects the first lap from elapsed race time before a marker anchor exists', () => {
  assert.match(source, /resolveRaceMapElapsedMs\([\s\S]*?\{ allTimeMode \},/);
  assert.match(source, /if \(markerIndex === null && raceElapsedMs !== null\) \{[\s\S]*?raceElapsedMs \+ localAdvanceMs/);
  assert.match(source, /const raceCourseMotionByCar = new Map\(\)/);
});
