import {
  abbreviateDriverName,
  CurrentLapClockTracker,
  RaceStateDeduplicator,
  countActiveVideos,
  currentLapClockValue,
  deriveSituations,
  displayRaceStatus,
  elapsedSinceRaceMarkerMs,
  estimateVehicleSpeedKph,
  classifyCompletedSectorTime,
  classifyBestTime,
  estimateLapDurationMs,
  estimateLapPacedProgress,
  formatDuration,
  formatSplitTime,
  formatStandingGap,
	leaderboardPositionChange,
	mergeTeamObserverFleet,
  normalizeLapHistory,
  normalizeObserverConfig,
	normalizePilotDevicesSnapshot,
	normalizeTeamObserverDirectoryProjection,
	normalizeTeamVehicleSelection,
  parseControlCommand,
  parsePitPresence,
  parseRaceState,
	parseVehicleGameplay,
  parseVehicleHealth,
  projectCourseProgress,
  raceClockValue,
  raceParticipantCars,
  reconstructRaceElapsedMs,
  standingsByConfiguredCar,
  TEAM_OBSERVER_MAXIMUM_CARS,
} from './observer-core.js?v=20260825-team-observer-v19';

const raceUiPerformance = window.MomoRaceUiPerformance;
if (!raceUiPerformance?.createObserverCars || !raceUiPerformance?.createSvgPathLookup
    || !raceUiPerformance?.pointAtProgress || !raceUiPerformance?.createDurationSampler
    || !raceUiPerformance?.normalizeSnapshotRate) {
  throw new Error('MomoRaceUiPerformance is required.');
}
const courseLayout = window.MomoCourseLayout;
if (!courseLayout?.applyToSvg || courseLayout.id !== 'experience-v1'
    || !Array.isArray(courseLayout.sectorBoundaries)) {
  throw new Error('MomoCourseLayout experience-v1 is required.');
}
const startupParams = new URLSearchParams(location.search);
const UI_TEST_MODE = startupParams.get('uiTest') === '1';
const UI_TEST_CARS = raceUiPerformance.normalizeFixtureCarCount(
  startupParams.get('uiTestCars'),
  0,
  TEAM_OBSERVER_MAXIMUM_CARS,
);
const UI_METRICS_ENABLED = ['1', 'true', 'yes', 'on'].includes(
  String(startupParams.get('uiMetrics') || (UI_TEST_CARS > 0 ? '1' : '')).toLowerCase(),
);
const UI_TEST_SNAPSHOT_HZ = UI_TEST_MODE
  ? raceUiPerformance.normalizeSnapshotRate(startupParams.get('uiSnapshotHz'), 0)
  : 0;
const UI_TEST_METRICS_WARMUP_MS = 2000;

const RECONNECT_BASE_MS = 1000;
const RECONNECT_MAX_MS = 10000;
const DISCONNECTED_RECONNECT_MS = 3000;
const RACE_STATE_POLL_MS = 500;
const RACE_STREAM_STALE_MS = 15_000;
const CLOCK_RENDER_INTERVAL_MS = 50;
const MARKER_RENDER_INTERVAL_MS = 1000 / 30;
const TELEMETRY_RENDER_INTERVAL_MS = 100;
const CONTROL_STALE_MS = 250;
const TRANSPORT_RENDER_INTERVAL_MS = 250;
const TEAM_SELECTION_LIMIT = 4;
const LEADERBOARD_POSITION_CHANGE_HOLD_MS = 2200;
const TEAM_SELECTION_STORAGE_KEY = 'momoTeamObserverVehiclesV2';
const LEGACY_TEAM_SELECTION_STORAGE_KEY = 'momoTeamObserverCarsV1';
const PILOT_DEVICES_POLL_MS = 5000;
const DIRECTORY_POLL_MS = 30000;
const CAMERA_MOTION_SCALE_G = 1.5;
const CAMERA_RPM_SCALE = 50_000;
const CAMERA_SPEED_SCALE_KPH = 120;
const CAMERA_BATTERY_WARNING_V = 7.0;
const CAMERA_BATTERY_CRITICAL_V = 6.6;
const CAMERA_ESC_WARNING_C = 70;
const CAMERA_ESC_CRITICAL_C = 85;
const CAMERA_MOTOR_WARNING_C = 80;
const CAMERA_MOTOR_CRITICAL_C = 100;
const COURSE_SECTOR_BOUNDARIES = courseLayout.sectorBoundaries;
const CAR_EFFECTS = Object.freeze({
  'gravel': { label: 'GRAVEL', durationMs: 1100, priority: 20 },
  'impact': { label: 'IMPACT', durationMs: 1900, priority: 80 },
  'heavy-impact': { label: 'HEAVY IMPACT', durationMs: 2600, priority: 100 },
  'boost-ready': { label: 'BOOST READY', durationMs: 1800, priority: 45 },
  'boost-active': { label: 'BOOST ACTIVE', durationMs: 2400, priority: 65 },
  'fuel-low': { label: 'FUEL LOW', durationMs: 1800, priority: 55 },
  'fuel-empty': { label: 'FUEL EMPTY', durationMs: 2800, priority: 95 },
  'pit-service': { label: 'PIT SERVICE', durationMs: 1800, priority: 50 },
  'pit-complete': { label: 'SERVICE COMPLETE', durationMs: 2300, priority: 75 },
  'position-up': { label: 'POSITION GAIN', durationMs: 1800, priority: 35 },
  'position-down': { label: 'POSITION LOST', durationMs: 1800, priority: 35 },
  'personal-best': { label: 'PERSONAL BEST', durationMs: 2100, priority: 60 },
  'overall-best': { label: 'OVERALL BEST', durationMs: 2400, priority: 70 },
});
const raceDeduplicator = new RaceStateDeduplicator();
const currentLapClocks = new CurrentLapClockTracker();
const healthByCar = new Map();
const telemetryByCar = new Map();
const controlByCar = new Map();
const vehicleEventByCar = new Map();
const vehicleEventHistoryByCar = new Map();
const pitByCar = new Map();
const pendingPitByCar = new Map();
const connectionByCar = new Map();
const clientByCar = new Map();
const vehicleEventTimers = new Map();
const carEffectTimers = new Map();
const activeEffectByCar = new Map();
const leaderboardPositionChangeByCar = new Map();
const leaderboardPositionChangeTimers = new Map();
const markerMotionByCar = new Map();
const markerRenderByCar = new Map();
const markerNodesByCar = new Map();
const sectorLiveNodeByCar = new Map();
const sectorCompletionHoldByCar = new Map();
const sectorCompletionNodeByCar = new Map();
const sectorContentByCar = new Map();
const seenSectorAchievements = new Set();
const cameraTitleNodesByCar = new Map();
const cameraTileNodesByCar = new Map();
const cameraEffectNodesByCar = new Map();
const leaderboardNodesByCar = new Map();
const leaderboardContentByCar = new Map();
const telemetryNodesByCar = new Map();
const healthNodesByCar = new Map();
const controlNodesByCar = new Map();
const renderedTelemetryByCar = new Map();
const pitMotionByCar = new Map();
const trackPathLookupByElement = new WeakMap();
const overviewRenderSampler = raceUiPerformance.createDurationSampler();
const leaderboardRenderSampler = raceUiPerformance.createDurationSampler();
const sectorRenderSampler = raceUiPerformance.createDurationSampler();
const markerRenderSampler = raceUiPerformance.createDurationSampler();
const uiLongTaskTracker = raceUiPerformance.createLongTaskTracker();
let markerMetricLogCounter = 0;
let observerConfig = null;
let baseObserverConfig = null;
let teamDirectory = null;
let pilotDevices = null;
let raceState = null;
let standingByCar = new Map();
let lapPaceByCar = new Map();
let normalizedLapHistory = [];
let trackGeometry = null;
let raceReceivedAt = 0;
let raceTransport = null;
let raceStreamOpen = false;
let raceClient = null;
let animationFrame = 0;
let racePollTimer = 0;
let racePollInFlight = false;
let raceFallbackEnabled = false;
let raceStatusRenderedAt = 0;
let clockRenderedAt = 0;
let markerRenderedAt = 0;
let telemetryRenderedAt = 0;
let leaderboardSignature = '';
let sectorRowsSignature = '';
let timingRowsSignature = '';
let situationsSignature = '';
let teamSelectionSignature = '';
let selectedTeamVehicleIds = [];
let pendingTeamVehicleId = '';
let activeRelayHost = '';
let teamPeersEnabled = false;
let pilotDevicesPollTimer = 0;
let directoryPollTimer = 0;
let fleetPollInFlight = false;
let uiTestSnapshotTimer = 0;
let uiTestSnapshotTick = 0;
let uiTestMetricsWarmupTimer = 0;

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function applyCarAccent(node, car) {
  if (node && car?.color) node.style.setProperty('--car', car.color);
  return node;
}

function svgElement(tag, attributes = {}) {
  const node = document.createElementNS('http://www.w3.org/2000/svg', tag);
  for (const [name, value] of Object.entries(attributes)) node.setAttribute(name, String(value));
  return node;
}

function selectedTeamCars() {
  if (!observerConfig) return [];
	const byVehicle = new Map(observerConfig.cars.map((car) => [car.vehicleId, car]));
	return selectedTeamVehicleIds.map((vehicleId) => byVehicle.get(vehicleId)).filter(Boolean);
}

function isTeamCarSelected(car) {
	return Boolean(car && selectedTeamVehicleIds.includes(car.vehicleId));
}

function loadStoredTeamSelection() {
  try {
    const raw = localStorage.getItem(TEAM_SELECTION_STORAGE_KEY);
    if (raw === null) return null;
    const value = JSON.parse(raw);
		if (!value || value.version !== 2 || !Array.isArray(value.vehicleIds)) return null;
		if (teamDirectory && value.eventSlug !== teamDirectory.event.slug) return null;
		return value.vehicleIds;
  } catch (_) {
    return null;
  }
}

function loadLegacyStoredTeamSelection() {
	try {
		const value = JSON.parse(localStorage.getItem(LEGACY_TEAM_SELECTION_STORAGE_KEY));
		return Array.isArray(value) ? value : null;
	} catch (_) {
		return null;
	}
}

function initialTeamSelection(params) {
	if (params.has('teamVehicles')) {
		return normalizeTeamVehicleSelection(
			observerConfig.cars,
			params.get('teamVehicles') || 'none',
			TEAM_SELECTION_LIMIT,
		);
	}
  if (params.has('teamCars')) {
		return normalizeTeamVehicleSelection(
      observerConfig.cars,
      params.get('teamCars') || 'none',
      TEAM_SELECTION_LIMIT,
    );
  }
  if (params.has('videoDevices')) {
		return normalizeTeamVehicleSelection(
      observerConfig.cars,
      params.get('videoDevices') || 'none',
      TEAM_SELECTION_LIMIT,
    );
  }
  const stored = loadStoredTeamSelection();
	if (stored !== null) return normalizeTeamVehicleSelection(observerConfig.cars, stored, TEAM_SELECTION_LIMIT);
	const legacy = loadLegacyStoredTeamSelection();
	return legacy === null
		? normalizeTeamVehicleSelection(observerConfig.cars, 'all', TEAM_SELECTION_LIMIT)
		: normalizeTeamVehicleSelection(observerConfig.cars, legacy, TEAM_SELECTION_LIMIT);
}

function persistTeamSelection() {
  try {
		localStorage.setItem(TEAM_SELECTION_STORAGE_KEY, JSON.stringify({
			version: 2,
			eventSlug: teamDirectory?.event.slug || '',
			directoryRevision: teamDirectory?.directoryRevision || '',
			vehicleIds: selectedTeamVehicleIds,
		}));
		localStorage.removeItem(LEGACY_TEAM_SELECTION_STORAGE_KEY);
  } catch (_) {
    // URL state remains available when storage is disabled.
  }
  const url = new URL(location.href);
	url.searchParams.set('teamVehicles', selectedTeamVehicleIds.length > 0 ? selectedTeamVehicleIds.join(',') : 'none');
	url.searchParams.delete('teamCars');
	url.searchParams.delete('videoDevices');
  history.replaceState(null, '', url);
}

function openTeamSelector() {
  const backdrop = document.getElementById('teamSelectorBackdrop');
  if (!backdrop) return;
  backdrop.hidden = false;
  renderTeamSelectionControls();
  document.getElementById('teamSelectorClose')?.focus();
}

function closeTeamSelector() {
  const backdrop = document.getElementById('teamSelectorBackdrop');
  if (!backdrop) return;
  backdrop.hidden = true;
	pendingTeamVehicleId = '';
  renderTeamSelectionControls();
  document.getElementById('teamSelectorOpen')?.focus();
}

function focusTeamCar(car) {
  const tile = cameraTileNodesByCar.get(car?.carId);
  if (!tile || !tile.isConnected) return;
  tile.classList.remove('is-focused');
  void tile.offsetWidth;
  tile.classList.add('is-focused');
  tile.scrollIntoView({ behavior: 'smooth', block: 'nearest', inline: 'nearest' });
  window.setTimeout(() => tile.classList.remove('is-focused'), 900);
}

function syncTeamSelectionDecorations() {
  for (const car of observerConfig?.cars || []) {
    const selected = isTeamCarSelected(car);
    const leader = leaderboardNodesByCar.get(car.carId);
    leader?.classList.toggle('is-team-selected', selected);
    leader?.setAttribute('aria-pressed', selected ? 'true' : 'false');
    const markerNodes = markerNodesByCar.get(car.carId);
    const marker = markerNodes?.marker;
    marker?.classList.toggle('is-team-selected', selected);
    marker?.setAttribute('aria-pressed', selected ? 'true' : 'false');
    updateTrackMarkerDensity(markerNodes, selected, observerConfig.cars.length);
  }
}

function renderTeamReplacementPanel() {
  const panel = document.getElementById('teamReplacementPanel');
  if (!panel) return;
	const candidate = observerConfig?.cars.find((car) => car.vehicleId === pendingTeamVehicleId);
  panel.hidden = !candidate;
  panel.replaceChildren();
  if (!candidate) return;
  panel.append(element('strong', '', `REPLACE WITH CAR ${candidate.displayNumber}`));
  const choices = element('div', 'team-replacement-choices');
  selectedTeamCars().forEach((car, index) => {
    const button = applyCarAccent(
      element('button', 'car-accent', `SLOT ${index + 1} / CAR ${car.displayNumber}`),
      car,
    );
    button.type = 'button';
    button.addEventListener('click', () => {
			const next = [...selectedTeamVehicleIds];
			next[index] = candidate.vehicleId;
			pendingTeamVehicleId = '';
      setTeamSelection(next, true);
    });
    choices.append(button);
  });
  const cancel = element('button', 'team-replacement-cancel', 'CANCEL');
  cancel.type = 'button';
  cancel.addEventListener('click', () => {
		pendingTeamVehicleId = '';
    renderTeamSelectionControls();
  });
  choices.append(cancel);
  panel.append(choices);
}

