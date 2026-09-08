// Loaded only by the explicitly selected UI demo. No network or device access.
export function demoSample(seconds, index = 0) {
  const time = seconds + index * 1.3;
  const phase = time % 7;
  const throttle = phase < 4.8 ? Math.min(1, phase / 0.9, (4.8 - phase) / 0.6) : 0;
  const brake = phase >= 4.8 && phase < 5.6 ? Math.sin((phase - 4.8) / 0.8 * Math.PI) * 0.7 : 0;
  const corner = Math.sin(time * 0.65);
  const boostPhase = time % 14;
  const boostActive = boostPhase > 10 && boostPhase < 12;
  const fuel = 92 - (time * 1.6 % 82);
  const hp = 100 - ((Math.floor(time / 14) + index) % 5) * 8;
  return {
    control: { throttle, brake },
    bend: corner * 0.22,
    health: {
      hp, fuel, fuelState: fuel < 20 ? 'low' : 'normal',
      boost: boostActive ? 100 - (boostPhase - 10) * 50 : Math.min(100, boostPhase * 12),
      boostState: boostActive ? 'active' : boostPhase > 8.4 && boostPhase <= 10 ? 'ready' : 'charging',
      boostRemainingMs: boostActive ? Math.round((12 - boostPhase) * 1000) : 0,
      gear: throttle > 0.8 ? 4 : throttle > 0.3 ? 3 : 2,
      mode: hp < 80 ? 'damaged' : 'healthy', speedCap: hp < 80 ? 0.8 : 1,
    },
    telemetry: {
      primary: { periodUs: 100000 }, counters: { missing: 0 },
      motion: { periodUs: 100000, motion: {
        lateralMps2: corner * (2 + throttle * 4),
        forwardMps2: throttle * 3.5 - brake * 8,
        yawRateRadPerSec: corner * 1.2,
      } },
      esc: { stale: false, state: { esc: {
        rpm: Math.round(5000 + throttle * 15500 + Math.sin(time * 1.4) * 1600),
        v: 7.9 - throttle * 0.55 - index * 0.12 + Math.sin(time * 0.3) * 0.08,
        tc: 46 + index * 5 + Math.sin(time * 0.17) * 12,
        tm: 56 + index * 7 + Math.sin(time * 0.13) * 16,
      } } },
    },
  };
}

function polygon(ctx, color, points) {
  ctx.fillStyle = color;
  ctx.beginPath();
  points.forEach(([x, y], i) => i ? ctx.lineTo(x, y) : ctx.moveTo(x, y));
  ctx.closePath(); ctx.fill();
}

function drawFrame(ctx, seconds, index, sample, color) {
  const w = 640, h = 360, horizon = 140;
  ctx.fillStyle = '#202d37'; ctx.fillRect(0, 0, w, h);
  const ceiling = ctx.createLinearGradient(0, 0, 0, horizon);
  ceiling.addColorStop(0, '#16232e'); ceiling.addColorStop(1, '#657985');
  ctx.fillStyle = ceiling; ctx.fillRect(0, 0, w, horizon);
  ctx.strokeStyle = '#a0b8c3'; ctx.lineWidth = 2;
  for (let i = -4; i <= 4; i++) {
    ctx.beginPath(); ctx.moveTo(320 + i * 110, 0); ctx.lineTo(320 + i * 28, horizon); ctx.stroke();
  }
  ctx.fillStyle = '#d0e9ed';
  for (let i = 0; i < 3; i++) ctx.fillRect(70 + i * 190, 42, 86, 4);
  ctx.fillStyle = '#72848b'; ctx.fillRect(0, 100, w, 40);
  ctx.fillStyle = '#263c4b'; ctx.fillRect(235, 108, 172, 24);
  ctx.fillStyle = '#d8ebf1'; ctx.font = 'bold 12px sans-serif'; ctx.fillText('SDKRACING  /  TEST CIRCUIT', 246, 124);
  ctx.fillStyle = '#778181'; ctx.fillRect(0, horizon, w, h - horizon);
  const travel = seconds * 13 + index * 8;
  const project = (z, offset = 0, elevation = 0) => [
    w / 2 + (sample.bend * z * z / (18 + z) + offset) * 170 / z,
    horizon + (190 - elevation * 170) / z,
  ];
  for (let z = 65; z >= 1; z -= 0.75) {
    const near = Math.max(0.7, z - 0.75);
    const alternate = Math.floor((z + travel) / 2) % 2;
    polygon(ctx, alternate ? '#303d42' : '#334147', [project(z, -2.5), project(z, 2.5), project(near, 2.5), project(near, -2.5)]);
    for (const side of [-1, 1]) {
      polygon(ctx, alternate ? '#c86053' : '#dde2dc', [project(z, side * 2.5), project(z, side * 2.85), project(near, side * 2.85), project(near, side * 2.5)]);
      polygon(ctx, alternate ? '#e3e7e5' : '#455d6a', [project(z, side * 3.1), project(z, side * 3.1, 0.35), project(near, side * 3.1, 0.35), project(near, side * 3.1)]);
    }
    if (Math.floor((z + travel) / 3) % 2 === 0) {
      polygon(ctx, '#a8b6b2', [project(z, -0.025), project(z, 0.025), project(near, 0.025), project(near, -0.025)]);
    }
  }
  // A small car ahead and cones provide visible depth and motion cues.
  const aheadZ = 8 + Math.sin(seconds * 0.4 + index) * 2;
  const [ax, ay] = project(aheadZ, Math.sin(seconds * 0.6) * 0.6);
  ctx.fillStyle = '#141c22'; ctx.fillRect(ax - 21, ay - 11, 42, 14);
  ctx.fillStyle = ['#e2b65d', '#56c8de', '#ed6b74', '#75e36a'][index % 4];
  ctx.fillRect(ax - 16, ay - 18, 32, 14);
  ctx.fillStyle = '#f06051'; ctx.fillRect(ax - 13, ay - 8, 5, 3); ctx.fillRect(ax + 8, ay - 8, 5, 3);
  for (let i = 5; i >= 0; i--) {
    const z = 2 + ((i * 8 + 48 - travel % 48) % 48);
    const [x, y] = project(z, 3.45);
    const size = 95 / z;
    polygon(ctx, '#eaa363', [[x - size * 0.55, y], [x, y - size * 1.7], [x + size * 0.55, y]]);
    ctx.fillStyle = '#f1eee2'; ctx.fillRect(x - size * 0.28, y - size * 0.7, size * 0.56, size * 0.18);
  }
  polygon(ctx, '#14242c', [[155, 360], [217, 337], [423, 337], [485, 360]]);
  polygon(ctx, color, [[241, 360], [266, 338], [291, 338], [284, 360]]);
  ctx.fillStyle = 'rgba(10, 20, 28, .78)'; ctx.fillRect(12, 12, 167, 25);
  ctx.fillStyle = '#f0c54a'; ctx.font = 'bold 12px sans-serif'; ctx.fillText('DEMO  /  SYNTHETIC FPV', 21, 29);
  ctx.fillStyle = '#ebf1f2'; ctx.font = '12px monospace';
  ctx.fillText(`CAM ${index + 1}   ${seconds.toFixed(1)}s`, 14, 345);
}

