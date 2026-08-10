import {
  RaceStateDeduplicator,
  countActiveVideos,
  currentLapClockValue,
  deriveSituations,
  displayRaceStatus,
  classifyBestTime,
  estimateCourseProgress,
  formatDuration,
  formatSplitTime,
  formatStandingGap,
  normalizeLapHistory,
  normalizeObserverConfig,
  parsePitPresence,
  parseRaceState,
  parseVehicleHealth,
  raceClockValue,
  standingsByConfiguredCar,
} from './observer-core.js';

const RECONNECT_BASE_MS = 1000;
const RECONNECT_MAX_MS = 10000;
const COURSE_SECTOR_BOUNDARIES = [0, 0.42277, 0.73115, 1];
const MARKER_RENDER_OFFSETS = [[-10, -10], [10, -10], [-10, 10], [10, 10]];
const raceDeduplicator = new RaceStateDeduplicator();
const healthByCar = new Map();
const telemetryByCar = new Map();
const pitByCar = new Map();
const connectionByCar = new Map();
const clients = [];
const impactTimers = new Map();
const markerMotionByCar = new Map();
let observerConfig = null;
let raceState = null;
let raceReceivedAt = 0;
let animationFrame = 0;

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
  url.searchParams.set('device', device);
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

class ObserverPeer {
  constructor(car, relayHost, video, onState, onRace, onHealth, onPit, onTelemetry) {
    this.car = car;
    this.relayHost = relayHost;
    this.video = video;
    this.onState = onState;
    this.onRace = onRace;
    this.onHealth = onHealth;
    this.onPit = onPit;
    this.onTelemetry = onTelemetry;
    this.ws = null;
    this.pc = null;
    this.pendingCandidates = [];
    this.remoteDescriptionSet = false;
    this.reconnectTimer = 0;
    this.reconnectAttempt = 0;
    this.closed = false;
    this.generation = 0;
    this.frameCount = 0;
    this.frameWindowStartedAt = performance.now();
    this.fps = 0;
    this.videoActive = false;
    this.raceOpen = false;
    this.telemetryOpen = false;
    this.telemetryTracker = window.FpvTelemetry
      ? new window.FpvTelemetry.TelemetryTracker()
      : null;
    this.motionFeatures = window.FpvTelemetry
      ? new window.FpvTelemetry.MotionFeatureExtractor()
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
      if (generation !== this.generation || typeof event.data !== 'string') return;
      const health = parseVehicleHealth(event.data);
      if (health) this.onHealth(this.car, health);
      const pit = parsePitPresence(event.data);
      if (pit) this.onPit(this.car, pit);
      if (!this.telemetryTracker || !event.data.startsWith('TEL:')) return;
      const arrivalMs = performance.now();
      const result = this.telemetryTracker.ingest(event.data, arrivalMs);
      if (!result.accepted) return;
      const motion = this.motionFeatures?.ingest(result.payload, arrivalMs) || null;
      const snapshot = this.telemetryTracker.getSnapshot(arrivalMs);
      this.onTelemetry(this.car, {
        motion,
        primary: snapshot.primary,
        counters: snapshot.counters,
      });
    };
    telemetry.onclose = () => {
      if (generation !== this.generation) return;
      this.telemetryOpen = false;
      this.setState(this.videoActive ? 'STREAMING' : 'CONNECTED', 'TELEMETRY CLOSED');
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
      this.setState(this.videoActive ? 'STREAMING' : 'CONNECTED', 'RACE CLOSED');
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
        this.reconnectAttempt = 0;
        this.setState(this.videoActive ? 'STREAMING' : 'CONNECTED', this.videoActive ? 'VIDEO ACTIVE' : 'PEER CONNECTED');
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
    this.videoActive = false;
    this.raceOpen = false;
    this.telemetryOpen = false;
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
    current.append(currentValue);
    const last = element('span', 'leader-time-last', 'LAST ');
    last.append(element('strong', '', formatDuration(standing?.lapTimeMs)));
    const best = element('span', 'leader-time-best', 'BEST ');
    best.append(element('strong', '', formatDuration(standing?.bestLapMs)));
    times.append(current, last, best);
    row.append(times);
    const healthNode = element('div', 'leader-health');
    const hp = health ? Math.round(health.hp) : null;
    healthNode.dataset.state = hp === null ? 'unknown' : hp > 60 ? 'healthy' : hp > 30 ? 'warning' : 'critical';
    const track = element('i', 'leader-health-track');
    const fill = element('b', 'leader-health-fill');
    fill.style.width = `${hp ?? 0}%`;
    track.append(fill);
    const state = health
      ? (health.speedCap < 0.999 ? `LIMIT ${Math.round(health.speedCap * 100)}%` : health.mode.toUpperCase())
      : standing?.status?.toUpperCase() || connection?.state || 'WAITING';
    healthNode.append(element('span', '', `HP ${hp ?? '--'}`), track, element('strong', '', state));
    row.append(healthNode);
    return row;
  }));
}