function renderTeamSelectionControls() {
  if (!observerConfig) return;
  const orderedCars = standingsByConfiguredCar(observerConfig.cars, raceState);
  const signature = JSON.stringify({
    cars: orderedCars.map(({ car, standing }) => [
      car.vehicleId, car.carId, car.displayNumber, car.color, carName(car, standing), car.device,
      car.directoryStatus, car.availability, car.sourceBound, standing?.status || '',
      connectionByCar.get(car.carId)?.state || '', isTeamCarSelected(car),
    ]),
    selected: selectedTeamVehicleIds,
    pending: pendingTeamVehicleId,
    directoryStale: Boolean(teamDirectory?.stale),
  });
  if (signature === teamSelectionSignature) return;
  teamSelectionSignature = signature;
  const slots = document.getElementById('teamSelectionSlots');
  if (slots) {
    const cars = selectedTeamCars();
    slots.replaceChildren(...Array.from({ length: TEAM_SELECTION_LIMIT }, (_, index) => {
      const car = cars[index];
      const slot = element('div', `team-selection-slot${car ? ' is-filled' : ''}`);
      if (!car) {
        const empty = element('button', 'team-slot-main', `SLOT ${index + 1}`);
        empty.type = 'button';
        empty.setAttribute('aria-label', `Select a car for slot ${index + 1}`);
        empty.addEventListener('click', openTeamSelector);
        slot.append(empty);
        return slot;
      }
      applyCarAccent(slot, car);
      const standing = standingByCar.get(car.carId);
      const main = element('button', 'team-slot-main');
      main.type = 'button';
      main.append(
        element('strong', '', `#${car.displayNumber}`),
        element('span', '', carName(car, standing)),
      );
      main.addEventListener('click', () => focusTeamCar(car));
      const remove = element('button', 'team-slot-remove', '×');
      remove.type = 'button';
      remove.setAttribute('aria-label', `Remove CAR ${car.displayNumber} from team monitor`);
      remove.addEventListener('click', () => {
				setTeamSelection(selectedTeamVehicleIds.filter((vehicleId) => vehicleId !== car.vehicleId), true);
      });
      slot.append(main, remove);
      return slot;
    }));
  }

  const list = document.getElementById('teamSelectorList');
  if (list) {
    list.replaceChildren(...orderedCars.map(({ car, standing }) => {
      const selected = isTeamCarSelected(car);
      const row = applyCarAccent(
        element('button', `team-selector-row${selected ? ' is-selected' : ''}`),
        car,
      );
      row.type = 'button';
      row.setAttribute('aria-pressed', selected ? 'true' : 'false');
      const identity = element('span', 'team-selector-identity');
      identity.append(element('strong', '', `#${car.displayNumber}`), element('span', '', carName(car, standing)));
      const connection = connectionByCar.get(car.carId);
      row.append(
        identity,
			element('span', 'team-selector-device', car.device || 'SOURCE UNBOUND'),
			element('em', '', selected ? connection?.state || 'SELECTED'
				: car.directoryStatus === 'maintenance' ? 'MAINTENANCE'
					: standing ? 'RACING' : String(car.availability || 'AVAILABLE').toUpperCase()),
      );
      row.addEventListener('click', () => {
        if (selected) {
					setTeamSelection(selectedTeamVehicleIds.filter((vehicleId) => vehicleId !== car.vehicleId), true);
          return;
        }
        requestTeamCar(car);
      });
      return row;
    }));
  }
  setTextIfChanged(
    document.getElementById('teamSelectionCount'),
		`${selectedTeamVehicleIds.length} / ${TEAM_SELECTION_LIMIT} SELECTED${teamDirectory?.stale ? ' · DIRECTORY STALE' : ''}`,
  );
  renderTeamReplacementPanel();
  syncTeamSelectionDecorations();
}

function requestTeamCar(car) {
  if (!car) return;
  if (isTeamCarSelected(car)) {
    focusTeamCar(car);
    return;
  }
	if (selectedTeamVehicleIds.length < TEAM_SELECTION_LIMIT) {
		setTeamSelection([...selectedTeamVehicleIds, car.vehicleId], true);
    return;
  }
	pendingTeamVehicleId = car.vehicleId;
  openTeamSelector();
}

function syncSelectedTeamPeers() {
  const selectedCarIds = new Set(selectedTeamCars().map((car) => car.carId));
  for (const car of observerConfig.cars) {
    if (selectedCarIds.has(car.carId)) continue;
    const client = clientByCar.get(car.carId);
    client?.close();
    clientByCar.delete(car.carId);
    healthByCar.delete(car.carId);
    telemetryByCar.delete(car.carId);
    controlByCar.delete(car.carId);
    vehicleEventByCar.delete(car.carId);
    vehicleEventHistoryByCar.delete(car.carId);
    pitByCar.delete(car.carId);
    renderedTelemetryByCar.delete(car.carId);
    if (!connectionByCar.get(car.carId)?.subscriptionDisabled) {
      updateCameraState(car, {
        state: 'DISABLED', detail: 'NOT SELECTED', fps: 0, videoActive: false,
        dataOpen: false, subscriptionDisabled: true,
      });
    }
  }
  createCameraTiles();
  for (const car of selectedTeamCars()) {
    const connection = connectionByCar.get(car.carId);
    if (!connection || connection.subscriptionDisabled) {
      updateCameraState(car, {
				state: car.sourceBound ? 'WAITING' : 'UNAVAILABLE',
				detail: car.sourceBound ? (teamPeersEnabled ? 'CONNECTING TO RELAY' : 'UI TEST') : 'SOURCE UNBOUND',
        fps: 0, videoActive: false, dataOpen: false, subscriptionDisabled: false,
      });
    }
  }
  if (!teamPeersEnabled) return;
  for (const car of selectedTeamCars()) {
		if (clientByCar.has(car.carId) || !car.sourceBound || !car.device) continue;
    const video = cameraTileNodesByCar.get(car.carId)?.querySelector('video');
    if (!video) continue;
    const client = new ObserverPeer(
      car,
      activeRelayHost,
      video,
      updateCameraState,
      handleHealth,
      handlePit,
      handleTelemetry,
      handleControl,
      handleVehicleEvent,
    );
    clientByCar.set(car.carId, client);
    client.connect();
  }
}

function setTeamSelection(value, persist = false) {
	const next = normalizeTeamVehicleSelection(observerConfig?.cars || [], value, TEAM_SELECTION_LIMIT);
	const changed = next.length !== selectedTeamVehicleIds.length
		|| next.some((vehicleId, index) => vehicleId !== selectedTeamVehicleIds[index]);
	selectedTeamVehicleIds = next;
	pendingTeamVehicleId = '';
  if (changed) {
    syncSelectedTeamPeers();
    for (const car of selectedTeamCars()) renderedTelemetryByCar.delete(car.carId);
    renderCameraTitles();
    renderCameraHealthDisplays();
    renderTelemetryDisplays();
    renderControlDisplays(performance.now());
    leaderboardSignature = '';
    sectorRowsSignature = '';
    renderLeaderboard();
    renderSectorRows();
    renderHeader();
    renderSituations();
  }
  renderTeamSelectionControls();
  if (persist) persistTeamSelection();
}

function applyCarEffect(carId) {
  const effect = activeEffectByCar.get(carId);
  const name = effect?.name || '';
  for (const node of [
    cameraEffectNodesByCar.get(carId)?.root,
    leaderboardNodesByCar.get(carId),
    markerNodesByCar.get(carId)?.marker,
  ]) {
    if (!node) continue;
    if (name) node.dataset.effect = name;
    else delete node.dataset.effect;
  }
  const badge = cameraEffectNodesByCar.get(carId)?.badge;
  if (badge) {
    badge.hidden = !effect;
    setTextIfChanged(badge, effect?.label || '');
  }
}

function triggerCarEffect(carId, name) {
  const definition = CAR_EFFECTS[name];
  if (!definition) return;
  const now = performance.now();
  const current = activeEffectByCar.get(carId);
  if (current && current.expiresAt > now && current.priority > definition.priority) return;
  const token = `${name}:${now}`;
  activeEffectByCar.set(carId, { name, token, expiresAt: now + definition.durationMs, ...definition });
  applyCarEffect(carId);
  if (carEffectTimers.has(carId)) window.clearTimeout(carEffectTimers.get(carId));
  carEffectTimers.set(carId, window.setTimeout(() => {
    carEffectTimers.delete(carId);
    if (activeEffectByCar.get(carId)?.token !== token) return;
    activeEffectByCar.delete(carId);
    applyCarEffect(carId);
  }, definition.durationMs));
}

function carName(car, standing = null) {
  return standing?.driver || car.driver || car.vehicleName || `CAR ${car.displayNumber}`;
}

function createWebSocketUrl(relayHost, device) {
  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const url = new URL(`${protocol}//${relayHost}/ws`);
  url.searchParams.set('role', 'observer');
  url.searchParams.set('client', 'web-observer');
  url.searchParams.set('device', device);
  return url.toString();
}

function createRaceStateUrl(relayHost) {
  return new URL(`/api/v1/race-state`, `${location.protocol}//${relayHost}`).toString();
}

function createRaceStateWebSocketUrl(relayHost) {
  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  const url = new URL(`${protocol}//${relayHost}/ws/race-state`);
  url.searchParams.set('client', 'web-observer');
  return url.toString();
}

function preferH264(transceiver) {
  if (!transceiver || typeof transceiver.setCodecPreferences !== 'function'
      || typeof RTCRtpSender === 'undefined' || !RTCRtpSender.getCapabilities) return;
  const codecs = RTCRtpSender.getCapabilities('video')?.codecs || [];
  const primary = codecs.filter((codec) => {
    const mime = String(codec.mimeType || '').toLowerCase();
    return mime.startsWith('video/') && !/(\/rtx|\/red|\/ulpfec|\/flexfec)/.test(mime);
  });
  const selected = primary.filter((codec) => String(codec.mimeType || '').toLowerCase() === 'video/h264');
  if (selected.length > 0) {
    transceiver.setCodecPreferences(selected.concat(primary.filter((codec) => !selected.includes(codec))));
  }
}

class RaceStateStream {
  constructor(relayHost, onRace, onState) {
    this.relayHost = relayHost;
    this.onRace = onRace;
    this.onState = onState;
    this.ws = null;
    this.reconnectTimer = 0;
    this.reconnectAttempt = 0;
    this.closed = false;
    this.generation = 0;
    this.activityTimer = 0;
    this.live = false;
  }

  connect() {
    if (this.closed) return;
    this.closeTransport();
    const generation = ++this.generation;
    const ws = new WebSocket(createRaceStateWebSocketUrl(this.relayHost));
    this.ws = ws;
    ws.onopen = () => {
      if (generation !== this.generation) return;
      this.reconnectAttempt = 0;
      this.markActivity(generation);
    };
    ws.onmessage = (event) => {
      if (generation !== this.generation) return;
      let message;
      try {
        message = JSON.parse(event.data);
      } catch (_) {
        return;
      }
      if (message.type === 'race-heartbeat') {
        this.markActivity(generation);
        return;
      }
      if (message.type !== 'race-state' || typeof message.data !== 'string') return;
      this.markActivity(generation);
      const state = parseRaceState(message.data);
      if (state) this.onRace(state, 'websocket');
    };
    ws.onerror = () => this.scheduleReconnect(generation);
    ws.onclose = () => this.scheduleReconnect(generation);
  }

  scheduleReconnect(generation) {
    if (generation !== this.generation || this.closed || this.reconnectTimer) return;
    this.setLive(false);
    this.closeTransport();
    const delay = Math.min(RECONNECT_BASE_MS * (2 ** Math.min(this.reconnectAttempt, 4)), RECONNECT_MAX_MS);
    this.reconnectAttempt += 1;
    this.reconnectTimer = window.setTimeout(() => {
      this.reconnectTimer = 0;
      this.connect();
    }, delay);
  }

  closeTransport() {
    if (this.activityTimer) window.clearTimeout(this.activityTimer);
    this.activityTimer = 0;
    if (!this.ws) return;
    this.ws.onopen = null;
    this.ws.onmessage = null;
    this.ws.onerror = null;
    this.ws.onclose = null;
    if (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING) this.ws.close();
    this.ws = null;
  }

  markActivity(generation) {
    if (generation !== this.generation || this.closed) return;
    if (this.activityTimer) window.clearTimeout(this.activityTimer);
    this.activityTimer = window.setTimeout(() => this.scheduleReconnect(generation), RACE_STREAM_STALE_MS);
    this.setLive(true);
  }

  setLive(live) {
    if (this.live === live) return;
    this.live = live;
    this.onState(live);
  }

  close() {
    this.closed = true;
    if (this.reconnectTimer) window.clearTimeout(this.reconnectTimer);
    this.reconnectTimer = 0;
    this.setLive(false);
    this.closeTransport();
  }
}

class ObserverPeer {
  constructor(car, relayHost, video, onState, onHealth, onPit, onTelemetry, onControl, onVehicleEvent) {
    this.car = car;
    this.relayHost = relayHost;
    this.video = video;
    this.onState = onState;
    this.onHealth = onHealth;
    this.onPit = onPit;
    this.onTelemetry = onTelemetry;
    this.onControl = onControl;
    this.onVehicleEvent = onVehicleEvent;
    this.ws = null;
    this.pc = null;
    this.pendingCandidates = [];
    this.remoteDescriptionSet = false;
    this.reconnectTimer = 0;
    this.disconnectedTimer = 0;
    this.reconnectAttempt = 0;
    this.closed = false;
    this.generation = 0;
    this.frameCount = 0;
    this.frameWindowStartedAt = performance.now();
    this.fps = 0;
    this.videoActive = false;
    this.dataOpen = false;
    this.telemetryTracker = window.FpvTelemetry
      ? new window.FpvTelemetry.TelemetryTracker()
      : null;
    this.motionFeatures = window.FpvTelemetry
      ? new window.FpvTelemetry.MotionFeatureExtractor()
      : null;
    this.latestMotion = null;
    this.vehicleEvents = window.FpvTelemetry
      ? new window.FpvTelemetry.RelayEventInbox()
      : null;
  }

  connect() {
    if (this.closed) return;
    this.closeTransport();
    const generation = ++this.generation;
    this.setState('CONNECTING', 'SIGNALING');
    const ws = new WebSocket(createWebSocketUrl(this.relayHost, this.car.device));
    this.ws = ws;
    ws.onopen = () => {
      if (generation !== this.generation) return;
      this.dataOpen = true;
      this.setState('CONNECTING', 'DATA WS OPEN');
      this.makeOffer(generation);
    };
    ws.onmessage = (event) => this.handleSignal(generation, event.data);
    ws.onerror = () => this.setState('CONNECTING', 'SIGNALING ERROR');
    ws.onclose = () => this.scheduleReconnect(generation, 'SIGNALING CLOSED');
  }

  handleTelemetryMessage(message) {
    if (typeof message !== 'string') return;
    const health = parseVehicleHealth(message);
    if (health) this.onHealth(this.car, health);
    const gameplay = parseVehicleGameplay(message);
    if (gameplay) this.onHealth(this.car, gameplay);
    const pit = parsePitPresence(message);
    if (pit) this.onPit(this.car, pit);
    if (!this.telemetryTracker || !message.startsWith('TEL:')) return;
    const arrivalMs = performance.now();
    const result = this.telemetryTracker.ingest(message, arrivalMs);
    if (!result.accepted) return;
    const nextMotion = this.motionFeatures?.ingest(result.payload, arrivalMs) || null;
    if (nextMotion) this.latestMotion = nextMotion;
    const snapshot = this.telemetryTracker.getSnapshot(arrivalMs);
    this.onTelemetry(this.car, {
      motion: this.latestMotion,
      primary: snapshot.primary,
      esc: snapshot.primaryEsc,
      counters: snapshot.counters,
    });
  }

  handleCommandMessage(message) {
    if (typeof message !== 'string') return;
    const control = parseControlCommand(message);
    if (control) this.onControl(this.car, control);
  }

  handleVehicleEventMessage(message) {
    if (typeof message !== 'string' || !this.vehicleEvents) return;
    const result = this.vehicleEvents.ingest(message);
    if ((result.event && result.event.carId !== this.car.carId)
        || result.events?.some((item) => item.carId !== this.car.carId)) return;
    if (result.status === 'snapshot' || result.status === 'live') {
      this.onVehicleEvent(this.car, result);
    }
  }

  createPeer(generation) {
    const pc = new RTCPeerConnection({ iceServers: [] });
    this.pc = pc;
    this.pendingCandidates = [];
    this.remoteDescriptionSet = false;

    const stream = new MediaStream();
    this.video.srcObject = stream;
    pc.ontrack = (event) => {
      if (generation !== this.generation) return;
      stream.addTrack(event.track);
      this.video.play().catch(() => {});
      this.setState('CONNECTED', 'VIDEO TRACK RECEIVED');
      this.monitorFrames(generation);
    };
    pc.onicecandidate = (event) => {
      if (!event.candidate || generation !== this.generation) return;
      this.sendSignal({ type: 'candidate', ice: event.candidate });
    };
    pc.onconnectionstatechange = () => {
      if (generation !== this.generation) return;
      if (pc.connectionState === 'connected') {
        if (this.disconnectedTimer) window.clearTimeout(this.disconnectedTimer);
        this.disconnectedTimer = 0;
        this.reconnectAttempt = 0;
        this.setState(this.videoActive ? 'STREAMING' : 'CONNECTED', this.videoActive ? 'VIDEO ACTIVE' : 'PEER CONNECTED');
      } else if (pc.connectionState === 'disconnected') {
        this.setState('RECONNECTING', 'PEER DISCONNECTED');
        if (!this.disconnectedTimer) {
          this.disconnectedTimer = window.setTimeout(() => {
            this.disconnectedTimer = 0;
            if (generation === this.generation && this.pc === pc
                && pc.connectionState === 'disconnected') {
              this.scheduleReconnect(generation, 'PEER DISCONNECTED');
            }
          }, DISCONNECTED_RECONNECT_MS);
        }
      } else if (pc.connectionState === 'failed' || pc.connectionState === 'closed') {
        this.scheduleReconnect(generation, `PEER ${pc.connectionState.toUpperCase()}`);
      }
    };
    const videoTransceiver = pc.addTransceiver('video', { direction: 'recvonly' });
    preferH264(videoTransceiver);
    return pc;
  }