export function createObserverDemo({ getVideo, onSample, onState, ScreenStats }) {
  const streams = new Map();
  let lastFrameAt = -Infinity, lastDataAt = -Infinity, lastStatsAt = -Infinity, closed = false;
  const startedAt = performance.now();
  function remove(id) {
    const entry = streams.get(id);
    if (!entry) return;
    entry.closed = true;
    if (entry.frameCallback !== null) entry.video.cancelVideoFrameCallback?.(entry.frameCallback);
    entry.stream.getTracks().forEach(track => track.stop());
    if (entry.video.srcObject === entry.stream) entry.video.srcObject = null;
    streams.delete(id);
  }
  function add(car) {
    const video = getVideo(car);
    if (!video) return null;
    const canvas = document.createElement('canvas'); canvas.width = 640; canvas.height = 360;
    const ctx = canvas.getContext('2d', { alpha: false });
    if (!ctx || typeof canvas.captureStream !== 'function') throw new Error('Canvas video capture is required for the Observer demo.');
    const stream = canvas.captureStream(0);
    const entry = { video, canvas, ctx, stream, track: stream.getVideoTracks()[0], closed: false,
      stats: new ScreenStats(video), frameCallback: null, frames: 0 };
    if (typeof entry.track?.requestFrame !== 'function') {
      stream.getTracks().forEach(track => track.stop());
      throw new Error('Canvas video requestFrame is required for the Observer demo.');
    }
    video.classList.remove('video-flipped');
    video.srcObject = stream;
    video.play().catch(() => {
      entry.error = 'PLAYBACK BLOCKED';
      if (!entry.closed) onState(car, { state: 'DEMO ERROR', detail: entry.error, videoActive: false });
    });
    const count = () => {
      if (entry.closed) return;
      entry.frames++; entry.stats.noteFrame();
      entry.frameCallback = video.requestVideoFrameCallback(count);
    };
    if (video.requestVideoFrameCallback) entry.frameCallback = video.requestVideoFrameCallback(count);
    streams.set(car.carId, entry);
    return entry;
  }
  return {
    tick(now, selected, cars) {
      if (closed || document.hidden) return;
      const ids = new Set(selected.slice(0, 4).map(car => car.carId));
      for (const id of streams.keys()) if (!ids.has(id)) remove(id);
      const draw = now - lastFrameAt >= 1000 / 30;
      const data = now - lastDataAt >= 100;
      const stats = now - lastStatsAt >= 1000;
      for (const car of selected.slice(0, 4)) {
        const entry = streams.get(car.carId) || add(car);
        if (!entry) continue;
        const index = cars.findIndex(candidate => candidate.carId === car.carId);
        const seconds = Math.max(0, now - startedAt) / 1000;
        const sample = demoSample(seconds, index);
        if (draw) { drawFrame(entry.ctx, seconds, index, sample, car.color || '#56c8de'); entry.track.requestFrame(); }
        if (data) onSample(car, sample, now);
        if (stats) {
          const window = entry.stats.takeWindow();
          onState(car, { state: entry.error ? 'DEMO ERROR' : 'DEMO', detail: entry.error || 'SYNTHETIC VIDEO + TELEMETRY',
            fps: window?.renderCallbackFps, videoActive: !entry.error && entry.frames > 0, dataOpen: false });
        }
      }
      if (draw) lastFrameAt = now;
      if (data) lastDataAt = now;
      if (stats) lastStatsAt = now;
    },
    close() { closed = true; for (const id of streams.keys()) remove(id); },
    streamCount() { return streams.size; },
  };
}