function renderSectorRows() {
  const root = document.getElementById('sectorRows');
  const standings = raceState?.standings || [];
  const overallSectorBest = new Map();
  for (const standing of standings) {
    for (const timing of standing.sectorTimes || []) {
      if (!Number.isFinite(timing?.bestMs)) continue;
      const previous = overallSectorBest.get(timing.sector);
      if (previous === undefined || timing.bestMs < previous) overallSectorBest.set(timing.sector, timing.bestMs);
    }
  }
  root.replaceChildren(...standingsByConfiguredCar(observerConfig.cars, raceState).map(({ car, standing }) => {
    const row = element('div', `sector-row car-${car.color}`);
    const currentSector = Number.isInteger(standing?.currentSector) ? standing.currentSector : null;
    row.classList.toggle('is-active', currentSector !== null && standing?.status === 'racing');
    const sectorCount = Number.isInteger(standing?.sectorCount) ? standing.sectorCount : 3;
    const sectorByNumber = new Map((standing?.sectorTimes || []).map((timing) => [timing.sector, timing]));
    const bars = element('div', 'sector-bars');
    for (let sector = 1; sector <= Math.min(3, sectorCount); sector += 1) {
      const timing = sectorByNumber.get(sector);
      const state = currentSector === sector ? 'active' : currentSector && sector < currentSector ? 'done' : '';
      const bar = element('i', state, `S${sector}`);
      if (timing?.lastMs) bar.title = `S${sector} ${formatSplitTime(timing.lastMs)}`;
      bars.append(bar);
    }
    const activeTiming = currentSector ? sectorByNumber.get(currentSector) : null;
    const sectorElapsed = Number.isFinite(standing?.allTimeMs) && Number.isFinite(standing?.lastMarkerRaceMs)
      ? Math.max(0, standing.allTimeMs - standing.lastMarkerRaceMs)
      : null;
    const currentValue = element('span', 'sector-value sector-live timing-cell', formatSplitTime(sectorElapsed));
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
  const history = normalizeLapHistory(raceState);
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
    const sectorTimeByNumber = new Map(entry.sectorTimes.map((timing) => [timing.sector, timing.timeMs]));
    const row = document.createElement('tr');
    const values = [
      formatDuration(entry.completedAtRaceMs),
      car ? `#${car.displayNumber}` : entry.carId,
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
  root.replaceChildren(...observerConfig.cars.map((car) => {
    const marker = svgElement('g', {
      id: `marker-${car.carId}`,
      class: `car-marker car-${car.color}`,
      'aria-label': `${car.carId} estimated course position`,
      hidden: '',
    });
    marker.append(
      svgElement('circle', { class: 'marker-confidence', r: 14 }),
      svgElement('circle', { class: 'marker-core', r: 10 }),
    );
    const number = svgElement('text', { class: 'marker-number', y: 3.5 });
    number.textContent = car.displayNumber;
    const title = svgElement('title');
    title.textContent = `${car.carId} waiting for sector timing`;
    marker.append(number, title);
    return marker;
  }));
}

function markerRaceElapsedMs(carId, standing, now) {
  const running = raceState?.phase === 'green' && standing?.status === 'racing';
  const localAdvance = running && raceReceivedAt ? Math.max(0, now - raceReceivedAt) : 0;
  if (Number.isFinite(standing?.raceElapsedMs)) return standing.raceElapsedMs + localAdvance;
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

function updateTrackMarkers(now) {
  if (!observerConfig || !raceState) return;
  const path = document.getElementById('coursePath');
  if (!path || typeof path.getTotalLength !== 'function') return;
  const length = path.getTotalLength();
  const standingByCar = new Map((raceState.standings || []).map((standing) => [standing.carId, standing]));
  observerConfig.cars.forEach((car, index) => {
    const marker = document.getElementById(`marker-${car.carId}`);
    const standing = standingByCar.get(car.carId);
    const elapsed = standing ? markerRaceElapsedMs(car.carId, standing, now) : null;
    const estimate = standing?.sectorCount === 3
      ? estimateCourseProgress(standing, elapsed, COURSE_SECTOR_BOUNDARIES)
      : null;
    if (!marker || !estimate) {
      marker?.setAttribute('hidden', '');
      return;
    }
    const point = path.getPointAtLength(estimate.courseProgress * length);
    const [offsetX, offsetY] = MARKER_RENDER_OFFSETS[index] || [0, 0];
    marker.removeAttribute('hidden');
    marker.setAttribute('transform', `translate(${(point.x + offsetX).toFixed(2)} ${(point.y + offsetY).toFixed(2)})`);
    marker.dataset.sector = String(estimate.currentSector);
    marker.dataset.progress = estimate.sectorProgress.toFixed(3);
    marker.querySelector('.marker-confidence').setAttribute('r', (14 + (estimate.sectorProgress * 8)).toFixed(1));
    marker.querySelector('title').textContent = `${car.carId} S${estimate.currentSector} ${Math.round(estimate.sectorProgress * 100)}% estimated`;
  });
}

function displayedRaceTime() {
  return raceClockValue(raceState, raceReceivedAt ? performance.now() - raceReceivedAt : 0);
}

function updateClock() {
  document.getElementById('raceClock').textContent = formatDuration(displayedRaceTime());
  const age = raceReceivedAt ? (performance.now() - raceReceivedAt) / 1000 : null;
  const updatedAgo = document.getElementById('updatedAgo');
  updatedAgo.textContent = age === null ? '--' : `${age.toFixed(1)}s`;
  updatedAgo.dataset.freshness = age === null ? 'waiting' : age < 3 ? 'live' : age < 10 ? 'delayed' : 'stale';
  if (raceState && observerConfig) {
    const elapsed = raceReceivedAt ? performance.now() - raceReceivedAt : 0;
    const standingByCar = new Map((raceState.standings || []).map((standing) => [standing.carId, standing]));
    for (const car of observerConfig.cars) {
      const value = formatDuration(currentLapClockValue(standingByCar.get(car.carId), raceState, elapsed));
      for (const node of document.querySelectorAll(`[data-current-lap="${car.carId}"]`)) {
        node.textContent = value;
      }
    }
  }
  updateTrackMarkers(performance.now());
  animationFrame = requestAnimationFrame(updateClock);
}

function renderAll() {
  if (!observerConfig) return;
  renderHeader();
  renderLeaderboard();
  renderSectorRows();
  renderTimingRows();
  renderSituations();
  renderPitState();
}

function createCameraTiles() {
  const root = document.getElementById('cameraGrid');
  root.replaceChildren(...observerConfig.cars.map((car) => {
    const tile = element('article', `camera-tile camera-${car.color}`);
    const head = element('div', 'camera-head');
    const title = element('strong', '', `CAR ${car.displayNumber} `);
    title.append(element('span', '', car.driver || car.device));
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
    const telemetry = element('div', 'camera-telemetry');
    telemetry.id = `camera-telemetry-${car.carId}`;
    telemetry.append(
      element('strong', 'telemetry-rate', 'TEL --'),
      element('span', 'telemetry-lateral', 'LAT -- G'),
      element('span', 'telemetry-forward', 'FWD -- G'),
      element('span', 'telemetry-yaw', 'YAW --'),
      element('span', 'telemetry-loss', 'LOSS --'),
    );
    feed.append(video, telemetry, videoState);
    tile.append(head, feed);
    return tile;
  }));
}

function updateCameraState(car, state) {
  connectionByCar.set(car.carId, state);
  const status = document.getElementById(`camera-status-${car.carId}`);
  if (status) {
    status.lastChild.textContent = state.state;
    status.dataset.state = String(state.state || 'waiting').toLowerCase();
  }
  const fps = document.getElementById(`camera-fps-${car.carId}`);
  if (fps) fps.textContent = state.fps > 0 ? `${state.fps.toFixed(1)} FPS` : '-- FPS';
  const videoState = document.getElementById(`video-state-${car.carId}`);
  if (videoState) {
    videoState.textContent = `${state.state} / RACE ${state.raceOpen ? 'OPEN' : 'CLOSED'} / TEL ${state.telemetryOpen ? 'OPEN' : 'CLOSED'}`;
    videoState.dataset.state = String(state.state || 'waiting').toLowerCase();
  }
  renderAll();
}

function handleRaceState(_car, state) {
  if (!raceDeduplicator.accept(state)) return;
  raceState = state;
  raceReceivedAt = performance.now();
  renderAll();
}

function handleHealth(car, health) {
  healthByCar.set(car.carId, health);
  renderAll();
}

function handlePit(car, pit) {
  if (pit.carId !== car.carId) return;
  pitByCar.set(car.carId, pit);
  renderAll();
}

function signed(value, digits = 2) {
  if (!Number.isFinite(value)) return '--';
  return `${value >= 0 ? '+' : ''}${value.toFixed(digits)}`;
}

function handleTelemetry(car, telemetry) {
  telemetryByCar.set(car.carId, telemetry);
  const root = document.getElementById(`camera-telemetry-${car.carId}`);
  if (!root) return;
  const feature = telemetry.motion;
  const motion = feature?.motion;
  const periodUs = feature?.periodUs || telemetry.primary?.periodUs;
  const rateHz = Number.isFinite(periodUs) && periodUs > 0 ? 1000000 / periodUs : null;
  root.querySelector('.telemetry-rate').textContent = rateHz ? `TEL ${rateHz.toFixed(1)} Hz` : 'TEL --';
  root.querySelector('.telemetry-lateral').textContent = `LAT ${signed(motion?.lateralMps2 / 9.80665)} G`;
  root.querySelector('.telemetry-forward').textContent = `FWD ${signed(motion?.forwardMps2 / 9.80665)} G`;
  root.querySelector('.telemetry-yaw').textContent = `YAW ${signed(motion?.yawRateRadPerSec)} rad/s`;
  root.querySelector('.telemetry-loss').textContent = `LOSS ${telemetry.counters?.missing ?? 0}`;
  root.dataset.active = motion ? 'true' : 'false';
  if (feature?.impact) {
    renderSituations();
    if (impactTimers.has(car.carId)) window.clearTimeout(impactTimers.get(car.carId));
    impactTimers.set(car.carId, window.setTimeout(() => {
      impactTimers.delete(car.carId);
      const current = telemetryByCar.get(car.carId);
      if (current?.motion) {
        telemetryByCar.set(car.carId, {
          ...current,
          motion: { ...current.motion, impactRecent: false, impact: false },
        });
      }
      renderSituations();
    }, 1900));
  }
}

async function loadConfig() {
  const params = new URLSearchParams(location.search);
  const configUrl = params.get('config') || 'observer-config.json';
  const response = await fetch(configUrl, { cache: 'no-store' });
  if (!response.ok) throw new Error(`observer config HTTP ${response.status}`);
  return normalizeObserverConfig(await response.json());
}

async function initialize() {
  document.documentElement.dataset.mode = 'live';
  try {
    observerConfig = await loadConfig();
    document.getElementById('trackName').textContent = observerConfig.trackName;
    createTrackMarkers();
    createCameraTiles();
    for (const car of observerConfig.cars) connectionByCar.set(car.carId, {
      state: 'WAITING', detail: 'NOT CONNECTED', fps: 0, videoActive: false,
      raceOpen: false, telemetryOpen: false,
    });
    renderAll();
    const params = new URLSearchParams(location.search);
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
      );
      clients.push(client);
      client.connect();
    }
    animationFrame = requestAnimationFrame(updateClock);
  } catch (error) {
    document.getElementById('liveStatus').textContent = 'CONFIG ERROR';
    document.getElementById('situationRows').replaceChildren(element('article', 'situation-item situation-limited', error.message || String(error)));
  }
}

window.addEventListener('pagehide', () => {
  if (animationFrame) cancelAnimationFrame(animationFrame);
  for (const client of clients) client.close();
  for (const timer of impactTimers.values()) window.clearTimeout(timer);
  impactTimers.clear();
});

initialize();