  async makeOffer(generation) {
    if (generation !== this.generation || this.closed) return;
    try {
      const pc = this.createPeer(generation);
      const offer = await pc.createOffer();
      await pc.setLocalDescription(offer);
      this.sendSignal({ type: pc.localDescription.type, sdp: pc.localDescription.sdp });
    } catch (error) {
      this.scheduleReconnect(generation, error?.message || 'OFFER FAILED');
    }
  }

  async handleSignal(generation, raw) {
    if (generation !== this.generation || !this.pc) return;
    let message;
    try {
      message = JSON.parse(raw);
    } catch (_) {
      return;
    }
    try {
      if (message.type === 'answer') {
        await this.pc.setRemoteDescription(message);
        this.remoteDescriptionSet = true;
        for (const candidate of this.pendingCandidates) {
          await this.pc.addIceCandidate(candidate);
        }
        this.pendingCandidates = [];
      } else if (message.type === 'candidate' && message.ice) {
        const candidate = new RTCIceCandidate(message.ice);
        if (this.remoteDescriptionSet) await this.pc.addIceCandidate(candidate);
        else this.pendingCandidates.push(candidate);
      } else if (message.type === 'telemetry') {
        this.handleTelemetryMessage(message.data);
      } else if (message.type === 'command') {
        this.handleCommandMessage(message.data);
      } else if (message.type === 'vehicle-event') {
        this.handleVehicleEventMessage(message.data);
      } else if (message.type === 'close' || message.type === 'error') {
        this.scheduleReconnect(generation, String(message.error || message.type).toUpperCase());
      }
    } catch (error) {
      this.scheduleReconnect(generation, error?.message || 'SIGNALING FAILED');
    }
  }

  sendSignal(message) {
    if (this.ws?.readyState === WebSocket.OPEN) this.ws.send(JSON.stringify(message));
  }

  monitorFrames(generation) {
    if (typeof this.video.requestVideoFrameCallback !== 'function') {
      this.video.onplaying = () => {
        if (generation !== this.generation || !this.video.videoWidth) return;
        this.videoActive = true;
        this.setState('STREAMING', 'VIDEO ACTIVE');
      };
      return;
    }
    const count = () => {
      if (generation !== this.generation || this.closed) return;
      const now = performance.now();
      this.frameCount += 1;
      if (!this.videoActive) {
        this.videoActive = true;
        this.setState('STREAMING', 'VIDEO ACTIVE');
      }
      const elapsed = now - this.frameWindowStartedAt;
      if (elapsed >= 1000) {
        this.fps = this.frameCount * 1000 / elapsed;
        this.frameCount = 0;
        this.frameWindowStartedAt = now;
        this.setState('STREAMING', 'VIDEO ACTIVE');
      }
      this.video.requestVideoFrameCallback(count);
    };
    this.frameCount = 0;
    this.frameWindowStartedAt = performance.now();
    this.video.requestVideoFrameCallback(count);
  }

  setState(state, detail) {
    this.onState(this.car, {
      state,
      detail,
      fps: this.fps,
      videoActive: this.videoActive,
      dataOpen: this.dataOpen,
    });
  }

  scheduleReconnect(generation, detail) {
    if (generation !== this.generation || this.closed || this.reconnectTimer) return;
    this.setState('RECONNECTING', detail);
    this.closeTransport();
    const delay = Math.min(RECONNECT_BASE_MS * (2 ** Math.min(this.reconnectAttempt, 4)), RECONNECT_MAX_MS);
    this.reconnectAttempt += 1;
    this.reconnectTimer = window.setTimeout(() => {
      this.reconnectTimer = 0;
      this.connect();
    }, delay);
  }

  closeTransport() {
    if (this.disconnectedTimer) window.clearTimeout(this.disconnectedTimer);
    this.disconnectedTimer = 0;
    this.videoActive = false;
    this.dataOpen = false;
    this.fps = 0;
    this.video.onplaying = null;
    if (this.ws) {
      this.ws.onopen = null;
      this.ws.onmessage = null;
      this.ws.onerror = null;
      this.ws.onclose = null;
      if (this.ws.readyState === WebSocket.OPEN) this.ws.close();
      this.ws = null;
    }
    if (this.pc) {
      this.pc.ontrack = null;
      this.pc.onicecandidate = null;
      this.pc.onconnectionstatechange = null;
      this.pc.close();
      this.pc = null;
    }
    if (this.video.srcObject) {
      for (const track of this.video.srcObject.getTracks()) track.stop();
      this.video.srcObject = null;
    }
  }

  close() {
    this.closed = true;
    if (this.reconnectTimer) window.clearTimeout(this.reconnectTimer);
    this.reconnectTimer = 0;
    this.closeTransport();
  }
}

function createLeaderboardHealth() {
  const root = element('div', 'leader-health');
  const track = element('i', 'leader-health-track');
  const fill = element('b', 'leader-health-fill');
  const value = element('strong', '', '--');
  track.append(fill);
  root.append(element('span', '', 'HP'), track, value);
  return { root, fill, value };
}

function createLeaderboardRow(car) {
  const row = element('article', 'leader-row');
  const nodes = {
    car,
    row,
    position: element('div', 'leader-pos'),
    positionValue: element('strong'),
    positionChange: element('span', 'leader-position-change'),
    avatar: element('span', 'leader-avatar'),
    avatarKey: '',
    driverName: element('strong'),
    carNumber: element('span', 'leader-car'),
    gap: element('div', 'leader-gap'),
    last: element('strong'),
    best: element('strong'),
    health: createLeaderboardHealth(),
  };
  row.tabIndex = 0;
  row.setAttribute('role', 'button');
  row.addEventListener('click', () => requestTeamCar(nodes.car));
  row.addEventListener('keydown', (event) => {
    if (event.key !== 'Enter' && event.key !== ' ') return;
    event.preventDefault();
    requestTeamCar(nodes.car);
  });
  const positionLine = element('span', 'leader-pos-line');
  positionLine.append(nodes.positionValue, nodes.positionChange);
  nodes.position.append(positionLine, nodes.carNumber);
  const driver = element('div', 'leader-driver');
  driver.append(nodes.driverName);
  const times = element('div', 'leader-times');
  const last = element('span', 'leader-time-last', 'LAST ');
  const best = element('span', 'leader-time-best', 'BEST ');
  last.append(nodes.last);
  best.append(nodes.best);
  times.append(last, best);
  row.append(nodes.position, nodes.avatar, driver, nodes.gap, times, nodes.health.root);
  return nodes;
}

function updateLeaderboardAvatar(nodes, car, standing) {
  const name = carName(car, standing);
  const key = car.portraitUrl ? `image:${car.portraitUrl}:${name}` : `text:${car.initials}:${name}`;
  if (key === nodes.avatarKey) return;
  nodes.avatarKey = key;
  if (car.portraitUrl) {
    const image = document.createElement('img');
    image.src = car.portraitUrl;
    image.alt = `${name} portrait`;
    nodes.avatar.removeAttribute('aria-label');
    nodes.avatar.replaceChildren(image);
  } else {
    nodes.avatar.textContent = car.initials;
    nodes.avatar.setAttribute('aria-label', `${name} portrait placeholder`);
  }
}

function updateLeaderboardHealth(nodes, value, state) {
  const level = Number.isFinite(value) ? Math.max(0, Math.min(100, value)) : 0;
  setDatasetIfChanged(nodes.root, 'state', state || 'unknown');
  if (nodes.fill.style.width !== `${level}%`) nodes.fill.style.width = `${level}%`;
  setTextIfChanged(nodes.value, Number.isFinite(value) ? String(Math.round(value)) : '--');
}

function updateLeaderboardRow(nodes, car, standing) {
  const health = healthByCar.get(car.carId);
  const status = ['racing', 'finished', 'retired'].includes(standing?.status)
    ? standing.status : 'waiting';
  const selected = isTeamCarSelected(car);
  const name = carName(car, standing);
  const className = `leader-row is-${status}${standing?.position === 1 ? ' is-leader' : ''}${selected ? ' is-team-selected' : ''}`;
  nodes.car = car;
  if (nodes.row.className !== className) nodes.row.className = className;
  applyCarAccent(nodes.row, car);
  const pressed = selected ? 'true' : 'false';
  if (nodes.row.getAttribute('aria-pressed') !== pressed) nodes.row.setAttribute('aria-pressed', pressed);
  const ariaLabel = `Monitor CAR ${car.displayNumber} ${name}`;
  if (nodes.row.getAttribute('aria-label') !== ariaLabel) nodes.row.setAttribute('aria-label', ariaLabel);
  setTextIfChanged(nodes.positionValue, standing?.position ? `P${standing.position}` : 'P--');
  const positionChange = leaderboardPositionChangeByCar.get(car.carId);
  nodes.positionChange.hidden = !positionChange;
  if (positionChange) {
    const className = `leader-position-change is-${positionChange.direction}`;
    if (nodes.positionChange.className !== className) nodes.positionChange.className = className;
    setTextIfChanged(nodes.positionChange, String(positionChange.places));
    const label = `${positionChange.direction === 'up' ? 'Up' : 'Down'} ${positionChange.places} position${positionChange.places === 1 ? '' : 's'}`;
    if (nodes.positionChange.getAttribute('aria-label') !== label) nodes.positionChange.setAttribute('aria-label', label);
    if (nodes.positionChange.title !== label) nodes.positionChange.title = label;
  } else {
    setTextIfChanged(nodes.positionChange, '');
    nodes.positionChange.removeAttribute('aria-label');
    nodes.positionChange.removeAttribute('title');
  }
  updateLeaderboardAvatar(nodes, car, standing);
  setTextIfChanged(nodes.driverName, name);
  setTextIfChanged(nodes.carNumber, `#${car.displayNumber}`);
  setTextIfChanged(nodes.gap, formatStandingGap(standing));
  setTextIfChanged(nodes.last, formatSplitTime(standing?.lapTimeMs));
  setTextIfChanged(nodes.best, formatSplitTime(standing?.bestLapMs));
  const hp = Number.isFinite(health?.hp) ? Math.round(health.hp) : null;
  updateLeaderboardHealth(nodes.health, hp, hp === null ? 'unknown' : health.mode);
}

function setLeaderboardPositionChange(carId, previousPosition, nextPosition) {
  const change = leaderboardPositionChange(previousPosition, nextPosition);
  if (!change) return;
  const token = `${carId}:${previousPosition}:${nextPosition}:${performance.now()}`;
  leaderboardPositionChangeByCar.set(carId, { ...change, token });
  if (leaderboardPositionChangeTimers.has(carId)) {
    window.clearTimeout(leaderboardPositionChangeTimers.get(carId));
  }
  leaderboardPositionChangeTimers.set(carId, window.setTimeout(() => {
    leaderboardPositionChangeTimers.delete(carId);
    if (leaderboardPositionChangeByCar.get(carId)?.token !== token) return;
    leaderboardPositionChangeByCar.delete(carId);
    renderLeaderboard();
  }, LEADERBOARD_POSITION_CHANGE_HOLD_MS));
}

function clearLeaderboardPositionChanges() {
  for (const timer of leaderboardPositionChangeTimers.values()) window.clearTimeout(timer);
  leaderboardPositionChangeTimers.clear();
  leaderboardPositionChangeByCar.clear();
}

function captureLeaderboardRowPositions() {
  const positions = new Map();
  for (const [carId, nodes] of leaderboardContentByCar) {
    if (!nodes.row.isConnected) continue;
    positions.set(carId, nodes.row.getBoundingClientRect().top);
    nodes.reorderAnimation?.cancel();
    nodes.reorderAnimation = null;
    nodes.row.style.zIndex = '';
  }
  return positions;
}

function animateLeaderboardReorder(previousPositions) {
  if (window.matchMedia?.('(prefers-reduced-motion: reduce)').matches) return;
  for (const [carId, previousTop] of previousPositions) {
    const nodes = leaderboardContentByCar.get(carId);
    if (!nodes?.row.isConnected) continue;
    const deltaY = previousTop - nodes.row.getBoundingClientRect().top;
    if (Math.abs(deltaY) < 1) continue;
    nodes.row.style.zIndex = '2';
    const animation = nodes.row.animate([
      { transform: `translateY(${deltaY}px)` },
      { transform: 'translateY(0)' },
    ], {
      duration: 420,
      easing: 'cubic-bezier(.2, .8, .2, 1)',
    });
    nodes.reorderAnimation = animation;
    animation.addEventListener('finish', () => {
      if (nodes.reorderAnimation !== animation) return;
      nodes.reorderAnimation = null;
      nodes.row.style.zIndex = '';
    }, { once: true });
    animation.addEventListener('cancel', () => {
      if (nodes.reorderAnimation !== animation) return;
      nodes.reorderAnimation = null;
      nodes.row.style.zIndex = '';
    }, { once: true });
  }
}

function renderLeaderboard() {
  const root = document.getElementById('leaderboardRows');
  const rows = standingsByConfiguredCar(raceParticipantCars(observerConfig.cars, raceState), raceState);
  const signature = JSON.stringify(rows.map(({ car, standing }) => {
    const health = healthByCar.get(car.carId);
    return {
      carId: car.carId,
      standing: standing ? {
        position: standing.position, status: standing.status, lapTimeMs: standing.lapTimeMs,
        bestLapMs: standing.bestLapMs, driver: standing.driver,
        intervalToAheadMs: standing.intervalToAheadMs,
        lapDeltaToAhead: standing.lapDeltaToAhead,
      } : null,
      health: health ? { hp: health.hp, mode: health.mode } : null,
      selected: isTeamCarSelected(car),
      positionChange: leaderboardPositionChangeByCar.get(car.carId) || null,
    };
  }));
  if (signature === leaderboardSignature) return;
  const startedAt = UI_METRICS_ENABLED ? performance.now() : 0;
  const previousPositions = captureLeaderboardRowPositions();
  leaderboardSignature = signature;
  let previousScrollTop = null;
  const preserveScroll = () => {
    if (previousScrollTop === null) previousScrollTop = root.scrollTop;
  };
  const activeCarIds = new Set();
  rows.forEach(({ car, standing }, index) => {
    activeCarIds.add(car.carId);
    let nodes = leaderboardContentByCar.get(car.carId);
    if (!nodes) {
      nodes = createLeaderboardRow(car);
      leaderboardContentByCar.set(car.carId, nodes);
      leaderboardNodesByCar.set(car.carId, nodes.row);
      applyCarEffect(car.carId);
    }
    updateLeaderboardRow(nodes, car, standing);
    const currentRow = root.children[index] || null;
    if (currentRow !== nodes.row) {
      preserveScroll();
      root.insertBefore(nodes.row, currentRow);
    }
  });
  for (const [carId, nodes] of leaderboardContentByCar) {
    if (activeCarIds.has(carId)) continue;
    preserveScroll();
    nodes.row.remove();
    leaderboardContentByCar.delete(carId);
    leaderboardNodesByCar.delete(carId);
  }
  if (previousScrollTop !== null) root.scrollTop = previousScrollTop;
  animateLeaderboardReorder(previousPositions);
  if (UI_METRICS_ENABLED) leaderboardRenderSampler.record(performance.now() - startedAt);
}

function createSectorRow(car) {
  const row = applyCarAccent(element('div', 'sector-row'), car);
  const carNumber = element('span', 'sector-car', `#${car.displayNumber}`);
  const barsRoot = element('div', 'sector-bars');
  const bars = [1, 2, 3].map((sector) => element('i', '', `S${sector}`));
  barsRoot.append(...bars);
  const current = element('span', 'sector-value sector-live timing-cell', '--.---');
  const last = element('span', 'sector-value sector-last timing-cell', '--.---');
  const best = element('span', 'sector-value sector-best timing-cell', '--.---');
  row.append(carNumber, barsRoot, current, last, best);
  return { row, carNumber, bars, current, last, best };
}

