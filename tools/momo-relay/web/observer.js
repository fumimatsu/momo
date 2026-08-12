import {
  abbreviateDriverName,
  RaceStateDeduplicator,
  countActiveVideos,
  currentLapClockValue,
  deriveSituations,
  displayRaceStatus,
  elapsedSinceRaceMarkerMs,
  classifyCompletedSectorTime,
  classifyBestTime,
  estimateLapDurationMs,
  estimateLapPacedProgress,
  formatDuration,
  formatSplitTime,
  formatStandingGap,
  normalizeLapHistory,
  normalizeObserverConfig,
  parseControlCommand,
  parsePitPresence,
  parseRaceState,
	parseVehicleGameplay,
  parseVehicleHealth,
  projectCourseProgress,
  raceClockValue,
  reconstructRaceElapsedMs,
  standingsByConfiguredCar,
} from './observer-core.js';

const RECONNECT_BASE_MS = 1000;
const RECONNECT_MAX_MS = 10000;
const DISCONNECTED_RECONNECT_MS = 3000;
const RACE_STATE_POLL_MS = 500;
const CLOCK_RENDER_INTERVAL_MS = 50;
const MARKER_RENDER_INTERVAL_MS = 1000 / 30;
const TELEMETRY_RENDER_INTERVAL_MS = 100;
const CONTROL_STALE_MS = 250;
const TRANSPORT_RENDER_INTERVAL_MS = 250;
const RACE_FRESHNESS_STALE_MS = 12000;
const COURSE_SECTOR_BOUNDARIES = [0, 0.42277, 0.73115, 1];
const MARKER_RENDER_OFFSETS = [[-16, -16], [16, -16], [-16, 16], [16, 16]];
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
const healthByCar = new Map();
const telemetryByCar = new Map();
const controlByCar = new Map();
const vehicleEventByCar = new Map();
const vehicleEventHistoryByCar = new Map();
const pitByCar = new Map();
const pendingPitByCar = new Map();
const connectionByCar = new Map();
const clients = [];
const vehicleEventTimers = new Map();
const carEffectTimers = new Map();
const activeEffectByCar = new Map();
const markerMotionByCar = new Map();
const markerRenderByCar = new Map();
const markerNodesByCar = new Map();
const currentLapNodeByCar = new Map();
const sectorLiveNodeByCar = new Map();
const sectorCompletionHoldByCar = new Map();
const sectorCompletionNodeByCar = new Map();
const cameraTitleNodesByCar = new Map();
const cameraEffectNodesByCar = new Map();
const leaderboardNodesByCar = new Map();
const telemetryNodesByCar = new Map();
const controlNodesByCar = new Map();
const renderedTelemetryByCar = new Map();
const pitMotionByCar = new Map();
const raceTransportByCar = new Map();
let observerConfig = null;
let raceState = null;
let standingByCar = new Map();
let lapPaceByCar = new Map();
let normalizedLapHistory = [];
let trackGeometry = null;
let raceReceivedAt = 0;
let animationFrame = 0;
let racePollTimer = 0;
let racePollInFlight = false;
let raceStatusRenderedAt = 0;
let clockRenderedAt = 0;
let markerRenderedAt = 0;
let telemetryRenderedAt = 0;

function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function svgElement(tag, attributes = {}) {
  const node = document.createElementNS('http://www.w3.org/2000/svg', tag);
  for (const [name, value] of Object.entries(attributes)) node.setAttribute(name, String(value));
  return node;
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
  return standing?.driver || car.driver || `CAR ${car.displayNumber}`;
}

