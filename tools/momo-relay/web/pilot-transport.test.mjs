import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const source = await readFile(new URL('./pilot.js', import.meta.url), 'utf8');

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