function updateSectorRow(nodes, car, standing, personalSectorBest, overallSectorBest, now) {
  const currentSector = Number.isInteger(standing?.currentSector) ? standing.currentSector : null;
  const active = currentSector !== null && standing?.status === 'racing';
  const rowClass = `sector-row${active ? ' is-active' : ''}`;
  if (nodes.row.className !== rowClass) nodes.row.className = rowClass;
  applyCarAccent(nodes.row, car);
  setTextIfChanged(nodes.carNumber, `#${car.displayNumber}`);
  const sectorCount = Number.isInteger(standing?.sectorCount) ? Math.min(3, standing.sectorCount) : 3;
  const sectorByNumber = new Map((standing?.sectorTimes || []).map((timing) => [timing.sector, timing]));
  const holdS3 = currentSector === 1 && (sectorCompletionHoldByCar.get(car.carId) || 0) > now;
  for (let sector = 1; sector <= 3; sector += 1) {
    const bar = nodes.bars[sector - 1];
    const visible = sector <= sectorCount;
    bar.toggleAttribute('hidden', !visible);
    if (!visible) {
      bar.className = '';
      bar.removeAttribute('title');
      bar.setAttribute('aria-label', `Sector ${sector} unavailable`);
      continue;
    }
    const timing = sectorByNumber.get(sector);
    const state = currentSector === sector
      ? 'active'
      : currentSector && sector < currentSector
        ? 'done'
        : sector === 3 && holdS3 ? 'recent' : '';
    const resultClass = state === 'done' || state === 'recent'
      ? classifyCompletedSectorTime(
        timing?.lastMs,
        personalSectorBest.get(car.carId)?.get(sector),
        overallSectorBest.get(sector),
      )
      : '';
    const className = [state, resultClass].filter(Boolean).join(' ');
    if (bar.className !== className) bar.className = className;
    const resultLabel = resultClass === 'overall-best'
      ? ' overall best'
      : resultClass === 'personal-best' ? ' personal best' : '';
    const ariaLabel = `Sector ${sector} ${state || 'upcoming'}${resultLabel}`;
    if (bar.getAttribute('aria-label') !== ariaLabel) bar.setAttribute('aria-label', ariaLabel);
    const title = timing?.lastMs ? `S${sector} ${formatSplitTime(timing.lastMs)}` : '';
    if (title) {
      if (bar.title !== title) bar.title = title;
    } else {
      bar.removeAttribute('title');
    }
    if (state === 'recent') sectorCompletionNodeByCar.set(car.carId, bar);
  }
  const activeTiming = currentSector ? sectorByNumber.get(currentSector) : null;
  const localAdvance = raceReceivedAt ? Math.max(0, now - raceReceivedAt) : 0;
  const currentLapElapsed = currentLapClockValue(standing, raceState, localAdvance);
  const raceElapsed = markerRaceElapsedMs(car.carId, standing, now, currentLapElapsed);
  setTextIfChanged(nodes.current, formatSplitTime(elapsedSinceRaceMarkerMs(standing, raceElapsed)));
  setTextIfChanged(nodes.last, formatSplitTime(activeTiming?.lastMs));
  setTextIfChanged(nodes.best, formatSplitTime(activeTiming?.bestMs));
  const bestClass = classifyBestTime(activeTiming?.lastMs, activeTiming?.bestMs, overallSectorBest.get(currentSector));
  const lastClass = `sector-value sector-last timing-cell${bestClass ? ` ${bestClass}` : ''}`;
  if (nodes.last.className !== lastClass) nodes.last.className = lastClass;
  const storedBestClass = classifyBestTime(activeTiming?.bestMs, activeTiming?.bestMs, overallSectorBest.get(currentSector));
  const storedClass = `sector-value sector-best timing-cell${storedBestClass ? ` ${storedBestClass}` : ''}`;
  if (nodes.best.className !== storedClass) nodes.best.className = storedClass;
  sectorLiveNodeByCar.set(car.carId, nodes.current);
}

function renderSectorRows() {
  const root = document.getElementById('sectorRows');
  const sectorCars = selectedTeamCars();
  const density = sectorCars.length > 32 ? 'crowded' : sectorCars.length > 16 ? 'dense' : 'normal';
  if (root.dataset.density !== density) root.dataset.density = density;
  const signature = JSON.stringify({
    standings: (raceState?.standings || []).map((standing) => ({
      carId: standing.carId, status: standing.status, currentSector: standing.currentSector,
      sectorCount: standing.sectorCount, sectorTimes: standing.sectorTimes,
    })),
    history: normalizedLapHistory.map((entry) => ({ carId: entry.carId, sectorTimes: entry.sectorTimes })),
    holds: Array.from(sectorCompletionHoldByCar.entries()),
    selected: selectedTeamVehicleIds,
  });
  if (signature === sectorRowsSignature) return;
  const startedAt = UI_METRICS_ENABLED ? performance.now() : 0;
  sectorRowsSignature = signature;
  sectorLiveNodeByCar.clear();
  sectorCompletionNodeByCar.clear();
  const standings = raceState?.standings || [];
  const overallSectorBest = new Map();
  const personalSectorBest = new Map();
  const recordSectorBest = (carId, sector, value) => {
    if (!Number.isFinite(value) || value <= 0 || !Number.isInteger(sector)) return;
    if (!personalSectorBest.has(carId)) personalSectorBest.set(carId, new Map());
    const personal = personalSectorBest.get(carId);
    const previousPersonal = personal.get(sector);
    if (previousPersonal === undefined || value < previousPersonal) personal.set(sector, value);
    const previousOverall = overallSectorBest.get(sector);
    if (previousOverall === undefined || value < previousOverall) overallSectorBest.set(sector, value);
  };
  for (const standing of standings) {
    for (const timing of standing.sectorTimes || []) {
      recordSectorBest(standing.carId, timing?.sector, timing?.bestMs);
    }
  }
  for (const entry of normalizedLapHistory) {
    for (const timing of entry.sectorTimes || []) {
      recordSectorBest(entry.carId, timing?.sector, timing?.timeMs);
    }
  }
  const rows = standingsByConfiguredCar(sectorCars, raceState);
  const activeCarIds = new Set();
  let previousScrollTop = null;
  const preserveScroll = () => {
    if (previousScrollTop === null) previousScrollTop = root.scrollTop;
  };
  const now = performance.now();
  rows.forEach(({ car, standing }, index) => {
    activeCarIds.add(car.carId);
    let nodes = sectorContentByCar.get(car.carId);
    if (!nodes) {
      nodes = createSectorRow(car);
      sectorContentByCar.set(car.carId, nodes);
    }
    updateSectorRow(nodes, car, standing, personalSectorBest, overallSectorBest, now);
    const currentRow = root.children[index] || null;
    if (currentRow !== nodes.row) {
      preserveScroll();
      root.insertBefore(nodes.row, currentRow);
    }
  });
  for (const [carId, nodes] of sectorContentByCar) {
    if (activeCarIds.has(carId)) continue;
    preserveScroll();
    nodes.row.remove();
    sectorContentByCar.delete(carId);
    sectorLiveNodeByCar.delete(carId);
    sectorCompletionNodeByCar.delete(carId);
    sectorCompletionHoldByCar.delete(carId);
  }
  if (previousScrollTop !== null) root.scrollTop = previousScrollTop;
  if (UI_METRICS_ENABLED) sectorRenderSampler.record(performance.now() - startedAt);
}

function renderTimingRows() {
  const root = document.getElementById('timingRows');
  const history = normalizedLapHistory;
  const signature = JSON.stringify({
    history,
    best: (raceState?.standings || []).map((standing) => ({
      carId: standing.carId, driver: standing.driver, bestLapMs: standing.bestLapMs,
      sectorTimes: standing.sectorTimes,
    })),
  });
  if (signature === timingRowsSignature) return;
  timingRowsSignature = signature;
  const carById = new Map(observerConfig.cars.map((car) => [car.carId, car]));
  const standingById = new Map((raceState?.standings || []).map((standing) => [standing.carId, standing]));
  const overallLapBest = Math.min(...(raceState?.standings || [])
    .map((standing) => standing.bestLapMs)
    .filter((value) => Number.isFinite(value) && value > 0));
  const overallSectorBest = new Map();
  for (const standing of raceState?.standings || []) {
    for (const timing of standing.sectorTimes || []) {
      if (!Number.isFinite(timing?.bestMs)) continue;
      const previous = overallSectorBest.get(timing.sector);
      if (previous === undefined || timing.bestMs < previous) overallSectorBest.set(timing.sector, timing.bestMs);
    }
  }
  if (history.length === 0) {
    const row = document.createElement('tr');
    row.className = 'history-empty';
    const cell = document.createElement('td');
    cell.colSpan = 7;
    cell.textContent = raceState ? 'WAITING FOR LAP HISTORY' : 'WAITING FOR RACE DATA';
    row.append(cell);
    root.replaceChildren(row);
    document.getElementById('historyCount').textContent = 'LAST 0';
    return;
  }
  root.replaceChildren(...history.map((entry) => {
    const car = carById.get(entry.carId);
    const standing = standingById.get(entry.carId);
    const driverName = String(standing?.driver || car?.driver || entry.carId).trim();
    const driverShortName = abbreviateDriverName(driverName, entry.carId);
    const sectorTimeByNumber = new Map(entry.sectorTimes.map((timing) => [timing.sector, timing.timeMs]));
    const row = document.createElement('tr');
    const values = [
      formatDuration(entry.completedAtRaceMs),
      `${car ? `#${car.displayNumber}` : entry.carId} ${driverShortName}`,
      `L${entry.lap}`,
      formatSplitTime(sectorTimeByNumber.get(1)),
      formatSplitTime(sectorTimeByNumber.get(2)),
      formatSplitTime(sectorTimeByNumber.get(3)),
      formatDuration(entry.lapTimeMs),
    ];
    values.forEach((value, index) => {
      const cell = document.createElement('td');
      if (index === 1) {
        const badge = element('span', 'table-car', value);
        badge.style.setProperty('--car', car?.color || '#75E36A');
        badge.title = driverName;
        cell.append(badge);
      } else cell.textContent = value;
      if (index >= 3 && index <= 5) {
        const sector = index - 2;
        const personalBest = standing?.sectorTimes?.find((timing) => timing.sector === sector)?.bestMs;
        const className = classifyBestTime(sectorTimeByNumber.get(sector), personalBest, overallSectorBest.get(sector));
        cell.classList.add('timing-cell');
        if (className) cell.classList.add(className);
      }
      if (index === 6) {
        cell.classList.add('lap-time');
        const className = classifyBestTime(entry.lapTimeMs, standing?.bestLapMs,
          Number.isFinite(overallLapBest) ? overallLapBest : null);
        cell.classList.add('timing-cell');
        if (className) cell.classList.add(className);
      }
      row.append(cell);
    });
    return row;
  }));
  document.getElementById('historyCount').textContent = `LAST ${history.length}`;
}

function renderSituations() {
  const root = document.getElementById('situationRows');
  const situations = deriveSituations(
    observerConfig.cars,
    raceState,
    healthByCar,
    connectionByCar,
    telemetryByCar,
    vehicleEventByCar,
    pitByCar,
  );
	if (teamDirectory?.stale) {
		const ageMs = Math.max(0, Number(teamDirectory.ageMs) || 0);
		const age = ageMs >= 3600000
			? `${Math.floor(ageMs / 3600000)}H OLD`
			: `${Math.max(1, Math.floor(ageMs / 60000))}M OLD`;
		situations.push({
			type: 'directory-stale',
			label: 'DIRECTORY STALE',
			primary: teamDirectory.event.name || 'VEHICLE DIRECTORY',
			detail: age,
			tone: 'watch',
			priority: 40,
		});
		situations.sort((left, right) => right.priority - left.priority).splice(4);
	}
  const signature = JSON.stringify(situations.map((situation) => [
    situation.type, situation.label, situation.primary, situation.detail, situation.tone, situation.priority,
  ]));
  if (signature === situationsSignature) return;
  situationsSignature = signature;
  if (situations.length === 0) {
    root.replaceChildren(element('article', 'situation-item situation-watch situation-empty', 'WAITING FOR LIVE RACE DATA'));
    return;
  }
  root.replaceChildren(...situations.map((situation) => {
    const item = element('article', `situation-item situation-${situation.tone}`);
    item.append(element('span', 'situation-kind', situation.label), element('strong', 'situation-primary', situation.primary), element('span', 'situation-detail', situation.detail));
    return item;
  }));
}

function renderHeader() {
  const connected = countActiveVideos(connectionByCar);
  const liveStatus = document.getElementById('liveStatus');
  liveStatus.textContent = raceState ? displayRaceStatus(raceState) : connected ? 'RACE WAIT' : 'WAITING';
  liveStatus.dataset.flag = String(raceState?.flag || 'none').toLowerCase();
  document.getElementById('heatValue').textContent = raceState?.raceInfo?.title || '--';
  document.getElementById('trackName').textContent = raceState?.raceInfo?.track || observerConfig.trackName;
  const leader = (raceState?.standings || []).find((standing) => standing.position === 1);
  const totalLaps = raceState?.raceInfo?.totalLaps;
  document.getElementById('lapValue').innerHTML = `${leader?.lap ?? '--'} <em>/ ${totalLaps ?? '--'}</em>`;
  const panelCount = document.getElementById('panelCount');
  panelCount.textContent = `${connected}/${TEAM_SELECTION_LIMIT} VIDEO`;
	panelCount.dataset.state = selectedTeamVehicleIds.length > 0 && connected === selectedTeamVehicleIds.length
    ? 'all' : connected > 0 ? 'partial' : 'none';
}

function renderPitState() {
  const active = Array.from(pitByCar.values()).filter((pit) => pit.present);
  const zone = document.getElementById('pitRecoveryZone');
  if (!zone) return;
  zone.classList.toggle('is-active', active.length > 0);
  zone.classList.toggle('is-complete', active.length > 0
    && active.every((pit) => pit.serviceState === 'complete'));
}

function createTrackMarkers() {
  const root = document.getElementById('trackMarkers');
  markerNodesByCar.clear();
  root.dataset.density = observerConfig.cars.length > 48
    ? 'crowded' : observerConfig.cars.length > 16 ? 'dense' : 'normal';
  root.replaceChildren(...observerConfig.cars.map((car) => {
    const selected = isTeamCarSelected(car);
    const marker = svgElement('g', {
      id: `marker-${car.carId}`,
      class: `car-marker${selected ? ' is-team-selected' : ''}`,
      'aria-label': `${car.carId} estimated course position`,
      'aria-pressed': selected ? 'true' : 'false',
      role: 'button',
      tabindex: '0',
      hidden: '',
    });
    applyCarAccent(marker, car);
    const hit = svgElement('circle', { class: 'marker-hit', r: 30 });
    const confidence = svgElement('circle', { class: 'marker-confidence', r: 23 });
    const core = svgElement('circle', { class: 'marker-core', r: 16 });
    marker.append(hit, confidence, core);
    const label = svgElement('text', { class: 'marker-label', y: 3 });
    label.textContent = car.displayNumber;
    const title = svgElement('title');
    title.textContent = `${car.carId} waiting for sector timing`;
    marker.append(label, title);
    marker.addEventListener('click', () => requestTeamCar(car));
    marker.addEventListener('keydown', (event) => {
      if (event.key !== 'Enter' && event.key !== ' ') return;
      event.preventDefault();
      requestTeamCar(car);
    });
    const nodes = { marker, hit, confidence, core, label, title, labelKey: '' };
    updateTrackMarkerDensity(nodes, selected, observerConfig.cars.length);
    markerNodesByCar.set(car.carId, nodes);
    return marker;
  }));
}

function updateTrackMarkerDensity(nodes, selected, fieldSize) {
  if (!nodes) return;
  const crowded = fieldSize > 48;
  const dense = fieldSize > 16;
  const hitRadius = selected ? 30 : crowded ? 13 : dense ? 19 : 30;
  const confidenceRadius = selected ? 23 : crowded ? 10 : dense ? 15 : 23;
  const coreRadius = selected ? 16 : crowded ? 7 : dense ? 10 : 16;
  for (const [node, radius] of [
    [nodes.hit, hitRadius],
    [nodes.confidence, confidenceRadius],
    [nodes.core, coreRadius],
  ]) {
    const value = String(radius);
    if (node?.getAttribute('r') !== value) node?.setAttribute('r', value);
  }
}