function createDriverAvatar(car, standing) {
  const avatar = element('span', 'leader-avatar');
  if (car.portraitUrl) {
    const image = document.createElement('img');
    image.src = car.portraitUrl;
    image.alt = `${carName(car, standing)} portrait`;
    avatar.append(image);
  } else {
    avatar.textContent = car.initials;
    avatar.setAttribute('aria-label', `${carName(car, standing)} portrait placeholder`);
  }
  return avatar;
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

class ObserverPeer {
  constructor(car, relayHost, video, onState, onRace, onHealth, onPit, onTelemetry, onControl, onVehicleEvent) {
    this.car = car;
    this.relayHost = relayHost;
    this.video = video;
    this.onState = onState;
    this.onRace = onRace;
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
    this.raceOpen = false;
    this.telemetryOpen = false;
    this.eventsOpen = false;
    this.telemetryTracker = window.FpvTelemetry
      ? new window.FpvTelemetry.TelemetryTracker()
      : null;
    this.motionFeatures = window.FpvTelemetry
      ? new window.FpvTelemetry.MotionFeatureExtractor()
      : null;
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
    ws.onopen = () => this.makeOffer(generation);
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
    const motion = this.motionFeatures?.ingest(result.payload, arrivalMs) || null;
    const snapshot = this.telemetryTracker.getSnapshot(arrivalMs);
    this.onTelemetry(this.car, {
      motion,
      primary: snapshot.primary,
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

    const telemetry = pc.createDataChannel('momo-telemetry', {
      ordered: false,
      maxRetransmits: 0,
    });
    telemetry.onopen = () => {
      if (generation !== this.generation) return;
      this.telemetryOpen = true;
      this.setState(this.videoActive ? 'STREAMING' : 'CONNECTED', 'TELEMETRY OPEN');
    };
    telemetry.onmessage = (event) => {
      if (generation !== this.generation) return;
      this.handleTelemetryMessage(event.data);
    };
    telemetry.onclose = () => {
      if (generation !== this.generation) return;
      this.telemetryOpen = false;
      this.setState(this.videoActive ? 'STREAMING' : 'CONNECTED', 'TELEMETRY CLOSED');
    };

    const command = pc.createDataChannel('momo-command', {
      ordered: false,
      maxRetransmits: 0,
    });
    command.onmessage = (event) => {
      if (generation !== this.generation) return;
      this.handleCommandMessage(event.data);
    };

    const race = pc.createDataChannel('momo-race', { ordered: true });
    race.onopen = () => {
      if (generation !== this.generation) return;
      this.raceOpen = true;
      this.setState(this.videoActive ? 'STREAMING' : 'CONNECTED', 'RACE OPEN');
    };
    race.onmessage = (event) => {
      if (generation !== this.generation || typeof event.data !== 'string') return;
      const state = parseRaceState(event.data);
      if (state) this.onRace(this.car, state);
    };
    race.onclose = () => {
      if (generation !== this.generation) return;
      this.raceOpen = false;
      this.scheduleReconnect(generation, 'RACE CHANNEL CLOSED');
    };
    race.onerror = () => {
      if (generation !== this.generation) return;
      this.setState('RECONNECTING', 'RACE CHANNEL ERROR');
    };

    const events = pc.createDataChannel('momo-events', { ordered: true });
    events.onopen = () => {
      if (generation !== this.generation) return;
      this.eventsOpen = true;
      this.setState(this.videoActive ? 'STREAMING' : 'CONNECTED', 'EVENTS OPEN');
    };
    events.onmessage = (event) => {
      if (generation !== this.generation) return;
      this.handleVehicleEventMessage(event.data);
    };
    events.onclose = () => {
      if (generation !== this.generation) return;
      this.eventsOpen = false;
      this.scheduleReconnect(generation, 'EVENTS CHANNEL CLOSED');
    };

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
      } else if (message.type === 'race-state' && typeof message.data === 'string') {
        const state = parseRaceState(message.data);
        if (state) this.onRace(this.car, state);
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
      raceOpen: this.raceOpen,
      telemetryOpen: this.telemetryOpen,
      eventsOpen: this.eventsOpen,
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
    this.raceOpen = false;
    this.telemetryOpen = false;
    this.eventsOpen = false;
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

function renderLeaderboard() {
  const root = document.getElementById('leaderboardRows');
  const rows = standingsByConfiguredCar(observerConfig.cars, raceState);
  currentLapNodeByCar.clear();
  leaderboardNodesByCar.clear();
  root.replaceChildren(...rows.map(({ car, standing }) => {
    const health = healthByCar.get(car.carId);
    const connection = connectionByCar.get(car.carId);
    const status = ['racing', 'finished', 'retired'].includes(standing?.status) ? standing.status : 'waiting';
    const row = element('article', `leader-row car-${car.color} is-${status}${standing?.position === 1 ? ' is-leader' : ''}`);
    row.append(element('div', 'leader-pos', standing?.position ? `P${standing.position}` : 'P--'), createDriverAvatar(car, standing));
    const driver = element('div', 'leader-driver');
    driver.append(element('strong', '', carName(car, standing)), element('span', 'leader-car', `CAR ${car.displayNumber}`));
    row.append(driver, element('div', 'leader-gap', formatStandingGap(standing)));
    const times = element('div', 'leader-times');
    const current = element('span', 'leader-time-current', 'CURRENT ');
    const currentValue = element('strong', '', formatDuration(standing?.currentLapMs));
    currentValue.dataset.currentLap = car.carId;
    currentLapNodeByCar.set(car.carId, currentValue);
    current.append(currentValue);
    const last = element('span', 'leader-time-last', 'LAST ');
    last.append(element('strong', '', formatDuration(standing?.lapTimeMs)));
    const best = element('span', 'leader-time-best', 'BEST ');
    best.append(element('strong', '', formatDuration(standing?.bestLapMs)));
    times.append(current, last, best);
    row.append(times);
		const resources = element('div', 'leader-resources');
		const appendResource = (label, value, state, text) => {
			const resource = element('div', `leader-resource leader-resource-${label.toLowerCase()}`);
			resource.dataset.state = state;
			const track = element('i', 'leader-resource-track');
			const fill = element('b', 'leader-resource-fill');
			fill.style.width = `${Number.isFinite(value) ? Math.max(0, Math.min(100, value)) : 0}%`;
			track.append(fill);
			resource.append(element('span', '', label), track, element('strong', '', text));
			resources.append(resource);
		};
		const hp = Number.isFinite(health?.hp) ? Math.round(health.hp) : null;
		const fuel = Number.isFinite(health?.fuel) ? Math.round(health.fuel) : null;
		const boost = Number.isFinite(health?.boost) ? Math.round(health.boost) : null;
		appendResource('HP', hp, hp === null ? 'unknown' : health.mode, hp ?? '--');
		appendResource('FUEL', fuel, health?.fuelState || 'unknown', health?.fuelState === 'empty' ? 'EMPTY' : fuel ?? '--');
		appendResource('BOOST', boost, health?.boostState || 'unknown',
			health?.boostState === 'ready' ? 'READY' : health?.boostState === 'active' ? `${(health.boostRemainingMs / 1000).toFixed(1)}s` : boost ?? '--');
		row.append(resources);
    leaderboardNodesByCar.set(car.carId, row);
    applyCarEffect(car.carId);
    return row;
  }));
}

function renderSectorRows() {
  const root = document.getElementById('sectorRows');
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
  root.replaceChildren(...standingsByConfiguredCar(observerConfig.cars, raceState).map(({ car, standing }) => {
    const row = element('div', `sector-row car-${car.color}`);
    const currentSector = Number.isInteger(standing?.currentSector) ? standing.currentSector : null;
    row.classList.toggle('is-active', currentSector !== null && standing?.status === 'racing');
    const sectorCount = Number.isInteger(standing?.sectorCount) ? standing.sectorCount : 3;
    const sectorByNumber = new Map((standing?.sectorTimes || []).map((timing) => [timing.sector, timing]));
    const bars = element('div', 'sector-bars');
    const holdS3 = currentSector === 1 && (sectorCompletionHoldByCar.get(car.carId) || 0) > performance.now();
    for (let sector = 1; sector <= Math.min(3, sectorCount); sector += 1) {
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
      const bar = element('i', [state, resultClass].filter(Boolean).join(' '), `S${sector}`);
      const resultLabel = resultClass === 'overall-best'
        ? ' overall best'
        : resultClass === 'personal-best' ? ' personal best' : '';
      bar.setAttribute('aria-label', `Sector ${sector} ${state || 'upcoming'}${resultLabel}`);
      if (timing?.lastMs) bar.title = `S${sector} ${formatSplitTime(timing.lastMs)}`;
      if (state === 'recent') sectorCompletionNodeByCar.set(car.carId, bar);
      bars.append(bar);
    }
    const activeTiming = currentSector ? sectorByNumber.get(currentSector) : null;
    const now = performance.now();
    const localAdvance = raceReceivedAt ? Math.max(0, now - raceReceivedAt) : 0;
    const currentLapElapsed = currentLapClockValue(standing, raceState, localAdvance);
    const raceElapsed = markerRaceElapsedMs(car.carId, standing, now, currentLapElapsed);
    const sectorElapsed = elapsedSinceRaceMarkerMs(standing, raceElapsed);
    const currentValue = element('span', 'sector-value sector-live timing-cell', formatSplitTime(sectorElapsed));
    sectorLiveNodeByCar.set(car.carId, currentValue);
    const lastValue = element('span', 'sector-value sector-last timing-cell', formatSplitTime(activeTiming?.lastMs));
    const bestValue = element('span', 'sector-value sector-best timing-cell', formatSplitTime(activeTiming?.bestMs));
    const bestClass = classifyBestTime(activeTiming?.lastMs, activeTiming?.bestMs, overallSectorBest.get(currentSector));
    if (bestClass) lastValue.classList.add(bestClass);
    const storedBestClass = classifyBestTime(activeTiming?.bestMs, activeTiming?.bestMs, overallSectorBest.get(currentSector));
    if (storedBestClass) bestValue.classList.add(storedBestClass);
    row.append(
      element('span', 'sector-car', `#${car.displayNumber}`),
      bars,
      currentValue,
      lastValue,
      bestValue,
    );
    return row;
  }));
}

function renderTimingRows() {
  const root = document.getElementById('timingRows');
  const history = normalizedLapHistory;
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
        badge.style.setProperty('--car', `var(--${car?.color || 'green'})`);
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
  panelCount.textContent = `${connected}/${observerConfig.cars.length} VIDEO`;
  panelCount.dataset.state = connected === observerConfig.cars.length ? 'all' : connected > 0 ? 'partial' : 'none';
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
  root.replaceChildren(...observerConfig.cars.map((car) => {
    const marker = svgElement('g', {
      id: `marker-${car.carId}`,
      class: `car-marker car-${car.color}`,
      'aria-label': `${car.carId} estimated course position`,
      hidden: '',
    });
    const confidence = svgElement('circle', { class: 'marker-confidence', r: 23 });
    marker.append(confidence, svgElement('circle', { class: 'marker-core', r: 16 }));
    const label = svgElement('text', { class: 'marker-label', y: 3 });
    label.textContent = car.displayNumber;
    const title = svgElement('title');
    title.textContent = `${car.carId} waiting for sector timing`;
    marker.append(label, title);
    markerNodesByCar.set(car.carId, { marker, confidence, label, title });
    return marker;
  }));
}

function updateTrackMarkerLabel(nodes, car, standing) {
  const driverLabel = abbreviateDriverName(standing?.driver || car.driver, '');
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
  if (!coursePath || !pitPath || typeof coursePath.getTotalLength !== 'function'
      || typeof pitPath.getTotalLength !== 'function') return null;
  return {
    coursePath,
    pitPath,
    courseLength: coursePath.getTotalLength(),
    pitLength: pitPath.getTotalLength(),
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
  if (!Number.isInteger(standing?.lastMarkerRaceMs)) return null;
  const key = `${raceState?.raceRunId || ''}:${standing.lastMarkerIndex}:${standing.lastMarkerRaceMs}`;
  let motion = markerMotionByCar.get(carId);
  if (!motion || motion.key !== key) {
    motion = { key, elapsedMs: 0, updatedAt: now };
    markerMotionByCar.set(carId, motion);
  }
  if (running) motion.elapsedMs += Math.max(0, now - motion.updatedAt);
  motion.updatedAt = now;
  return standing.lastMarkerRaceMs + motion.elapsedMs;
}

function setTextIfChanged(node, value) {
  if (node && node.textContent !== value) node.textContent = value;
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
  const [offsetX, offsetY] = MARKER_RENDER_OFFSETS[index] || [0, 0];
  marker.removeAttribute('hidden');
  marker.setAttribute('transform', `translate(${(rendered.x + offsetX).toFixed(2)} ${(rendered.y + offsetY).toFixed(2)})`);
  if (marker.dataset.motionState !== target.state) marker.dataset.motionState = target.state;
  const radius = String(target.confidenceRadius);
  if (confidence.getAttribute('r') !== radius) confidence.setAttribute('r', radius);
  setTextIfChanged(title, target.title);
}

function updateTrackMarkers(now) {
  if (!observerConfig || !raceState || !trackGeometry) return;
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
    const elapsed = raceReceivedAt ? now - raceReceivedAt : 0;
    for (const car of observerConfig.cars) {
      const standing = standingByCar.get(car.carId);
      const currentLapElapsed = currentLapClockValue(standing, raceState, elapsed);
      const value = formatDuration(currentLapElapsed);
      setTextIfChanged(currentLapNodeByCar.get(car.carId), value);
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
    for (const car of observerConfig.cars) renderCameraTransportState(car, now);
  }
  animationFrame = requestAnimationFrame(updateAnimationFrame);
}

function renderAll() {
  if (!observerConfig) return;
  renderHeader();
  renderCameraTitles();
  renderLeaderboard();
  renderSectorRows();
  renderTimingRows();
  renderSituations();
  renderPitState();
}

function renderCameraTitles() {
  for (const car of observerConfig.cars) {
    const nodes = cameraTitleNodesByCar.get(car.carId);
    if (!nodes) continue;
    const driverName = standingByCar.get(car.carId)?.driver || car.driver || car.device;
    nodes.driver.textContent = driverName;
    nodes.root.title = driverName;
  }
}

function createCameraTiles() {
  const root = document.getElementById('cameraGrid');
  cameraTitleNodesByCar.clear();
  cameraEffectNodesByCar.clear();
  telemetryNodesByCar.clear();
  controlNodesByCar.clear();
  root.replaceChildren(...observerConfig.cars.map((car) => {
    const tile = element('article', `camera-tile camera-${car.color}`);
    const head = element('div', 'camera-head');
    const title = element('strong', '', `CAR ${car.displayNumber} `);
    const driver = element('span', '', car.driver || car.device);
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
    const telemetry = element('div', 'camera-telemetry');
    telemetry.id = `camera-telemetry-${car.carId}`;
    const rate = element('strong', 'telemetry-rate', 'TEL --');
    const lateral = element('span', 'telemetry-lateral', 'LAT -- G');
    const forward = element('span', 'telemetry-forward', 'FWD -- G');
    const yaw = element('span', 'telemetry-yaw', 'YAW --');
    const loss = element('span', 'telemetry-loss', 'LOSS --');
    telemetry.append(rate, lateral, forward, yaw, loss);
    telemetryNodesByCar.set(car.carId, { root: telemetry, rate, lateral, forward, yaw, loss });
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
    feed.append(video, telemetry, controls, videoState, eventFlash);
    tile.append(head, feed);
    cameraEffectNodesByCar.set(car.carId, { root: tile, badge: eventFlash });
    applyCarEffect(car.carId);
    return tile;
  }));
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
  }
}

function renderCameraTransportState(car, now) {
  const state = connectionByCar.get(car.carId);
  const raceTransport = raceTransportByCar.get(car.carId);
  const videoState = document.getElementById(`video-state-${car.carId}`);
  if (!state || !videoState) return;
  const raceAge = raceTransport ? Math.max(0, now - raceTransport.receivedAt) : null;
  const raceText = !state.raceOpen
    ? 'RACE CLOSED'
    : raceAge === null
      ? 'RACE DC WAIT'
      : `RACE DC ${(raceAge / 1000).toFixed(1)}s`;
  setTextIfChanged(videoState, `${state.state} / ${raceText} / TEL ${state.telemetryOpen ? 'OPEN' : 'CLOSED'} / EVT ${state.eventsOpen ? 'OPEN' : 'CLOSED'}`);
  const stateName = String(state.state || 'waiting').toLowerCase();
  const standing = standingByCar.get(car.carId);
  const activelyRacing = raceState?.phase === 'green' && standing?.status === 'racing';
  const freshness = raceAge === null
    ? 'waiting'
    : !activelyRacing
      ? 'settled'
      : raceAge < RACE_FRESHNESS_STALE_MS ? 'live' : 'stale';
  if (videoState.dataset.state !== stateName) videoState.dataset.state = stateName;
  if (videoState.dataset.raceFreshness !== freshness) videoState.dataset.raceFreshness = freshness;
}

function handleRaceState(car, state) {
  if (car) {
    raceTransportByCar.set(car.carId, {
      receivedAt: performance.now(),
      sequence: Number.isInteger(state.sequence) ? state.sequence : null,
    });
    renderCameraTransportState(car, performance.now());
  }
  if (!raceDeduplicator.accept(state)) return;
  const previousRunId = raceState?.raceRunId || '';
  const nextRunId = state.raceRunId || '';
  if (previousRunId && nextRunId && previousRunId !== nextRunId) {
    pitByCar.clear();
    pitMotionByCar.clear();
    markerMotionByCar.clear();
    markerRenderByCar.clear();
    sectorCompletionHoldByCar.clear();
  }
  const previousStandingByCar = new Map((raceState?.standings || []).map((standing) => [standing.carId, standing]));
  const previousOverallSectorBest = new Map();
  for (const standing of raceState?.standings || []) {
    for (const timing of standing.sectorTimes || []) {
      if (!Number.isFinite(timing?.bestMs)) continue;
      const previous = previousOverallSectorBest.get(timing.sector);
      if (previous === undefined || timing.bestMs < previous) previousOverallSectorBest.set(timing.sector, timing.bestMs);
    }
  }
  const overallSectorBest = new Map();
  for (const standing of state.standings || []) {
    for (const timing of standing.sectorTimes || []) {
      if (!Number.isFinite(timing?.bestMs)) continue;
      const previous = overallSectorBest.get(timing.sector);
      if (previous === undefined || timing.bestMs < previous) overallSectorBest.set(timing.sector, timing.bestMs);
    }
  }
  for (const standing of state.standings || []) {
    const previous = previousStandingByCar.get(standing.carId);
    const completedLap = previousRunId === nextRunId
      && previous?.currentSector === 3 && standing.currentSector === 1
      && previous.lastMarkerIndex === 2 && standing.lastMarkerIndex === 0;
    if (completedLap) sectorCompletionHoldByCar.set(standing.carId, performance.now() + 2500);
    if (previousRunId === nextRunId && Number.isInteger(previous?.position)
        && Number.isInteger(standing.position) && previous.position !== standing.position) {
      triggerCarEffect(standing.carId, standing.position < previous.position ? 'position-up' : 'position-down');
    }
    if (previousRunId === nextRunId) {
      const previousSectors = new Map((previous?.sectorTimes || []).map((timing) => [timing.sector, timing]));
      for (const timing of standing.sectorTimes || []) {
        if (!Number.isFinite(timing?.lastMs)
            || previousSectors.get(timing.sector)?.lastMs === timing.lastMs) continue;
        const currentOverall = overallSectorBest.get(timing.sector);
        const previousOverall = previousOverallSectorBest.get(timing.sector);
        const overallReference = currentOverall === undefined
          ? previousOverall
          : previousOverall === undefined ? currentOverall : Math.min(currentOverall, previousOverall);
        const result = classifyCompletedSectorTime(
          timing.lastMs,
          timing.bestMs ?? previousSectors.get(timing.sector)?.bestMs,
          overallReference,
        );
        if (result === 'overall-best' || result === 'personal-best') triggerCarEffect(standing.carId, result);
      }
    }
  }
  raceState = state;
  raceReceivedAt = performance.now();
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
    if (state) handleRaceState(null, state);
  } catch (_) {
    // DataChannel remains the primary path; the next poll retries recovery.
  } finally {
    racePollInFlight = false;
  }
}

function startRaceStatePolling(relayHost) {
  const url = createRaceStateUrl(relayHost);
  void pollRaceState(url);
  racePollTimer = window.setInterval(() => void pollRaceState(url), RACE_STATE_POLL_MS);
}

function handleHealth(car, health) {
  const previous = healthByCar.get(car.carId);
	const next = health.fuel === undefined && previous?.fuel !== undefined ? { ...previous, ...health } : health;
	if (previous?.hp === next.hp && previous?.speedCap === next.speedCap && previous?.mode === next.mode
		&& previous?.fuel === next.fuel && previous?.boost === next.boost
		&& previous?.boostState === next.boostState && previous?.gear === next.gear) return;
  healthByCar.set(car.carId, next);
  if (previous?.boostState !== next.boostState) {
    if (next.boostState === 'ready') triggerCarEffect(car.carId, 'boost-ready');
    if (next.boostState === 'active') triggerCarEffect(car.carId, 'boost-active');
  }
  if (previous?.fuelState !== next.fuelState) {
    if (next.fuelState === 'low') triggerCarEffect(car.carId, 'fuel-low');
    if (next.fuelState === 'empty') triggerCarEffect(car.carId, 'fuel-empty');
  }
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
  setTextIfChanged(nodes.rate, rateHz ? `TEL ${rateHz.toFixed(1)} Hz` : 'TEL --');
  setTextIfChanged(nodes.lateral, `LAT ${signed(motion?.lateralMps2 / 9.80665)} G`);
  setTextIfChanged(nodes.forward, `FWD ${signed(motion?.forwardMps2 / 9.80665)} G`);
  setTextIfChanged(nodes.yaw, `YAW ${signed(motion?.yawRateRadPerSec)} rad/s`);
  setTextIfChanged(nodes.loss, `LOSS ${telemetry.counters?.missing ?? 0}`);
  nodes.root.dataset.active = motion ? 'true' : 'false';
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
			carId: car.carId, driver: drivers[index] || car.driver, position: index + 1, status: 'racing', lap: 6 - index,
			currentLapMs: 9100 + index * 430, lapTimeMs: 14200 + index * 510, bestLapMs: 13700 + index * 390,
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
		const fixture = gameplayFixtures[index] || gameplayFixtures[0];
		healthByCar.set(car.carId, {
			...fixture,
			speedCap: fixture.hp >= 70 ? 0.9 + ((fixture.hp - 70) / 300) : 0.7,
			mode: fixture.hp < 70 ? 'damaged' : 'healthy',
		});
	});
	raceReceivedAt = performance.now();
	rebuildRaceViewCache();
}

async function initialize() {
  document.documentElement.dataset.mode = 'live';
  updateDisplayClass();
  window.addEventListener('resize', updateDisplayClass);
  try {
    observerConfig = await loadConfig();
    document.getElementById('trackName').textContent = observerConfig.trackName;
    createTrackMarkers();
    trackGeometry = readTrackGeometry();
    createCameraTiles();
    for (const car of observerConfig.cars) connectionByCar.set(car.carId, {
      state: 'WAITING', detail: 'NOT CONNECTED', fps: 0, videoActive: false,
      raceOpen: false, telemetryOpen: false, eventsOpen: false,
    });
		const params = new URLSearchParams(location.search);
		if (params.get('uiTest') === '1') {
			seedObserverUiTest();
			renderAll();
			if (params.get('effectTest') === '1') {
				['heavy-impact', 'boost-ready', 'pit-complete', 'overall-best'].forEach((effect, index) => {
					const car = observerConfig.cars[index];
					if (car) triggerCarEffect(car.carId, effect);
				});
			}
			animationFrame = requestAnimationFrame(updateAnimationFrame);
			return;
		}
    renderAll();
    const relayHost = params.get('relayHost') || location.host;
    for (const car of observerConfig.cars) {
      const client = new ObserverPeer(
        car,
        relayHost,
        document.getElementById(`video-${car.carId}`),
        updateCameraState,
        handleRaceState,
        handleHealth,
        handlePit,
        handleTelemetry,
        handleControl,
        handleVehicleEvent,
      );
      clients.push(client);
      client.connect();
    }
    if (params.get('raceFallback') === 'http') startRaceStatePolling(relayHost);
    animationFrame = requestAnimationFrame(updateAnimationFrame);
  } catch (error) {
    document.getElementById('liveStatus').textContent = 'CONFIG ERROR';
    document.getElementById('situationRows').replaceChildren(element('article', 'situation-item situation-limited', error.message || String(error)));
  }
}

window.addEventListener('pagehide', () => {
  if (animationFrame) cancelAnimationFrame(animationFrame);
  if (racePollTimer) window.clearInterval(racePollTimer);
  for (const client of clients) client.close();
  for (const timer of vehicleEventTimers.values()) window.clearTimeout(timer);
  vehicleEventTimers.clear();
  for (const timer of carEffectTimers.values()) window.clearTimeout(timer);
  carEffectTimers.clear();
  window.removeEventListener('resize', updateDisplayClass);
});

initialize();