function updateTrackMarkerLabel(nodes, car, standing) {
  const showLabel = observerConfig.cars.length <= 16
    || isTeamCarSelected(car)
    || standing?.position <= 4
    || vehicleEventByCar.has(car.carId);
  const driverLabel = abbreviateDriverName(standing?.driver || car.driver, '');
  const labelKey = `${showLabel}:${driverLabel}:${car.displayNumber}`;
  if (nodes.labelKey === labelKey) return;
  nodes.labelKey = labelKey;
  nodes.label.toggleAttribute('hidden', !showLabel);
  if (!showLabel) return;
  const preferred = driverLabel || car.displayNumber;
  setTextIfChanged(nodes.label, preferred);
  nodes.marker.dataset.labelMode = driverLabel ? 'name' : 'number';
  if (!driverLabel || typeof nodes.label.getComputedTextLength !== 'function') return;
  let width = 0;
  try {
    width = nodes.label.getComputedTextLength();
  } catch (_) {
    return;
  }
  if (width <= 28) return;
  setTextIfChanged(nodes.label, car.displayNumber);
  nodes.marker.dataset.labelMode = 'number';
}

function readTrackGeometry() {
  const coursePath = document.getElementById('coursePath');
  const pitPath = document.getElementById('pitPath');
  if (!coursePath || typeof coursePath.getTotalLength !== 'function') return null;
  const pitAvailable = pitPath && typeof pitPath.getTotalLength === 'function';
  return {
    coursePath,
    pitPath: pitAvailable ? pitPath : null,
    courseLength: coursePath.getTotalLength(),
    pitLength: pitAvailable ? pitPath.getTotalLength() : 0,
  };
}

function markerRaceElapsedMs(carId, standing, now, currentLapElapsed) {
  const running = raceState?.phase === 'green' && standing?.status === 'racing';
  const localAdvance = running && raceReceivedAt ? Math.max(0, now - raceReceivedAt) : 0;
  if (Number.isFinite(standing?.raceElapsedMs)) return standing.raceElapsedMs + localAdvance;
  const reconstructed = reconstructRaceElapsedMs(standing, normalizedLapHistory, currentLapElapsed);
  if (reconstructed !== null) return reconstructed;
  if (raceState?.allTimeMode !== 'countdown' && Number.isFinite(standing?.allTimeMs)) {
    return standing.allTimeMs + localAdvance;
  }
  const anchorRaceMs = Number.isInteger(standing?.routeProgress?.raceTimeMs)
    ? standing.routeProgress.raceTimeMs
    : standing?.lastMarkerRaceMs;
  if (!Number.isInteger(anchorRaceMs)) return null;
  const anchorKey = standing?.routeProgress
    ? `${standing.routeProgress.gateIndex}:${standing.routeProgress.raceTimeMs}`
    : `${standing.lastMarkerIndex}:${standing.lastMarkerRaceMs}`;
  const key = `${raceState?.raceRunId || ''}:${anchorKey}`;
  let motion = markerMotionByCar.get(carId);
  if (!motion || motion.key !== key) {
    motion = { key, elapsedMs: 0, updatedAt: now };
    markerMotionByCar.set(carId, motion);
  }
  if (running) motion.elapsedMs += Math.max(0, now - motion.updatedAt);
  motion.updatedAt = now;
  return anchorRaceMs + motion.elapsedMs;
}

function setTextIfChanged(node, value) {
  if (node && node.textContent !== value) node.textContent = value;
}

function setDatasetIfChanged(node, name, value) {
  if (node && node.dataset[name] !== value) node.dataset[name] = value;
}

function rebuildRaceViewCache() {
  const standings = Array.isArray(raceState?.standings) ? raceState.standings : [];
  standingByCar = new Map(standings.map((standing) => [standing.carId, standing]));
  normalizedLapHistory = normalizeLapHistory(raceState);
  lapPaceByCar = new Map(standings.map((standing) => [
    standing.carId,
    estimateLapDurationMs(
      raceState,
      standing,
      COURSE_SECTOR_BOUNDARIES,
      observerConfig.motion.defaultLapMs,
      normalizedLapHistory,
    ),
  ]));
}

function markerKey(standing) {
  if (Number.isInteger(standing?.routeProgress?.gateIndex)
      && Number.isInteger(standing?.routeProgress?.raceTimeMs)) {
    return `route:${standing.routeProgress.gateIndex}:${standing.routeProgress.raceTimeMs}`;
  }
  return Number.isInteger(standing?.lastMarkerIndex) && Number.isInteger(standing?.lastMarkerRaceMs)
    ? `${standing.lastMarkerIndex}:${standing.lastMarkerRaceMs}`
    : '';
}

function motionOptions() {
  return {
    minimumMs: observerConfig.motion.checkpointGraceMinMs,
    maximumMs: observerConfig.motion.checkpointGraceMaxMs,
    ratio: observerConfig.motion.checkpointGraceRatio,
    clockToleranceMs: observerConfig.motion.markerClockToleranceMs,
  };
}

function pointOnCourse(path, length, progress) {
  let lookup = trackPathLookupByElement.get(path);
  if (!lookup) {
    lookup = raceUiPerformance.createSvgPathLookup(path);
    if (lookup) trackPathLookupByElement.set(path, lookup);
  }
  if (lookup) return raceUiPerformance.pointAtProgress(lookup, progress);
  let normalized = ((progress % 1) + 1) % 1;
  if (progress > 0 && Math.abs(normalized) < 0.000001) normalized = 1;
  return path.getPointAtLength(normalized * length);
}

function markerTargetOnTrack(car, standing, now, path, length) {
  const localAdvance = raceReceivedAt ? Math.max(0, now - raceReceivedAt) : 0;
  const currentLapElapsed = currentLapClockValue(standing, raceState, localAdvance);
  const raceElapsed = markerRaceElapsedMs(car.carId, standing, now, currentLapElapsed);
  const pace = lapPaceByCar.get(car.carId);
  const estimate = pace && standing?.sectorCount === 3
    ? estimateLapPacedProgress(
      standing,
      raceElapsed,
      currentLapElapsed,
      COURSE_SECTOR_BOUNDARIES,
      pace.durationMs,
      motionOptions(),
    )
    : null;
  if (!estimate) return null;
  const point = pointOnCourse(path, length, estimate.courseProgress);
  const correctionState = estimate.state === 'holding-checkpoint' ? 'hold' : 'drive';
  return {
    x: point.x,
    y: point.y,
    key: `${raceState.raceRunId || ''}:track:${markerKey(standing)}:${correctionState}`,
    state: estimate.state,
    confidenceRadius: estimate.state === 'projected' ? 23 : estimate.state === 'awaiting-checkpoint' ? 28 : 31,
    title: `CAR ${car.displayNumber} ${estimate.state.replaceAll('-', ' ')} / ${pace.source} ${Math.round(pace.durationMs / 100) / 10}s`,
    correctionMs: observerConfig.motion.markerCorrectionMs,
  };
}

function advanceMotionClock(motion, now, running) {
  if (!Number.isFinite(motion.updatedAt)) motion.updatedAt = now;
  if (running) motion.elapsedMs += Math.max(0, now - motion.updatedAt);
  motion.updatedAt = now;
  return motion.elapsedMs;
}

function markerTargetInPit(car, standing, now, coursePath, courseLength, pitPath, pitLength) {
  if (!pitPath || !pitLength) return null;
  const pit = pitByCar.get(car.carId);
  let motion = pitMotionByCar.get(car.carId);
  if (!motion || motion.entryId !== pit?.entryId) return null;
  const running = raceState?.phase === 'green' && standing?.status === 'racing';
  const serviceProgress = observerConfig.motion.pitServiceProgress;
  const servicePoint = pitPath.getPointAtLength(serviceProgress * pitLength);

  if (pit?.present) {
    if (motion.phase === 'pit-entry') {
      const elapsed = advanceMotionClock(motion, now, running);
      const duration = observerConfig.motion.pitEntryMs;
      const progress = duration > 0 ? Math.min(1, elapsed / duration) : 1;
      const eased = 1 - ((1 - progress) ** 3);
      const point = pitPath.getPointAtLength((serviceProgress * eased) * pitLength);
      if (progress >= 1) motion.phase = 'pit-service';
      return {
        x: point.x,
        y: point.y,
        key: `${raceState.raceRunId || ''}:pit-entry:${motion.entryId}`,
        state: 'pit-entry',
        confidenceRadius: 23,
        title: `CAR ${car.displayNumber} PIT IN`,
        correctionMs: observerConfig.motion.markerCorrectionMs,
      };
    }
    return {
      x: servicePoint.x,
      y: servicePoint.y,
      key: `${raceState.raceRunId || ''}:pit-service:${motion.entryId}`,
      state: pit.serviceState === 'complete' ? 'pit-complete' : 'pit-service',
      confidenceRadius: 21,
      title: `CAR ${car.displayNumber} ${pit.serviceState === 'complete' ? 'SERVICE COMPLETE' : 'PIT SERVICE'}`,
      correctionMs: observerConfig.motion.markerCorrectionMs,
    };
  }

  if (motion.phase === 'pit-exit') {
    const elapsed = advanceMotionClock(motion, now, running);
    const progress = Math.min(1, elapsed / observerConfig.motion.pitExitMs);
    const eased = 1 - ((1 - progress) ** 2);
    const pitProgress = serviceProgress + ((1 - serviceProgress) * eased);
    const point = pitPath.getPointAtLength(pitProgress * pitLength);
    if (progress >= 1) {
      motion.phase = 'track-after-pit';
      motion.elapsedMs = 0;
      motion.updatedAt = now;
      motion.markerKeyAtExit = markerKey(standing);
    }
    return {
      x: point.x,
      y: point.y,
      key: `${raceState.raceRunId || ''}:pit-exit:${motion.entryId}`,
      state: 'pit-exit',
      confidenceRadius: 24,
      title: `CAR ${car.displayNumber} PIT OUT`,
      correctionMs: 0,
    };
  }

  if (motion.phase === 'track-after-pit') {
    const currentMarkerKey = markerKey(standing);
    if (currentMarkerKey && currentMarkerKey !== motion.markerKeyAtExit) {
      pitMotionByCar.delete(car.carId);
      return null;
    }
    const pace = lapPaceByCar.get(car.carId);
    const elapsed = advanceMotionClock(motion, now, running);
    const projection = pace ? projectCourseProgress(
      observerConfig.motion.pitRejoinProgress,
      COURSE_SECTOR_BOUNDARIES[1],
      elapsed,
      pace.durationMs,
      motionOptions(),
    ) : null;
    if (!projection) return null;
    const point = pointOnCourse(coursePath, courseLength, projection.courseProgress);
    return {
      x: point.x,
      y: point.y,
      key: `${raceState.raceRunId || ''}:track-after-pit:${motion.entryId}:${projection.state === 'holding-checkpoint' ? 'hold' : 'drive'}`,
      state: projection.state === 'holding-checkpoint' ? 'holding-checkpoint' : 'pit-rejoin',
      confidenceRadius: projection.state === 'holding-checkpoint' ? 31 : 26,
      title: `CAR ${car.displayNumber} PIT REJOIN / ${pace.source}`,
      correctionMs: observerConfig.motion.markerCorrectionMs,
    };
  }
  return null;
}

function renderMarkerTarget(nodes, car, index, target, now) {
  const { marker, confidence, title } = nodes;
  let rendered = markerRenderByCar.get(car.carId);
  if (!rendered) {
    rendered = { key: target.key, x: target.x, y: target.y, correctionStartedAt: 0 };
    markerRenderByCar.set(car.carId, rendered);
  } else if (rendered.key !== target.key) {
    rendered.key = target.key;
    rendered.correctionStartedAt = target.correctionMs > 0 ? now : 0;
    rendered.correctionFromX = rendered.x;
    rendered.correctionFromY = rendered.y;
  }
  if (rendered.correctionStartedAt) {
    const correction = Math.min(1, (now - rendered.correctionStartedAt) / target.correctionMs);
    const eased = 1 - ((1 - correction) ** 3);
    rendered.x = rendered.correctionFromX + ((target.x - rendered.correctionFromX) * eased);
    rendered.y = rendered.correctionFromY + ((target.y - rendered.correctionFromY) * eased);
    if (correction >= 1) rendered.correctionStartedAt = 0;
  } else {
    rendered.x = target.x;
    rendered.y = target.y;
  }
  const [offsetX, offsetY] = raceUiPerformance.markerOffset(index, observerConfig.cars.length);
  marker.removeAttribute('hidden');
  marker.setAttribute('transform', `translate(${(rendered.x + offsetX).toFixed(2)} ${(rendered.y + offsetY).toFixed(2)})`);
  if (marker.dataset.motionState !== target.state) marker.dataset.motionState = target.state;
  const selected = isTeamCarSelected(car);
  const radius = String(selected
    ? target.confidenceRadius
    : observerConfig.cars.length > 48
      ? Math.min(10, target.confidenceRadius)
      : observerConfig.cars.length > 16 ? Math.min(15, target.confidenceRadius) : target.confidenceRadius);
  if (confidence.getAttribute('r') !== radius) confidence.setAttribute('r', radius);
  setTextIfChanged(title, target.title);
}

function updateTrackMarkers(now) {
  if (!observerConfig || !raceState || !trackGeometry) return;
  const startedAt = UI_METRICS_ENABLED ? performance.now() : 0;
  const { coursePath, courseLength, pitPath, pitLength } = trackGeometry;
  observerConfig.cars.forEach((car, index) => {
    const nodes = markerNodesByCar.get(car.carId);
    const marker = nodes?.marker;
    const standing = standingByCar.get(car.carId);
    if (!marker || !standing) {
      marker?.setAttribute('hidden', '');
      return;
    }
    let target = markerTargetInPit(car, standing, now, coursePath, courseLength, pitPath, pitLength);
    if (!target) target = markerTargetOnTrack(car, standing, now, coursePath, courseLength);
    if (!target) {
      marker.setAttribute('hidden', '');
      return;
    }
    marker.dataset.sector = Number.isInteger(standing.currentSector) ? String(standing.currentSector) : '';
    renderMarkerTarget(nodes, car, index, target, now);
    updateTrackMarkerLabel(nodes, car, standing);
    applyCarEffect(car.carId);
  });
  if (UI_METRICS_ENABLED) {
    markerRenderSampler.record(performance.now() - startedAt);
    markerMetricLogCounter += 1;
    if (markerMetricLogCounter % 40 === 0) {
      console.info('[TEAM OBSERVER UI METRICS]', JSON.stringify(publishUiDiagnostics()));
    }
  }
}

function displayedRaceTime(now) {
  return raceClockValue(raceState, raceReceivedAt ? now - raceReceivedAt : 0, normalizedLapHistory);
}

function renderClocks(now) {
  setTextIfChanged(document.getElementById('raceClock'), formatDuration(displayedRaceTime(now)));
  const age = raceReceivedAt ? (now - raceReceivedAt) / 1000 : null;
  const updatedAgo = document.getElementById('updatedAgo');
  setTextIfChanged(updatedAgo, age === null ? '--' : `${age.toFixed(1)}s`);
  const freshness = age === null ? 'waiting' : age < 3 ? 'live' : age < 10 ? 'delayed' : 'stale';
  if (updatedAgo.dataset.freshness !== freshness) updatedAgo.dataset.freshness = freshness;
  if (raceState && observerConfig) {
    for (const car of observerConfig.cars) {
      const standing = standingByCar.get(car.carId);
      const currentLapElapsed = currentLapClocks.value(car.carId, now);
      const raceElapsed = markerRaceElapsedMs(car.carId, standing, now, currentLapElapsed);
      setTextIfChanged(
        sectorLiveNodeByCar.get(car.carId),
        formatSplitTime(elapsedSinceRaceMarkerMs(standing, raceElapsed)),
      );
      const holdUntil = sectorCompletionHoldByCar.get(car.carId) || 0;
      if (holdUntil > 0 && holdUntil <= now) {
        sectorCompletionHoldByCar.delete(car.carId);
        const completionNode = sectorCompletionNodeByCar.get(car.carId);
        if (completionNode) {
          completionNode.classList.remove('recent');
          completionNode.setAttribute('aria-label', 'Sector 3 upcoming');
          sectorCompletionNodeByCar.delete(car.carId);
        }
      }
    }
  }
}

function renderTelemetryDisplays() {
  if (!observerConfig) return;
  for (const car of observerConfig.cars) {
    const telemetry = telemetryByCar.get(car.carId);
    if (!telemetry || renderedTelemetryByCar.get(car.carId) === telemetry) continue;
    renderCameraTelemetry(car, telemetry);
    renderedTelemetryByCar.set(car.carId, telemetry);
  }
}

function setPedalLevel(node, value) {
  const level = Math.max(0, Math.min(1, Number(value) || 0));
  const transform = `scaleY(${level.toFixed(3)})`;
  if (node.style.transform !== transform) node.style.transform = transform;
}

function renderControlDisplays(now) {
  if (!observerConfig) return;
  for (const car of observerConfig.cars) {
    const nodes = controlNodesByCar.get(car.carId);
    if (!nodes) continue;
    const control = controlByCar.get(car.carId);
    const active = Boolean(control && now - control.receivedAt <= CONTROL_STALE_MS);
    const throttle = active ? control.throttle : 0;
    const brake = active ? control.brake : 0;
    setPedalLevel(nodes.throttle, throttle);
    setPedalLevel(nodes.brake, brake);
    nodes.root.dataset.active = active ? 'true' : 'false';
    nodes.root.setAttribute('aria-label', `Throttle ${Math.round(throttle * 100)} percent, brake ${Math.round(brake * 100)} percent`);
  }
}

function updateAnimationFrame(now) {
  if (now - clockRenderedAt >= CLOCK_RENDER_INTERVAL_MS) {
    clockRenderedAt = now;
    renderClocks(now);
  }
  if (now - markerRenderedAt >= MARKER_RENDER_INTERVAL_MS) {
    markerRenderedAt = now;
    updateTrackMarkers(now);
  }
  if (now - telemetryRenderedAt >= TELEMETRY_RENDER_INTERVAL_MS) {
    telemetryRenderedAt = now;
    renderTelemetryDisplays();
    renderControlDisplays(now);
  }
  if (observerConfig && now - raceStatusRenderedAt >= TRANSPORT_RENDER_INTERVAL_MS) {
    raceStatusRenderedAt = now;
    for (const car of selectedTeamCars()) renderCameraTransportState(car, now);
  }
  animationFrame = requestAnimationFrame(updateAnimationFrame);
}

function renderAll() {
  if (!observerConfig) return;
  const startedAt = UI_METRICS_ENABLED ? performance.now() : 0;
  renderHeader();
  renderCameraTitles();
  renderCameraHealthDisplays();
  renderTelemetryDisplays();
  renderLeaderboard();
  renderSectorRows();
  renderTimingRows();
  renderSituations();
  renderPitState();
  renderTeamSelectionControls();
  if (UI_METRICS_ENABLED) overviewRenderSampler.record(performance.now() - startedAt);
}

function renderCameraTitles() {
  for (const car of observerConfig.cars) {
    const nodes = cameraTitleNodesByCar.get(car.carId);
    if (!nodes) continue;
    const driverName = standingByCar.get(car.carId)?.driver || car.driver || car.vehicleName || car.device;
    nodes.driver.textContent = driverName;
    nodes.root.title = driverName;
  }
}

function clamp01(value) {
  return Math.max(0, Math.min(1, Number.isFinite(value) ? value : 0));
}

function setMeterLevel(fill, level) {
  if (fill) fill.style.transform = `scaleX(${clamp01(level).toFixed(3)})`;
}

function setInstrumentState(node, state) {
  if (node && node.dataset.state !== state) node.dataset.state = state;
}

function classifyLowInstrument(value, warning, critical) {
  if (!Number.isFinite(value)) return 'waiting';
  if (value <= critical) return 'critical';
  if (value <= warning) return 'warning';
  return 'normal';
}

function classifyHighInstrument(value, warning, critical) {
  if (!Number.isFinite(value)) return 'waiting';
  if (value >= critical) return 'critical';
  if (value >= warning) return 'warning';
  return 'normal';
}

function createCameraVital(kind, label, unit) {
  const root = element('div', `camera-vital camera-vital-${kind}`);
  root.dataset.state = 'waiting';
  const icon = element('i', 'camera-vital-icon');
  icon.setAttribute('aria-hidden', 'true');
  const copy = element('span', 'camera-vital-copy');
  copy.append(element('small', '', label));
  const reading = element('strong');
  const value = element('output', '', '--');
  reading.append(value, element('em', '', unit));
  copy.append(reading);
  root.append(icon, copy);
  return { root, value };
}

function createCameraResource(kind, label) {
  const root = element('div', `camera-resource camera-resource-${kind}`);
  root.dataset.state = 'waiting';
  const icon = element('i', 'camera-resource-icon');
  icon.setAttribute('aria-hidden', 'true');
  const name = element('span', 'camera-resource-label', label);
  const track = element('span', 'camera-resource-track');
  const fill = element('i', 'camera-resource-fill');
  track.append(fill);
  const value = element('output', '', 'WAIT');
  root.append(icon, name, track, value);
  return { root, fill, value };
}

function renderCameraHealth(car, health = healthByCar.get(car.carId)) {
  const nodes = healthNodesByCar.get(car.carId);
  if (!nodes) return;
  if (!health) {
    for (const resource of [nodes.damage, nodes.fuel, nodes.boost]) {
      setInstrumentState(resource.root, 'waiting');
      setMeterLevel(resource.fill, 0);
      setTextIfChanged(resource.value, 'WAIT');
    }
    setTextIfChanged(nodes.gear, 'G--');
    return;
  }

  const hp = Number(health.hp);
  const damage = Number.isFinite(hp) ? Math.max(0, Math.min(100, 100 - hp)) : null;
  const damageState = health.mode === 'critical' || health.mode === 'limp' || damage >= 60
    ? 'critical' : health.mode === 'damaged' || damage >= 30 ? 'warning' : 'normal';
  setInstrumentState(nodes.damage.root, damage === null ? 'waiting' : damageState);
  setMeterLevel(nodes.damage.fill, damage === null ? 0 : damage / 100);
  setTextIfChanged(nodes.damage.value, damage === null ? 'WAIT' : `${Math.round(damage)}%`);

  const fuel = Number(health.fuel);
  const fuelState = health.fuelState === 'empty' ? 'critical'
    : health.fuelState === 'low' ? 'warning' : Number.isFinite(fuel) ? 'normal' : 'waiting';
  setInstrumentState(nodes.fuel.root, fuelState);
  setMeterLevel(nodes.fuel.fill, Number.isFinite(fuel) ? fuel / 100 : 0);
  setTextIfChanged(nodes.fuel.value, fuelState === 'critical' ? 'EMPTY'
    : Number.isFinite(fuel) ? `${Math.round(fuel)}%` : 'WAIT');

  const boost = Number(health.boost);
  const boostState = health.boostState === 'active' ? 'active'
    : health.boostState === 'ready' ? 'ready' : Number.isFinite(boost) ? 'charging' : 'waiting';
  setInstrumentState(nodes.boost.root, boostState);
  setMeterLevel(nodes.boost.fill, Number.isFinite(boost) ? boost / 100 : 0);
  const boostText = boostState === 'active' && Number.isFinite(health.boostRemainingMs)
    ? `${(health.boostRemainingMs / 1000).toFixed(1)}s`
    : boostState === 'ready' ? 'READY' : Number.isFinite(boost) ? `${Math.round(boost)}%` : 'WAIT';
  setTextIfChanged(nodes.boost.value, boostText);
  setTextIfChanged(nodes.gear, Number.isInteger(health.gear) ? `G${health.gear}` : 'G--');
}

function renderCameraHealthDisplays() {
  if (!observerConfig) return;
  for (const car of observerConfig.cars) renderCameraHealth(car);
}

function createCameraTile(car) {
    const cached = cameraTileNodesByCar.get(car.carId);
    if (cached) return cached;
    const tile = applyCarAccent(element('article', 'camera-tile'), car);
    tile.dataset.carId = car.carId;
    const head = element('div', 'camera-head');
    const title = element('strong', '', `CAR ${car.displayNumber} `);
		const driver = element('span', '', car.driver || car.vehicleName || car.device || 'SOURCE UNBOUND');
    title.append(driver);
    cameraTitleNodesByCar.set(car.carId, { root: title, driver });
    const status = element('span', '', '');
    status.id = `camera-status-${car.carId}`;
    status.append(element('i'), document.createTextNode('WAITING'));
    const fps = element('em', '', '-- FPS');
    fps.id = `camera-fps-${car.carId}`;
    head.append(title, status, fps);
    const feed = element('div', 'camera-feed');
    const video = document.createElement('video');
    video.id = `video-${car.carId}`;
    video.autoplay = true;
    video.muted = true;
    video.playsInline = true;
    video.classList.toggle('video-flipped', car.flip);
    video.setAttribute('aria-label', `CAR ${car.displayNumber} onboard video`);
    const videoState = element('span', 'video-state', 'WAITING FOR RELAY');
    videoState.id = `video-state-${car.carId}`;
    const eventFlash = element('strong', 'camera-event-flash');
    eventFlash.hidden = true;
    const dashboard = element('div', 'camera-dashboard');
    dashboard.id = `camera-dashboard-${car.carId}`;
    dashboard.dataset.active = 'false';

    const motionCard = element('section', 'camera-instrument camera-motion-card');
    motionCard.setAttribute('aria-label', 'Vehicle acceleration and yaw');
    const motionStatus = element('div', 'camera-motion-status');
    const rate = element('strong', 'telemetry-rate', '--Hz');
    const loss = element('span', 'telemetry-loss', 'L--');
    motionStatus.append(rate, loss);
    const motionScope = element('div', 'camera-motion-scope');
    motionScope.setAttribute('aria-hidden', 'true');
    motionScope.append(
      element('i', 'camera-motion-ring'),
      element('i', 'camera-motion-axis horizontal'),
      element('i', 'camera-motion-axis vertical'),
    );
    const motionDot = element('b', 'camera-motion-dot');
    motionScope.append(motionDot);
    const motionValues = element('div', 'camera-motion-values');
    const lateral = element('span', 'telemetry-lateral', 'L --');
    const forward = element('span', 'telemetry-forward', 'F --');
    const yaw = element('span', 'telemetry-yaw', 'Y --');
    motionValues.append(lateral, forward, yaw);
    motionCard.append(motionStatus, motionScope, motionValues);

    const powerCard = element('section', 'camera-instrument camera-power-card');
    powerCard.setAttribute('aria-label', 'ESC powertrain telemetry');
    const rpmRow = element('div', 'camera-rpm camera-speed');
    const rpmLabel = element('span', '', car.speedProfile ? 'EST KM/H' : 'RPM');
    const rpm = element('output', '', '--');
    const rpmTrack = element('span', 'camera-rpm-track');
    const rpmFill = element('i', 'camera-rpm-fill');
    rpmTrack.append(rpmFill);
    rpmRow.append(rpmLabel, rpm, rpmTrack);
    const vitalRow = element('div', 'camera-vitals');
    const voltage = createCameraVital('battery', 'BAT', 'V');
    const escTemp = createCameraVital('esc', 'ESC', '°');
    const motorTemp = createCameraVital('motor', 'MTR', '°');
    vitalRow.append(voltage.root, escTemp.root, motorTemp.root);
    powerCard.append(rpmRow, vitalRow);

    const resourceCard = element('section', 'camera-instrument camera-resource-card');
    resourceCard.setAttribute('aria-label', 'Damage, fuel and boost status');
    const resourceHead = element('div', 'camera-resource-head');
    resourceHead.append(element('span', '', 'VEHICLE'), element('strong', 'camera-gear', 'G--'));
    const damage = createCameraResource('damage', 'DMG');
    const fuel = createCameraResource('fuel', 'FUEL');
    const boost = createCameraResource('boost', 'BOOST');
    resourceCard.append(resourceHead, damage.root, fuel.root, boost.root);

    dashboard.append(motionCard, powerCard, resourceCard);
    telemetryNodesByCar.set(car.carId, {
      root: dashboard,
      rate,
      loss,
      motionScope,
      motionDot,
      lateral,
      forward,
      yaw,
      rpmLabel,
      rpm,
      rpmFill,
      voltage: voltage.value,
      voltageRoot: voltage.root,
      escTemp: escTemp.value,
      escTempRoot: escTemp.root,
      motorTemp: motorTemp.value,
      motorTempRoot: motorTemp.root,
    });
    healthNodesByCar.set(car.carId, {
      gear: resourceHead.lastChild,
      damage,
      fuel,
      boost,
    });
    const controls = element('div', 'camera-controls');
    controls.dataset.active = 'false';
    controls.setAttribute('aria-label', 'Throttle 0 percent, brake 0 percent');
    const throttleMeter = element('span', 'camera-pedal camera-pedal-throttle');
    const throttleTrack = element('i', 'camera-pedal-track');
    const throttleFill = element('b', 'camera-pedal-fill');
    throttleTrack.append(throttleFill);
    throttleMeter.append(throttleTrack, element('em', '', 'T'));
    const brakeMeter = element('span', 'camera-pedal camera-pedal-brake');
    const brakeTrack = element('i', 'camera-pedal-track');
    const brakeFill = element('b', 'camera-pedal-fill');
    brakeTrack.append(brakeFill);
    brakeMeter.append(brakeTrack, element('em', '', 'B'));
    controls.append(brakeMeter, throttleMeter);
    controlNodesByCar.set(car.carId, { root: controls, throttle: throttleFill, brake: brakeFill });
    feed.append(video, dashboard, controls, videoState, eventFlash);
    tile.append(head, feed);
    cameraEffectNodesByCar.set(car.carId, { root: tile, badge: eventFlash });
    cameraTileNodesByCar.set(car.carId, tile);
    applyCarEffect(car.carId);
    return tile;
}

function createEmptyCameraTile(index) {
  const tile = element('article', 'camera-tile camera-slot-empty');
  const button = element('button', 'camera-slot-select');
  button.type = 'button';
  button.setAttribute('aria-label', `Select a car for team monitor slot ${index + 1}`);
  button.append(element('strong', '', `SLOT ${index + 1}`), element('span', '', 'SELECT CAR'));
  button.addEventListener('click', openTeamSelector);
  tile.append(button);
  return tile;
}

function createCameraTiles() {
  const root = document.getElementById('cameraGrid');
  if (!root) return;
  const cars = selectedTeamCars();
  root.replaceChildren(...Array.from({ length: TEAM_SELECTION_LIMIT }, (_, index) => (
    cars[index] ? createCameraTile(cars[index]) : createEmptyCameraTile(index)
  )));
}

function updateCameraState(car, state) {
  const previous = connectionByCar.get(car.carId);
  const summaryChanged = !previous
    || previous.state !== state.state
    || previous.detail !== state.detail
    || previous.videoActive !== state.videoActive;
  connectionByCar.set(car.carId, state);
  const status = document.getElementById(`camera-status-${car.carId}`);
  if (status) {
    setTextIfChanged(status.lastChild, state.state);
    status.dataset.state = String(state.state || 'waiting').toLowerCase();
  }
  const fps = document.getElementById(`camera-fps-${car.carId}`);
  setTextIfChanged(fps, state.fps > 0 ? `${state.fps.toFixed(1)} FPS` : '-- FPS');
  renderCameraTransportState(car, performance.now());
  if (summaryChanged) {
    renderHeader();
    renderSituations();
    renderTeamSelectionControls();
  }
}

function renderCameraTransportState(car, now) {
  const state = connectionByCar.get(car.carId);
  const videoState = document.getElementById(`video-state-${car.carId}`);
  if (!state || !videoState) return;
  const raceAge = raceTransport ? Math.max(0, now - raceTransport.receivedAt) : null;
  const dataText = state.subscriptionDisabled
    ? 'NOT SUBSCRIBED'
    : `DATA WS ${state.dataOpen ? 'OPEN' : 'CLOSED'}`;
  const raceText = raceAge === null
    ? `RACE WS ${raceStreamOpen ? 'OPEN' : 'WAIT'}`
    : `RACE ${raceTransport.source === 'http' ? 'HTTP' : 'WS'} ${(raceAge / 1000).toFixed(1)}s`;
  setTextIfChanged(videoState, `${state.subscriptionDisabled ? 'VIDEO OFF' : state.state} / ${dataText} / ${raceText}`);
  const stateName = String(state.state || 'waiting').toLowerCase();
  const standing = standingByCar.get(car.carId);
  const activelyRacing = raceState?.phase === 'green' && standing?.status === 'racing';
  const raceTransportLive = raceTransport?.source === 'http'
    ? raceAge <= RACE_STATE_POLL_MS * 3
    : raceStreamOpen;
  const freshness = raceAge === null
    ? 'waiting'
    : !activelyRacing
      ? 'settled'
      : raceTransportLive ? 'live' : 'stale';
  if (videoState.dataset.state !== stateName) videoState.dataset.state = stateName;
  if (videoState.dataset.raceFreshness !== freshness) videoState.dataset.raceFreshness = freshness;
}

function handleRaceState(state, source = 'websocket') {
  raceTransport = {
    source,
    receivedAt: performance.now(),
    sequence: Number.isInteger(state.sequence) ? state.sequence : null,
  };
  for (const car of selectedTeamCars()) renderCameraTransportState(car, performance.now());
  if (!raceDeduplicator.accept(state)) return;
	if (state.roster) applyTeamObserverFleet(state.roster);
  const previousRunId = raceState?.raceRunId || '';
  const nextRunId = state.raceRunId || '';
  if (previousRunId && nextRunId && previousRunId !== nextRunId) {
    pitByCar.clear();
    pitMotionByCar.clear();
    markerMotionByCar.clear();
    markerRenderByCar.clear();
    sectorCompletionHoldByCar.clear();
    seenSectorAchievements.clear();
    clearLeaderboardPositionChanges();
  }
  const previousStandingByCar = new Map((raceState?.standings || []).map((standing) => [standing.carId, standing]));
  for (const standing of state.standings || []) {
    const previous = previousStandingByCar.get(standing.carId);
    const completedLap = previousRunId === nextRunId
      && previous?.currentSector === 3 && standing.currentSector === 1
      && previous.lastMarkerIndex === 2 && standing.lastMarkerIndex === 0;
    if (completedLap) sectorCompletionHoldByCar.set(standing.carId, performance.now() + 2500);
    if (previousRunId === nextRunId && Number.isInteger(previous?.position)
        && Number.isInteger(standing.position) && previous.position !== standing.position) {
      setLeaderboardPositionChange(standing.carId, previous.position, standing.position);
      triggerCarEffect(standing.carId, standing.position < previous.position ? 'position-up' : 'position-down');
    }
    for (const timing of standing.sectorTimes || []) {
      const result = timing?.achievement === 'overall_best'
        ? 'overall-best'
        : timing?.achievement === 'personal_best' ? 'personal-best' : '';
      if (!result || !Number.isInteger(timing.sampleLap) || !Number.isFinite(timing.lastMs)) continue;
      const achievementKey = [
        nextRunId, standing.carId, timing.sector, timing.sampleLap, timing.lastMs, timing.achievement,
      ].join(':');
      if (seenSectorAchievements.has(achievementKey)) continue;
      seenSectorAchievements.add(achievementKey);
      while (seenSectorAchievements.size > 256) {
        seenSectorAchievements.delete(seenSectorAchievements.values().next().value);
      }
      if (previousRunId === nextRunId) triggerCarEffect(standing.carId, result);
    }
  }
  raceState = state;
  raceReceivedAt = performance.now();
  currentLapClocks.ingest(state, raceReceivedAt);
  rebuildRaceViewCache();
  for (const [carId, pit] of pendingPitByCar) {
    if (pit.raceRunId !== nextRunId) continue;
    pendingPitByCar.delete(carId);
    pitByCar.set(carId, pit);
  }
  for (const [carId, pit] of pitByCar) {
    if (pit.raceRunId !== nextRunId) {
      pitByCar.delete(carId);
      pitMotionByCar.delete(carId);
      continue;
    }
    const configuredCar = observerConfig?.cars.find((entry) => entry.carId === carId);
    if (configuredCar && !pitMotionByCar.has(carId)) updatePitMotion(configuredCar, pit, null);
  }
  renderAll();
}

async function pollRaceState(url) {
  if (racePollInFlight) return;
  racePollInFlight = true;
  try {
    const response = await fetch(url, { cache: 'no-store' });
    if (response.status === 204 || !response.ok) return;
    const state = parseRaceState(await response.json());
    if (state) handleRaceState(state, 'http');
  } catch (_) {
    // The dedicated Race WebSocket remains primary; the next poll retries explicit fallback recovery.
  } finally {
    racePollInFlight = false;
  }
}

function startRaceStatePolling(relayHost) {
  if (racePollTimer) return;
  const url = createRaceStateUrl(relayHost);
  void pollRaceState(url);
  racePollTimer = window.setInterval(() => void pollRaceState(url), RACE_STATE_POLL_MS);
}

function stopRaceStatePolling() {
  if (!racePollTimer) return;
  window.clearInterval(racePollTimer);
  racePollTimer = 0;
}

function syncRaceStateFallback(relayHost) {
  if (!raceFallbackEnabled || raceStreamOpen) {
    stopRaceStatePolling();
    return;
  }
  startRaceStatePolling(relayHost);
}

function handleHealth(car, health) {
  const previous = healthByCar.get(car.carId);
	const next = health.fuel === undefined && previous?.fuel !== undefined ? { ...previous, ...health } : health;
	if (previous?.hp === next.hp && previous?.speedCap === next.speedCap && previous?.mode === next.mode
		&& previous?.fuel === next.fuel && previous?.boost === next.boost
		&& previous?.fuelState === next.fuelState && previous?.boostState === next.boostState
		&& previous?.boostRemainingMs === next.boostRemainingMs && previous?.gear === next.gear) return;
  healthByCar.set(car.carId, next);
  if (previous?.boostState !== next.boostState) {
    if (next.boostState === 'ready') triggerCarEffect(car.carId, 'boost-ready');
    if (next.boostState === 'active') triggerCarEffect(car.carId, 'boost-active');
  }
  if (previous?.fuelState !== next.fuelState) {
    if (next.fuelState === 'low') triggerCarEffect(car.carId, 'fuel-low');
    if (next.fuelState === 'empty') triggerCarEffect(car.carId, 'fuel-empty');
  }
  renderCameraHealth(car, next);
  renderLeaderboard();
  renderSituations();
}

function updatePitMotion(car, pit, previous) {
  const currentMotion = pitMotionByCar.get(car.carId);
  const now = performance.now();
  if (pit.present) {
    if (!currentMotion || currentMotion.entryId !== pit.entryId) {
      const rendered = markerRenderByCar.get(car.carId);
      pitMotionByCar.set(car.carId, {
        entryId: pit.entryId,
        phase: rendered ? 'pit-entry' : 'pit-service',
        elapsedMs: 0,
        updatedAt: now,
      });
    }
  } else if (pit.entryId && previous?.present && previous.entryId === pit.entryId) {
    pitMotionByCar.set(car.carId, {
      entryId: pit.entryId,
      phase: 'pit-exit',
      elapsedMs: 0,
      updatedAt: now,
    });
  } else if (pit.entryId && (!currentMotion || currentMotion.entryId !== pit.entryId)
      && pit.exitedAtUnixMs > 0 && Date.now() - pit.exitedAtUnixMs < observerConfig.motion.pitExitMs) {
    pitMotionByCar.set(car.carId, {
      entryId: pit.entryId,
      phase: 'pit-exit',
      elapsedMs: Math.max(0, Date.now() - pit.exitedAtUnixMs),
      updatedAt: now,
    });
  }
}

function handlePit(car, pit) {
  if (pit.carId !== car.carId) return;
  if (!raceState?.raceRunId || pit.raceRunId !== raceState.raceRunId) {
    pendingPitByCar.set(car.carId, pit);
    return;
  }
  pendingPitByCar.delete(car.carId);
  const previous = pitByCar.get(car.carId);
  const unchanged = previous
    && previous.raceRunId === pit.raceRunId
    && previous.present === pit.present
    && previous.entryId === pit.entryId
    && previous.enteredAtUnixMs === pit.enteredAtUnixMs
    && previous.exitedAtUnixMs === pit.exitedAtUnixMs
    && previous.exitReason === pit.exitReason
    && previous.lastAcceptedTick === pit.lastAcceptedTick
    && previous.serviceState === pit.serviceState
    && previous.hp === pit.hp;
  if (unchanged) return;
  pitByCar.set(car.carId, pit);
  if (pit.present && !previous?.present) triggerCarEffect(car.carId, 'pit-service');
  if (pit.present && pit.serviceState === 'complete' && previous?.serviceState !== 'complete') {
    triggerCarEffect(car.carId, 'pit-complete');
  }
  if (raceState?.raceRunId) updatePitMotion(car, pit, previous);
  renderSituations();
  renderPitState();
}

function signed(value, digits = 2) {
  if (!Number.isFinite(value)) return '--';
  return `${value >= 0 ? '+' : ''}${value.toFixed(digits)}`;
}

function renderCameraTelemetry(car, telemetry) {
  const nodes = telemetryNodesByCar.get(car.carId);
  if (!nodes) return;
  const feature = telemetry.motion;
  const motion = feature?.motion;
  const periodUs = feature?.periodUs || telemetry.primary?.periodUs;
  const rateHz = Number.isFinite(periodUs) && periodUs > 0 ? 1000000 / periodUs : null;
  const missing = telemetry.counters?.missing;
  const lateralG = Number.isFinite(motion?.lateralMps2) ? motion.lateralMps2 / 9.80665 : null;
  const forwardG = Number.isFinite(motion?.forwardMps2) ? motion.forwardMps2 / 9.80665 : null;
  setTextIfChanged(nodes.rate, rateHz ? `${rateHz.toFixed(0)}Hz` : '--Hz');
  setTextIfChanged(nodes.loss, Number.isInteger(missing) ? `L${missing}` : 'L--');
  setTextIfChanged(nodes.lateral, `L ${signed(lateralG, 1)}`);
  setTextIfChanged(nodes.forward, `F ${signed(forwardG, 1)}`);
  setTextIfChanged(nodes.yaw, `Y ${signed(motion?.yawRateRadPerSec, 1)}`);
  const scopeX = Number.isFinite(lateralG)
    ? 50 + Math.max(-1, Math.min(1, lateralG / CAMERA_MOTION_SCALE_G)) * 39 : 50;
  const scopeY = Number.isFinite(forwardG)
    ? 50 - Math.max(-1, Math.min(1, forwardG / CAMERA_MOTION_SCALE_G)) * 39 : 50;
  nodes.motionDot.style.left = `${scopeX.toFixed(1)}%`;
  nodes.motionDot.style.top = `${scopeY.toFixed(1)}%`;
  nodes.motionScope.dataset.active = motion ? 'true' : 'false';

  const escStream = telemetry.esc;
  const esc = escStream?.state?.esc;
  const rpm = Number.isInteger(esc?.rpm) ? esc.rpm : null;
  const voltage = Number.isFinite(esc?.v) ? esc.v : null;
  const escTemperature = Number.isFinite(esc?.tc) ? esc.tc : null;
  const motorTemperature = Number.isFinite(esc?.tm) ? esc.tm : null;
  const stale = Boolean(escStream?.stale);
  const speedKph = rpm === null ? null : estimateVehicleSpeedKph(rpm, car.speedProfile);
  const speedAvailable = Number.isFinite(speedKph);
  setTextIfChanged(nodes.rpmLabel, car.speedProfile ? 'EST KM/H' : 'RPM');
  setTextIfChanged(nodes.rpm, speedAvailable
    ? speedKph.toFixed(1)
    : rpm === null ? '--' : rpm.toLocaleString('en-US'));
  setMeterLevel(nodes.rpmFill, speedAvailable
    ? speedKph / CAMERA_SPEED_SCALE_KPH
    : rpm === null ? 0 : rpm / CAMERA_RPM_SCALE);
  setTextIfChanged(nodes.voltage, voltage === null ? '--' : voltage.toFixed(1));
  setTextIfChanged(nodes.escTemp, escTemperature === null ? '--' : escTemperature.toFixed(0));
  setTextIfChanged(nodes.motorTemp, motorTemperature === null ? '--' : motorTemperature.toFixed(0));
  setInstrumentState(nodes.voltageRoot, stale ? 'stale'
    : classifyLowInstrument(voltage, CAMERA_BATTERY_WARNING_V, CAMERA_BATTERY_CRITICAL_V));
  setInstrumentState(nodes.escTempRoot, stale ? 'stale'
    : classifyHighInstrument(escTemperature, CAMERA_ESC_WARNING_C, CAMERA_ESC_CRITICAL_C));
  setInstrumentState(nodes.motorTempRoot, stale ? 'stale'
    : classifyHighInstrument(motorTemperature, CAMERA_MOTOR_WARNING_C, CAMERA_MOTOR_CRITICAL_C));
  nodes.root.dataset.active = motion || esc ? 'true' : 'false';
  nodes.root.dataset.stale = stale ? 'true' : 'false';
  nodes.root.setAttribute(
    'aria-label',
    `Telemetry ${rateHz ? `${rateHz.toFixed(0)} hertz` : 'waiting'}, `
      + `${speedAvailable ? `estimated wheel speed ${speedKph.toFixed(1)} kilometers per hour, ` : ''}`
      + `RPM ${rpm ?? 'waiting'}, battery ${voltage ?? 'waiting'} volts, `
      + `ESC ${escTemperature ?? 'waiting'} degrees, motor ${motorTemperature ?? 'waiting'} degrees`,
  );
}

function handleTelemetry(car, telemetry) {
  telemetryByCar.set(car.carId, telemetry);
}

function handleControl(car, control) {
  controlByCar.set(car.carId, { ...control, receivedAt: performance.now() });
}

function handleVehicleEvent(car, result) {
  vehicleEventHistoryByCar.set(car.carId, result.events || []);
  if (!result.transient || !result.event) return;
  vehicleEventByCar.set(car.carId, result.event);
  triggerCarEffect(car.carId, result.event.impactClass === 'severe'
    ? 'heavy-impact' : result.event.impactClass === 'strong' ? 'impact' : 'gravel');
  renderSituations();
  if (vehicleEventTimers.has(car.carId)) window.clearTimeout(vehicleEventTimers.get(car.carId));
  vehicleEventTimers.set(car.carId, window.setTimeout(() => {
    vehicleEventTimers.delete(car.carId);
    if (vehicleEventByCar.get(car.carId)?.eventId === result.event.eventId) {
      vehicleEventByCar.delete(car.carId);
    }
    renderSituations();
  }, 1900));
}

async function loadConfig() {
  const params = new URLSearchParams(location.search);
  const configUrl = params.get('config') || 'observer-config.json';
  const response = await fetch(configUrl, { cache: 'no-store' });
  if (!response.ok) throw new Error(`observer config HTTP ${response.status}`);
  return normalizeObserverConfig(await response.json());
}

function createRelayHTTPURL(relayHost, pathname) {
	return new URL(pathname, `${location.protocol}//${relayHost}`).toString();
}

async function loadTeamObserverDirectory(relayHost) {
	const response = await fetch(createRelayHTTPURL(relayHost, '/api/v1/team-observer-directory'), { cache: 'no-store' });
	if (response.status === 204 || response.status === 404) return null;
	if (!response.ok) throw new Error(`Team Observer directory HTTP ${response.status}`);
	return normalizeTeamObserverDirectoryProjection(await response.json());
}

async function loadPilotDevices(relayHost) {
	const response = await fetch(createRelayHTTPURL(relayHost, '/api/v1/pilot-devices'), { cache: 'no-store' });
	if (!response.ok) throw new Error(`Relay pilot devices HTTP ${response.status}`);
	return normalizePilotDevicesSnapshot(await response.json());
}

function fleetTopologySignature(cars) {
	return JSON.stringify(cars.map((car) => [
		car.vehicleId, car.sourceId, car.carId, car.flip, car.speedProfileId,
	]));
}

function clearCameraNodeMaps() {
	for (const map of [
		cameraTitleNodesByCar, cameraTileNodesByCar, cameraEffectNodesByCar,
		telemetryNodesByCar, healthNodesByCar, controlNodesByCar,
	]) map.clear();
}

function syncPersistentCarAccents() {
	for (const car of observerConfig?.cars || []) {
		applyCarAccent(markerNodesByCar.get(car.carId)?.marker, car);
		applyCarAccent(cameraTileNodesByCar.get(car.carId), car);
	}
}

function applyTeamObserverFleet(roster = raceState?.roster || null) {
	if (!baseObserverConfig) return false;
	const nextCars = mergeTeamObserverFleet(baseObserverConfig, teamDirectory, pilotDevices, roster);
	if (observerConfig && fleetTopologySignature(observerConfig.cars) === fleetTopologySignature(nextCars)) {
		if (JSON.stringify(observerConfig.cars) === JSON.stringify(nextCars)) return false;
		observerConfig = { ...baseObserverConfig, cars: nextCars };
		syncPersistentCarAccents();
		leaderboardSignature = '';
		sectorRowsSignature = '';
		timingRowsSignature = '';
		renderAll();
		return false;
	}
	const oldCarsByVehicle = new Map((observerConfig?.cars || []).map((car) => [car.vehicleId, car]));
	const selectionTokens = selectedTeamVehicleIds.flatMap((vehicleId) => {
		const previous = oldCarsByVehicle.get(vehicleId);
		return previous?.sourceId ? [vehicleId, previous.sourceId] : [vehicleId];
	});
	for (const client of clientByCar.values()) client.close();
	clientByCar.clear();
	clearCameraNodeMaps();
	observerConfig = { ...baseObserverConfig, cars: nextCars };
	selectedTeamVehicleIds = normalizeTeamVehicleSelection(nextCars, selectionTokens, TEAM_SELECTION_LIMIT);
	connectionByCar.clear();
	for (const car of nextCars) connectionByCar.set(car.carId, {
		state: isTeamCarSelected(car) ? (car.sourceBound ? 'WAITING' : 'UNAVAILABLE') : 'DISABLED',
		detail: isTeamCarSelected(car) ? (car.sourceBound ? 'NOT CONNECTED' : 'SOURCE UNBOUND') : 'NOT SELECTED',
		fps: 0,
		videoActive: false,
		dataOpen: false,
		subscriptionDisabled: !isTeamCarSelected(car),
	});
	createTrackMarkers();
	createCameraTiles();
	leaderboardSignature = '';
	sectorRowsSignature = '';
	timingRowsSignature = '';
	rebuildRaceViewCache();
	renderAll();
	if (teamPeersEnabled) syncSelectedTeamPeers();
	persistTeamSelection();
	return true;
}

async function refreshTeamObserverFleet({ directory = false, devices = true } = {}) {
	if (fleetPollInFlight || document.hidden || !activeRelayHost) return;
	fleetPollInFlight = true;
	try {
		if (directory) {
			try {
				const nextDirectory = await loadTeamObserverDirectory(activeRelayHost);
				teamDirectory = nextDirectory;
			} catch (error) {
				console.warn(error);
			}
		}
		if (devices) {
			try {
				pilotDevices = await loadPilotDevices(activeRelayHost);
			} catch (error) {
				console.warn(error);
			}
		}
		applyTeamObserverFleet();
	} finally {
		fleetPollInFlight = false;
	}
}

function startTeamObserverFleetPolling() {
	if (!pilotDevicesPollTimer) {
		pilotDevicesPollTimer = window.setInterval(
			() => void refreshTeamObserverFleet({ devices: true }),
			PILOT_DEVICES_POLL_MS,
		);
	}
	if (!directoryPollTimer) {
		directoryPollTimer = window.setInterval(
			() => void refreshTeamObserverFleet({ directory: true, devices: false }),
			DIRECTORY_POLL_MS,
		);
	}
}

function stopTeamObserverFleetPolling() {
	if (pilotDevicesPollTimer) window.clearInterval(pilotDevicesPollTimer);
	if (directoryPollTimer) window.clearInterval(directoryPollTimer);
	pilotDevicesPollTimer = 0;
	directoryPollTimer = 0;
}

function updateDisplayClass() {
  const scale = Number.isFinite(window.devicePixelRatio) ? window.devicePixelRatio : 1;
  const physicalWidth = window.innerWidth * scale;
  const physicalHeight = window.innerHeight * scale;
  const scaled4k = physicalWidth >= 3000 && physicalHeight >= 1600 && window.innerWidth < 3000;
  document.documentElement.classList.toggle('scaled-4k', scaled4k);
}

function seedObserverUiTest() {
	const now = Date.now();
	const drivers = ['マッドエックス', '給電セレリタ', 'ノーパイロット', 'テストドライバー'];
	raceState = {
		type: 'race_state', version: 2, raceId: 'race-test', raceRunId: 'rr-ui-test', phase: 'green', flag: 'none',
		serverTimeMs: now, raceTimeMs: 84500, raceInfo: { title: 'FINAL', track: observerConfig.trackName, totalLaps: 10 },
		standings: observerConfig.cars.map((car, index) => ({
			carId: car.carId, driver: drivers[index] || car.driver, position: index + 1, status: 'racing',
			lap: Math.max(1, 6 - Math.floor(index / 20)),
			currentLapMs: 9100 + index * 430, lapTimeMs: 14200 + index * 510, bestLapMs: 13700 + index * 390,
			...(index > 0 ? { intervalToAheadMs: index === 1 ? 0 : 180 + ((index % 11) * 73) } : {}),
			sectorCount: 3, currentSector: (index % 3) + 1, lastMarkerIndex: index % 3,
			lastMarkerRaceMs: 78000 + index * 900, raceElapsedMs: 84500,
			sectorTimes: [1, 2, 3].map((sector) => ({
				sector, lastMs: 4400 + (sector * 240) + (index * 170), bestMs: 4300 + (sector * 220) + (index * 150),
			})),
		})),
		lapHistory: [],
	};
	const gameplayFixtures = [
		{ hp: 100, fuel: 68, fuelState: 'normal', boost: 100, boostState: 'ready', boostRemainingMs: 0, gear: 3 },
		{ hp: 80, fuel: 17, fuelState: 'low', boost: 56, boostState: 'charging', boostRemainingMs: 0, gear: 3 },
		{ hp: 92, fuel: 44, fuelState: 'normal', boost: 72, boostState: 'active', boostRemainingMs: 1800, gear: 4 },
		{ hp: 48, fuel: 0, fuelState: 'empty', boost: 0, boostState: 'charging', boostRemainingMs: 0, gear: 2 },
	];
	observerConfig.cars.forEach((car, index) => {
		const fixtureIndex = index % gameplayFixtures.length;
		const fixture = gameplayFixtures[fixtureIndex];
		healthByCar.set(car.carId, {
			...fixture,
			speedCap: fixture.hp >= 70 ? 0.9 + ((fixture.hp - 70) / 300) : 0.7,
			mode: fixture.hp < 70 ? 'damaged' : 'healthy',
		});
		telemetryByCar.set(car.carId, {
			primary: { periodUs: 33_333 },
			motion: {
				periodUs: 33_333,
				motion: {
					lateralMps2: [3.4, -6.8, 1.1, -2.7][fixtureIndex],
					forwardMps2: [1.8, -2.5, 4.2, -5.8][fixtureIndex],
					yawRateRadPerSec: [0.7, -1.4, 0.3, -0.9][fixtureIndex],
				},
			},
			counters: { missing: [0, 2, 5, 12][fixtureIndex] },
			esc: {
				stale: false,
				state: {
					esc: {
						rpm: [18_420, 31_850, 24_300, 8_900][fixtureIndex],
						v: [7.8, 7.1, 7.5, 6.5][fixtureIndex],
						tc: [42, 68, 77, 88][fixtureIndex],
						tm: [48, 74, 86, 104][fixtureIndex],
					},
				},
			},
		});
		controlByCar.set(car.carId, {
			steering: 0,
			throttle: [0.52, 0.88, 0.36, 0][fixtureIndex],
			brake: [0, 0, 0.28, 0.64][fixtureIndex],
			receivedAt: Number.POSITIVE_INFINITY,
		});
	});
	raceReceivedAt = performance.now();
	currentLapClocks.ingest(raceState, raceReceivedAt);
	rebuildRaceViewCache();
}

function createNextObserverUiTestSnapshot() {
	if (!raceState || UI_TEST_SNAPSHOT_HZ <= 0) return null;
	uiTestSnapshotTick += 1;
	const elapsedMs = Math.round(1000 / UI_TEST_SNAPSHOT_HZ);
	const activeIndex = (uiTestSnapshotTick - 1) % Math.max(1, raceState.standings.length);
	const standings = raceState.standings.map((standing, index) => {
		const raceElapsedMs = (Number(standing.raceElapsedMs) || 0) + elapsedMs;
		if (index !== activeIndex) {
			return {
				...standing,
				currentLapMs: (Number(standing.currentLapMs) || 0) + elapsedMs,
				raceElapsedMs,
			};
		}
		const completedSector = Number.isInteger(standing.currentSector) ? standing.currentSector : 1;
		const currentSector = (completedSector % 3) + 1;
		const sectorTimes = (standing.sectorTimes || []).map((timing) => {
			if (timing.sector !== completedSector) return { ...timing };
			const lastMs = 4300 + (completedSector * 220) + ((index % 17) * 37) + (uiTestSnapshotTick % 23);
			return { ...timing, lastMs, bestMs: Math.min(Number(timing.bestMs) || lastMs, lastMs) };
		});
		return {
			...standing,
			lap: currentSector === 1 ? (Number(standing.lap) || 0) + 1 : standing.lap,
			currentSector,
			lastMarkerIndex: currentSector - 1,
			lastMarkerRaceMs: raceElapsedMs,
			currentLapMs: currentSector === 1 ? 0 : (Number(standing.currentLapMs) || 0) + elapsedMs,
			raceElapsedMs,
			sectorTimes,
		};
	});
	return {
		...raceState,
		sequence: uiTestSnapshotTick,
		serverTimeMs: Date.now(),
		raceTimeMs: (Number(raceState.raceTimeMs) || 0) + elapsedMs,
		standings,
	};
}

function startObserverUiTestSnapshots() {
	if (uiTestSnapshotTimer || UI_TEST_SNAPSHOT_HZ <= 0) return;
	uiTestSnapshotTimer = window.setInterval(() => {
		const state = createNextObserverUiTestSnapshot();
		if (state) handleRaceState(state, 'fixture');
	}, Math.round(1000 / UI_TEST_SNAPSHOT_HZ));
}

function stopObserverUiTestSnapshots() {
	if (!uiTestSnapshotTimer) return;
	window.clearInterval(uiTestSnapshotTimer);
	uiTestSnapshotTimer = 0;
}

function resetUiDiagnostics() {
	overviewRenderSampler.reset();
	leaderboardRenderSampler.reset();
	sectorRenderSampler.reset();
	markerRenderSampler.reset();
	uiLongTaskTracker.reset();
	publishUiDiagnostics();
}

function startObserverUiMetricsWarmupReset() {
	if (!UI_TEST_MODE || !UI_METRICS_ENABLED || uiTestMetricsWarmupTimer) return;
	uiTestMetricsWarmupTimer = window.setTimeout(() => {
		uiTestMetricsWarmupTimer = 0;
		resetUiDiagnostics();
	}, UI_TEST_METRICS_WARMUP_MS);
}

async function initialize() {
  document.documentElement.dataset.mode = 'live';
  updateDisplayClass();
  window.addEventListener('resize', updateDisplayClass);
  try {
		baseObserverConfig = await loadConfig();
		if (UI_TEST_MODE && UI_TEST_CARS > 0) {
			baseObserverConfig = normalizeObserverConfig({
				...baseObserverConfig,
				cars: raceUiPerformance.createObserverCars(UI_TEST_CARS, baseObserverConfig.cars),
			});
		}
    const params = startupParams;
		activeRelayHost = params.get('relayHost') || location.host;
		if (!UI_TEST_MODE) {
			try {
				teamDirectory = await loadTeamObserverDirectory(activeRelayHost);
			} catch (error) {
				console.warn(error);
			}
			try {
				pilotDevices = await loadPilotDevices(activeRelayHost);
			} catch (error) {
				console.warn(error);
			}
		}
		observerConfig = {
			...baseObserverConfig,
			cars: mergeTeamObserverFleet(baseObserverConfig, teamDirectory, pilotDevices, null),
		};
		selectedTeamVehicleIds = initialTeamSelection(params);
    document.getElementById('trackName').textContent = observerConfig.trackName;
    courseLayout.applyToSvg(document.getElementById('trackMap'));
    createTrackMarkers();
    trackGeometry = readTrackGeometry();
    createCameraTiles();
    for (const car of observerConfig.cars) connectionByCar.set(car.carId, {
			state: isTeamCarSelected(car) ? (car.sourceBound ? 'WAITING' : 'UNAVAILABLE') : 'DISABLED',
			detail: isTeamCarSelected(car) ? (car.sourceBound ? 'NOT CONNECTED' : 'SOURCE UNBOUND') : 'NOT SELECTED',
      fps: 0, videoActive: false, dataOpen: false,
      subscriptionDisabled: !isTeamCarSelected(car),
    });
		document.getElementById('teamSelectorOpen')?.addEventListener('click', openTeamSelector);
		document.getElementById('teamSelectorClose')?.addEventListener('click', closeTeamSelector);
		document.getElementById('teamSelectionClear')?.addEventListener('click', () => setTeamSelection([], true));
		document.getElementById('teamSelectorBackdrop')?.addEventListener('click', (event) => {
			if (event.target === event.currentTarget) closeTeamSelector();
		});
		window.addEventListener('keydown', (event) => {
			if (event.key === 'Escape' && !document.getElementById('teamSelectorBackdrop')?.hidden) closeTeamSelector();
		});
		if (UI_TEST_MODE) {
			seedObserverUiTest();
			renderAll();
			publishUiDiagnostics();
			if (params.get('effectTest') === '1') {
				['heavy-impact', 'boost-ready', 'pit-complete', 'overall-best'].forEach((effect, index) => {
					const car = observerConfig.cars[index];
					if (car) triggerCarEffect(car.carId, effect);
				});
			}
			animationFrame = requestAnimationFrame(updateAnimationFrame);
			startObserverUiTestSnapshots();
			startObserverUiMetricsWarmupReset();
			return;
    }
    renderAll();
    teamPeersEnabled = true;
    raceFallbackEnabled = params.get('raceFallback') !== 'off';
    raceClient = new RaceStateStream(activeRelayHost, handleRaceState, (open) => {
      raceStreamOpen = open;
      syncRaceStateFallback(activeRelayHost);
      for (const car of selectedTeamCars()) renderCameraTransportState(car, performance.now());
    });
    raceClient.connect();
    syncRaceStateFallback(activeRelayHost);
    syncSelectedTeamPeers();
		startTeamObserverFleetPolling();
    animationFrame = requestAnimationFrame(updateAnimationFrame);
  } catch (error) {
    document.getElementById('liveStatus').textContent = 'CONFIG ERROR';
    document.getElementById('situationRows').replaceChildren(element('article', 'situation-item situation-limited', error.message || String(error)));
  }
}

window.addEventListener('pagehide', () => {
  if (animationFrame) cancelAnimationFrame(animationFrame);
  uiLongTaskTracker.stop();
  raceFallbackEnabled = false;
	stopTeamObserverFleetPolling();
  raceClient?.close();
  stopRaceStatePolling();
	stopObserverUiTestSnapshots();
	if (uiTestMetricsWarmupTimer) window.clearTimeout(uiTestMetricsWarmupTimer);
	uiTestMetricsWarmupTimer = 0;
  for (const client of clientByCar.values()) client.close();
  clientByCar.clear();
  for (const timer of vehicleEventTimers.values()) window.clearTimeout(timer);
  vehicleEventTimers.clear();
  for (const timer of carEffectTimers.values()) window.clearTimeout(timer);
  carEffectTimers.clear();
  clearLeaderboardPositionChanges();
  window.removeEventListener('resize', updateDisplayClass);
});

function getUiDiagnostics() {
  return {
    metricsEnabled: UI_METRICS_ENABLED,
    fixtureCars: UI_TEST_CARS,
    snapshotRateHz: UI_TEST_SNAPSHOT_HZ,
    snapshotTicks: uiTestSnapshotTick,
    metricsWarmupMs: UI_TEST_MODE ? UI_TEST_METRICS_WARMUP_MS : 0,
    fleetCars: observerConfig?.cars.length || 0,
    selectedCars: selectedTeamVehicleIds.length,
    markerNodes: markerNodesByCar.size,
    videoPeers: clientByCar.size,
    overviewRender: overviewRenderSampler.snapshot(),
    leaderboardRender: leaderboardRenderSampler.snapshot(),
    sectorRender: sectorRenderSampler.snapshot(),
    markerRender: markerRenderSampler.snapshot(),
    longTasks: uiLongTaskTracker.snapshot(),
  };
}

function publishUiDiagnostics() {
  const diagnostics = getUiDiagnostics();
  if (UI_METRICS_ENABLED) document.documentElement.dataset.uiDiagnostics = JSON.stringify(diagnostics);
  return diagnostics;
}

window.momoTeamObserverUi = {
  getDiagnostics: getUiDiagnostics,
  resetDiagnostics: resetUiDiagnostics,
};

if (UI_METRICS_ENABLED) uiLongTaskTracker.start();

initialize();
