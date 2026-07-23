(() => {
  'use strict';

  const DEFAULT_HOST = '192.168.11.3:8080';
  const RECONNECT_BASE_DELAY_MS = 500;
  const RECONNECT_MAX_DELAY_MS = 5000;
  const ROOM_FULL_RETRY_BASE_DELAY_MS = getNumberParam('roomFullRetryMs', 10000);
  const ROOM_FULL_RETRY_MAX_DELAY_MS = getNumberParam('roomFullRetryMaxMs', 30000);
  const VIDEO_FREEZE_TIMEOUT_MS = getNumberParam('videoFreezeMs', 12000);
  const CONNECT_GRACE_MS = getNumberParam('connectGraceMs', 15000);
  const SIGNALING_MODE = getStringParam(['signaling', 'signalingMode'], 'relay').toLowerCase();
  // signaling=ayame でも Relay が相手なら、Pi 直結用 serial ではなく Relay の
  // command / telemetry / race 契約を使う。外部 Pilot URL は relayTransport=1 を指定する。
  const RELAY_TRANSPORT = SIGNALING_MODE === 'relay' || getBooleanParam('relayTransport', false);
  const AUTO_RECONNECT = getBooleanParam('autoReconnect', true);
  const AUTO_RECONNECT_ON_VIDEO_LOST = getBooleanParam('videoReconnect', SIGNALING_MODE === 'ayame');
  const RACE_CAR_ID = getStringParam(['carId', 'raceCarId'], '');
  const DEVICE_STATUS_MODE = getDeviceStatusMode();
  const CONTROL_UI_MODE = normalizeControlUiMode(getStringParam(['controlUi'], 'auto'));
  const RC_TX_INTERVAL_MS = getNumberParam('rcTxMs', 20);
  const RC_STEERING_THROW = getNumberParam('rcSteeringThrow', 400);
  const RC_THROTTLE_THROW = getNumberParam('rcThrottleThrow', 300);
  const RC_THROTTLE_MIN = getNumberParam('rcThrottleMin', 1300);
  const RC_BRAKE_VALUE = getNumberParam('rcBrakeValue', 1300);
  const RC_BRAKE_DURATION_MS = getNumberParam('rcBrakeMs', 1000);
  const RC_BRAKE_THRESHOLD = getNumberParam('rcBrakeThreshold', 1700);
  const RC_THROTTLE_GEAR_MIN_VALUES = [1200, 1200, 1000, 1000, 1000];
  const RC_THROTTLE_GEAR_MAX_VALUES = [1600, 1700, 1800, 1900, 2000];
  const RC_GEAR_COUNT = Math.max(1, Math.min(5, getIntegerParam('rcGearCount', 5)));
  const RC_INITIAL_GEAR = Math.max(1, Math.min(RC_GEAR_COUNT, getIntegerParam('rcGear', 1)));
  const RC_STEERING_NEUTRAL_DEADBAND_US = getNumberParamAllowZero('rcSteeringNeutralDeadband', 10);
  const RC_THROTTLE_NEUTRAL_DEADBAND_US = getNumberParamAllowZero('rcThrottleNeutralDeadband', 10);
  const GAMEPAD_PROFILE_STORAGE_KEY_LEGACY = 'fpvGamepadMapping';
  const GAMEPAD_PROFILE_STORAGE_KEY = getGamepadProfileStorageKey();
  const GAMEPAD_PROFILE = loadGamepadProfile();
  const GAMEPAD_ENABLED = getBooleanParam('gamepad', true);
  const GAMEPAD_INDEX = getNumberParamWithProfile('gamepadIndex', 'index', 0, true);
  const GAMEPAD_STEERING_AXIS = getNumberParamWithProfile('gamepadSteeringAxis', 'steeringAxis', 0, true);
  const GAMEPAD_STEERING_INVERT = getBooleanParamWithProfile('gamepadSteeringInvert', 'steeringInvert', false);
  const GAMEPAD_STEERING_DEADZONE = getNumberParamWithProfile('gamepadSteeringDeadzone', 'steeringDeadzone', 0.03);
  const GAMEPAD_STEERING_CENTER = getNumberParamWithProfile('gamepadSteeringCenter', 'steeringCenter', 0);
  const GAMEPAD_STEERING_LEFT = getNumberParamWithProfile('gamepadSteeringLeft', 'steeringLeft', -1);
  const GAMEPAD_STEERING_RIGHT = getNumberParamWithProfile('gamepadSteeringRight', 'steeringRight', 1);
  const GAMEPAD_STEERING_CALIBRATED =
    hasNumberParamWithProfile('gamepadSteeringCenter', 'steeringCenter') &&
    hasNumberParamWithProfile('gamepadSteeringLeft', 'steeringLeft') &&
    hasNumberParamWithProfile('gamepadSteeringRight', 'steeringRight');
  const GAMEPAD_STEERING_GAIN = getEffectiveSteeringGain(
    getNumberParamWithProfile('gamepadSteeringGain', 'steeringGain', GAMEPAD_STEERING_CALIBRATED ? 1.0 : 3.75),
    GAMEPAD_STEERING_CALIBRATED,
  );
  const GAMEPAD_THROTTLE_AXIS = getNumberParamWithProfile('gamepadThrottleAxis', 'throttleAxis', 5, true);
  const GAMEPAD_THROTTLE_INVERT = getBooleanParamWithProfile('gamepadThrottleInvert', 'throttleInvert', false);
  const GAMEPAD_THROTTLE_IDLE = getNumberParamWithProfile('gamepadThrottleIdle', 'throttleIdle', 1);
  const GAMEPAD_THROTTLE_PRESSED = getNumberParamWithProfile('gamepadThrottlePressed', 'throttlePressed', -1);
  const GAMEPAD_THROTTLE_IDLE_CONFIGURED = hasNumberParamWithProfile('gamepadThrottleIdle', 'throttleIdle');
  const GAMEPAD_BRAKE_AXIS = getNumberParamWithProfile('gamepadBrakeAxis', 'brakeAxis', 6, true);
  const GAMEPAD_BRAKE_INVERT = getBooleanParamWithProfile('gamepadBrakeInvert', 'brakeInvert', false);
  const GAMEPAD_BRAKE_IDLE = getNumberParamWithProfile('gamepadBrakeIdle', 'brakeIdle', 1);
  const GAMEPAD_BRAKE_PRESSED = getNumberParamWithProfile('gamepadBrakePressed', 'brakePressed', -1);
  const GAMEPAD_BRAKE_IDLE_CONFIGURED = hasNumberParamWithProfile('gamepadBrakeIdle', 'brakeIdle');
  const GAMEPAD_PEDAL_DEADZONE = getNumberParamWithProfile('gamepadPedalDeadzone', 'pedalDeadzone', 0.05);
  const GAMEPAD_DRIVE_BUTTON = getNumberParamWithProfile('gamepadDriveButton', 'driveButton', 8, true);
  const GAMEPAD_DRIVE_BUTTON_ENABLED = getBooleanParam('gamepadDriveButtonEnabled', true);
  const GAMEPAD_PADDLE_LEFT_BUTTON = getNumberParamWithProfile('gamepadPaddleLeftButton', 'paddleLeftButton', 0, true);
  const GAMEPAD_PADDLE_RIGHT_BUTTON = getNumberParamWithProfile('gamepadPaddleRightButton', 'paddleRightButton', 1, true);
  const GAMEPAD_FFB_PRESET_BUTTON = getNumberParamWithProfile('gamepadFfbPresetButton', 'ffbPresetButton', -1, true);
  const OSD_UPDATE_INTERVAL_MS = getNumberParam('osdMs', 100);
  const DC_PING_ENABLED = getBooleanParam('dcPing', false);
  const DC_PING_INTERVAL_MS = getNumberParam('dcPingMs', 1000);
  // ffbTest は過去の検証 URL 向けの互換名。通常は gamepad.html の ffbEnabled を使う。
  const FFB_ENABLED = getBooleanParamWithProfile('ffbEnabled', 'ffbEnabled', getBooleanParam('ffbTest', false));
  const FFB_BRIDGE_URL = getStringParam('ffbUrl', GAMEPAD_PROFILE?.ffbBridgeUrl || 'ws://127.0.0.1:24725');
  const FFB_BASE_FRICTION = Math.max(0, Math.min(1.0, getNumberParamWithProfile('ffbBaseFriction', 'ffbBaseFriction', 0.28)));
  const FFB_PARKING_FRICTION = Math.max(0, Math.min(1.0, getNumberParamWithProfile('ffbParkingFriction', 'ffbParkingFriction', 0.08)));
  const FFB_BASE_DAMPER = Math.max(0, Math.min(1.0, getNumberParamWithProfile('ffbBaseDamper', 'ffbBaseDamper', 0.05)));
  const FFB_SPEED_DAMPER = Math.max(0, Math.min(1.0, getNumberParamWithProfile('ffbSpeedDamper', 'ffbSpeedDamper', 0.15)));
  const FFB_PRESETS = Object.freeze({
    weak: Object.freeze({ scale: 0.65, label: 'Weak' }),
    medium: Object.freeze({ scale: 1.00, label: 'Medium' }),
    strong: Object.freeze({ scale: 1.35, label: 'Strong' }),
  });
  const FFB_INITIAL_PRESET = normalizeFfbPreset(getStringParam('ffbPreset', GAMEPAD_PROFILE?.ffbPreset || 'medium'));
  const FFB_SEND_INTERVAL_MS = Math.max(20, Math.min(100, getNumberParam('ffbSendMs', 20)));
  const FFB_RECONNECT_DELAY_MS = 2000;
  const FFB_SPEED_PROXY_ACCEL_PER_SEC = 0.55;
  const FFB_SPEED_PROXY_COAST_DECEL_PER_SEC = 0.22;
  const FFB_SPEED_PROXY_BRAKE_DECEL_PER_SEC = 1.10;
  const AYAME_SIGNALING_URL = getStringParam(
    ['ayameUrl', 'signalingUrl'],
    'wss://ayame-labo.shiguredo.app/signaling',
  );
  const AYAME_ROOM_ID = getStringParam(['roomId', 'ayameRoomId'], '');
  const AYAME_CLIENT_ID = getAyameClientId();
  const AYAME_SIGNALING_KEY = getStringParam(['signalingKey', 'ayameKey'], '');
  const AUTO_START = getBooleanParam('autoStart', SIGNALING_MODE !== 'ayame');
  const ICE_MODE = normalizeIceMode(getStringParam(['iceMode', 'ice'], 'auto'));
  const STUN_URLS = getStringListParam(['stunUrls', 'stunUrl'], ['stun:stun.l.google.com:19302']);
  const TURN_URLS = getStringListParam(['turnUrls', 'turnUrl'], []);
  const TURN_USERNAME = getStringParam(['turnUsername', 'turnUser'], '');
  const TURN_CREDENTIAL = getStringParam(['turnCredential', 'turnPassword'], '');
  const AUDIO_FILTER_DEFAULT = getBooleanParam('audioFilter', false);
  const AUDIO_FILTER_Q = getNumberParam('audioFilterQ', 24);
  const AUDIO_FILTER_FREQS = getStringListParam(['audioFilterFreqs'], ['50', '100', '150'])
    .map((value) => Number(value))
    .filter((value) => Number.isFinite(value) && value > 0);
  const MEDIA_CONTROLS_VISIBLE = getBooleanParam(
    'audioControls',
    getBooleanParam('mediaControls', true),
  );
  const MIC_DEFAULT_VOLUME = Math.max(0, Math.min(200, getNumberParamAllowZero('micVolume', 100)));
  const MIC_METER_INTERVAL_MS = 100;
  const ROOM_LOCK_ENABLED = getBooleanParam('roomLock', SIGNALING_MODE === 'ayame');
  const ROOM_LOCK_URL = normalizeBaseUrl(getStringParam(['lockUrl', 'roomLockUrl'], defaultRoomLockUrl()));
  const ROOM_LOCK_TTL_SEC = getNumberParam('roomLockTtl', 30);
  const ROOM_LOCK_POLL_MS = getNumberParam('roomLockPollMs', 5000);
  const ROOM_LOCK_HEARTBEAT_MAX_FAILURES = Math.max(1, getIntegerParam('roomLockHeartbeatFailures', 3));
  const RACE_START_SIGNAL_LIGHT_COUNT = 5;
  const RACE_START_SIGNAL_GREEN_MS = getNumberParam('raceSignalMs', 4000);
  const RACE_BATTLE_ENABLED = getBooleanParam('raceBattle', true);
  const RACE_BATTLE_DEMO = getBooleanParam('raceBattleDemo', false);
  const RACE_BATTLE_MAX_GAP_MS = 5000;
  const RACE_BATTLE_GAP_STEP_MS = 100;
  const RACE_BATTLE_MIN_OFFSET_PX = 30;
  const RACE_BATTLE_MAX_OFFSET_PX = 80;
  const RACE_ANNOUNCE_ENABLED = getBooleanParam('raceAnnounce', false);
  const RACE_ANNOUNCE_LANGUAGE = getStringParam('raceAnnounceLang', 'ja-JP');
  const RACE_ANNOUNCE_VOICE = getStringParam('raceAnnounceVoice', '');
  const RACE_ANNOUNCE_RATE = Math.max(0.5, Math.min(2.5, getNumberParam('raceAnnounceRate', 1.1)));
  const RACE_ANNOUNCE_VOLUME = Math.max(0, Math.min(1, getNumberParamAllowZero('raceAnnounceVolume', 0.9)));

  const remoteVideo = document.getElementById('remote_video');
  const endpointInput = document.getElementById('endpoint');
  const dataTextInput = document.getElementById('data_text');
  const steeringInput = document.getElementById('steering');
  const throttleInput = document.getElementById('throttle');
  const steeringValue = document.getElementById('steeringValue');
  const throttleValue = document.getElementById('throttleValue');
  const wsState = document.getElementById('wsState');
  const iceState = document.getElementById('iceState');
  const dcState = document.getElementById('dcState');
  const hostState = document.getElementById('hostState');
  const timeState = document.getElementById('timeState');
  const linkState = document.getElementById('linkState');
  const videoState = document.getElementById('videoState');
  const fpsState = document.getElementById('fpsState');
  const renderFpsState = document.getElementById('renderFpsState');
  const netState = document.getElementById('netState');
  const jitterState = document.getElementById('jitterState');
  const rttState = document.getElementById('rttState');
  const dcRttState = document.getElementById('dcRttState');
  const latencyState = document.getElementById('latencyState');
  const dropState = document.getElementById('dropState');
  const uptimeState = document.getElementById('uptimeState');
  const retryState = document.getElementById('retryState');
  const lastEventState = document.getElementById('lastEventState');
  const diagState = document.getElementById('diagState');
  const videoAgeState = document.getElementById('videoAgeState');
  const rcState = document.getElementById('rcState');
  const telemetryState = document.getElementById('telemetryState');
  const modeState = document.getElementById('modeState');
  const deviceState = document.getElementById('deviceState');
  const racePhase = document.getElementById('racePhase');
  const raceStartSignal = document.getElementById('raceStartSignal');
  const raceStartSignalLights = Array.from(document.querySelectorAll('[data-race-signal-light]'));
  const raceLapCount = document.getElementById('raceLapCount');
  const raceCurrentLap = document.getElementById('raceCurrentLap');
  const raceLastLap = document.getElementById('raceLastLap');
  const raceBestLap = document.getElementById('raceBestLap');
  const raceTotalTime = document.getElementById('raceTotalTime');
  const racePosition = document.getElementById('racePosition');
  const raceBattle = document.getElementById('raceBattle');
  const raceBattleState = document.getElementById('raceBattleState');
  const raceBattleAhead = document.getElementById('raceBattleAhead');
  const raceBattleAheadPosition = document.getElementById('raceBattleAheadPosition');
  const raceBattleAheadName = document.getElementById('raceBattleAheadName');
  const raceBattleAheadGap = document.getElementById('raceBattleAheadGap');
  const raceBattleSelfPosition = document.getElementById('raceBattleSelfPosition');
  const raceBattleSelfName = document.getElementById('raceBattleSelfName');
  const raceBattleBehind = document.getElementById('raceBattleBehind');
  const raceBattleBehindPosition = document.getElementById('raceBattleBehindPosition');
  const raceBattleBehindName = document.getElementById('raceBattleBehindName');
  const raceBattleBehindGap = document.getElementById('raceBattleBehindGap');
  const raceLapHistory = document.getElementById('raceLapHistory');
  const btnReconnect = document.getElementById('btnReconnect');
  const btnFullscreen = document.getElementById('btnFullscreen');
  const btnFlip = document.getElementById('btnFlip');
  const btnMirror = document.getElementById('btnMirror');
  const btnAudio = document.getElementById('btnAudio');
  const btnAudioFilter = document.getElementById('btnAudioFilter');
  const btnMic = document.getElementById('btnMic');
  const micControl = btnMic?.closest('.mic-control');
  const micVolumeInput = document.getElementById('micVolume');
  const micMeter = document.getElementById('micMeter');
  const btnDebug = document.getElementById('btnDebug');
  const modeSelect = document.getElementById('modeSelect');
  const btnApplyMode = document.getElementById('btnApplyMode');
  const btnRefreshMode = document.getElementById('btnRefreshMode');
  const btnRefreshDevice = document.getElementById('btnRefreshDevice');
  const btnInputSetup = document.getElementById('btnInputSetup');
  const ffbPresetControls = document.getElementById('ffbPresetControls');
  const ffbPresetButtons = Array.from(document.querySelectorAll('[data-ffb-preset]'));
  const btnDrive = document.getElementById('btnDrive');
  const btnSend = document.getElementById('btnSend');
  const btnNeutral = document.getElementById('btnNeutral');
  const btnDisconnect = document.getElementById('btnDisconnect');
  const gearState = document.getElementById('gearState');
  const gearButtons = Array.from(document.querySelectorAll('.gear-button'));
  let ffbClient = null;
  let ffbOutputEnabled = false;
  let ffbForceActive = false;
  let ffbAcquireRequestedDeviceId = '';
  let ffbSendTimer = 0;
  let ffbReconnectTimer = 0;
  let ffbShuttingDown = false;
  let ffbSpeedProxy = 0;
  let ffbSpeedProxyAt = performance.now();
  let activeFfbPreset = FFB_INITIAL_PRESET;
  const driveHud = document.getElementById('driveHud');
  const driveHudMode = document.getElementById('driveHudMode');
  const driveHudSteeringMarker = document.getElementById('driveHudSteeringMarker');
  const driveHudSteering = document.getElementById('driveHudSteering');
  const driveHudThrottle = document.getElementById('driveHudThrottle');
  const driveHudThrottleValue = document.getElementById('driveHudThrottleValue');
  const driveHudBrake = document.getElementById('driveHudBrake');
  const driveHudBrakeValue = document.getElementById('driveHudBrakeValue');
  const driveHudGear = document.getElementById('driveHudGear');
  const driveHudGearSteps = Array.from(document.querySelectorAll('.drive-gear-step'));
  const driveHudConnection = document.getElementById('driveHudConnection');

  let ws = null;
  let peerConnection = null;
  let dataChannel = null;
  let telemetryChannel = null;
  let raceChannel = null;
  let candidates = [];
  let hasReceivedSdp = false;
  let fpsFrameCount = 0;
  let fpsStartedAt = performance.now();
  let lastTotalVideoFrames = 0;
  let lastQualitySampleAt = performance.now();
  let lastVideoFrameAt = 0;
  let lastDecodedFrameAt = 0;
  let reconnectTimer = null;
  let reconnectAttempt = 0;
  let reconnectReason = '';
  let reconnectAfter = 0;
  let shouldReconnect = AUTO_START;
  let connectStartedAt = 0;
  let connectedAt = 0;
  let visibleSince = performance.now();
  let reconnectCount = 0;
  let lastEvent = 'start';
  let lastReconnectAt = 0;
  let lastReconnectReason = '';
  let lastWsClose = 'n/a';
  let eventLog = [];
  const eventCounters = {
    videoLost: 0,
    noVideo: 0,
    wsClosed: 0,
    wsError: 0,
    peerClosed: 0,
    dcClosed: 0,
    roomFull: 0,
    iceFailed: 0,
    pcFailed: 0,
  };
  let modeOptions = [];
  let lastStatsSampleAt = 0;
  let lastBytesReceived = 0;
  let lastPacketsReceived = 0;
  let lastPacketsLost = 0;
  let lastFramesDropped = 0;
  let lastJitterBufferDelay = 0;
  let lastJitterBufferEmittedCount = 0;
  let lastTotalProcessingDelay = 0;
  let lastFramesDecoded = 0;
  let decodedFrameHistory = [];
  let rcDriveEnabled = false;
  let currentGear = RC_INITIAL_GEAR;
  let rcTxTimer = null;
  let rcBrakeTimer = null;
  let lastRcCommand = 'S:1500,T:1500';
  let lastTelemetry = 'n/a';
  let dcPingSeq = 0;
  let dcRttMs = null;
  let lastDcPongAt = 0;
  let lastDeviceHostHint = '';
  const pendingDcPings = new Map();
  let deviceClock = null;
  const pressedControlKeys = new Set();
  const activeRcPointers = new Map();
  const gamepadButtonState = new Map();
  let gamepadSeen = false;
  let lastGamepadAt = 0;
  let lastGamepadStatus = 'n/a';
  const driveHudState = {
    steering: 0,
    throttle: 0,
    brake: 0,
  };
  let ayameIceServers = [];
  let audioContext = null;
  let audioSourceNode = null;
  let audioGainNode = null;
  let audioFilterNodes = [];
  let audioFilterEnabled = false;
  let audioSender = null;
  let micEnabled = false;
  let micStream = null;
  let micAudioContext = null;
  let micSourceNode = null;
  let micGainNode = null;
  let micAnalyserNode = null;
  let micDestinationNode = null;
  let micOutputTrack = null;
  let micMeterTimer = null;
  let roomLease = null;
  let roomLockStatus = null;
  let roomLockBusy = false;
  let roomLockStatusTimer = null;
  let roomLockHeartbeatTimer = null;
  let roomLockHeartbeatFailures = 0;
  let activeRaceRunId = '';
  let raceServerClockOffsetMs = 0;
  let raceStartSignalGreenUntil = 0;
  let lastRaceLapAnnouncementKey = '';
  const receivedRaceLapHistory = new Map();
  const raceState = {
    phase: 'STANDBY',
    phaseCode: 'idle',
    carId: '',
    lap: null,
    lapCount: null,
    position: null,
    fieldSize: null,
    totalTimeMs: null,
    currentLapMs: null,
    lastLapMs: null,
    bestLapMs: null,
    startAtMs: null,
    serverTimeMs: null,
    laps: [],
    rivals: [],
    clockRunning: false,
    sampledAt: 0,
  };
  const gamepadPedalIdle = {
    throttle: GAMEPAD_THROTTLE_IDLE,
    brake: GAMEPAD_BRAKE_IDLE,
  };

  function getUrlParams() {
    const params = new URLSearchParams(location.search);
    const hash = location.hash.replace(/^#\??/, '');
    if (!hash) {
      return params;
    }
    const hashParams = new URLSearchParams(hash);
    hashParams.forEach((value, key) => {
      if (!params.has(key)) {
        params.set(key, value);
      }
    });
    return params;
  }

  function updateOsdScale() {
    const viewport = window.visualViewport;
    const width = viewport?.width || window.innerWidth;
    const height = viewport?.height || window.innerHeight;
    // FHD を基準にしていた従来値では、1440p のウルトラワイドで OSD が小さすぎた。
    // 720p 以下は従来どおり、FHD 以上は従来比 1.5 倍まで拡大する。
    const isUltrawide = width / Math.max(height, 1) >= 2;
    const ultrawideBoost = isUltrawide ? 1.12 : 1;
    const scale = Math.max(1, Math.min(3, ultrawideBoost * 1.5 * width / 1920, ultrawideBoost * 1.5 * height / 1080));
    document.documentElement.style.setProperty('--osd-scale', scale.toFixed(4));
    window.requestAnimationFrame(() => {
      const driveHudHeight = driveHud?.offsetHeight || 0;
      const scaledOverflow = driveHudHeight * Math.max(0, scale - 1);
      document.documentElement.style.setProperty(
        '--drive-hud-bottom',
        `${Math.round(20 + scaledOverflow)}px`,
      );
    });
  }

  function normalizeControlUiMode(value) {
    const mode = String(value || '').toLowerCase();
    return ['auto', 'manual', 'drive'].includes(mode) ? mode : 'auto';
  }

  function getGamepadProfileStorageKey() {
    const device = new URLSearchParams(location.search).get('device')?.trim();
    return device ? `${GAMEPAD_PROFILE_STORAGE_KEY_LEGACY}:${encodeURIComponent(device)}`
                  : GAMEPAD_PROFILE_STORAGE_KEY_LEGACY;
  }

  function loadGamepadProfile() {
    try {
      const raw = window.localStorage?.getItem(GAMEPAD_PROFILE_STORAGE_KEY);
      if (!raw) {
        return {};
      }
      const parsed = JSON.parse(raw);
      return parsed && typeof parsed === 'object' ? parsed : {};
    } catch (_) {
      return {};
    }
  }

  function getNumberParamWithProfile(paramName, profileName, defaultValue, integer = false) {
    const params = getUrlParams();
    const raw = params.get(paramName);
    if (raw !== null) {
      const value = Number(raw);
      if (Number.isFinite(value)) {
        return integer ? Math.trunc(value) : value;
      }
      return defaultValue;
    }
    const profileValue = GAMEPAD_PROFILE[profileName];
    if (Number.isFinite(profileValue)) {
      return integer ? Math.trunc(profileValue) : profileValue;
    }
    return defaultValue;
  }

  function hasNumberParamWithProfile(paramName, profileName) {
    const params = getUrlParams();
    if (params.has(paramName)) {
      return Number.isFinite(Number(params.get(paramName)));
    }
    return Number.isFinite(GAMEPAD_PROFILE[profileName]);
  }

  function getBooleanParamWithProfile(paramName, profileName, defaultValue) {
    const params = getUrlParams();
    const raw = params.get(paramName);
    if (raw !== null) {
      return raw !== '0' && raw !== 'false';
    }
    const profileValue = GAMEPAD_PROFILE[profileName];
    if (typeof profileValue === 'boolean') {
      return profileValue;
    }
    return defaultValue;
  }

  function getEffectiveSteeringGain(rawGain, calibrated) {
    if (!calibrated) {
      return rawGain;
    }
    if (Math.abs(rawGain - 4.0) < 0.001 || Math.abs(rawGain - 3.75) < 0.001) {
      return 1.0;
    }
    return rawGain;
  }

  function getInitialHost() {
    const params = getUrlParams();
    const host = params.get('host');
    if (host) {
      return host;
    }
    if (location.protocol === 'http:' || location.protocol === 'https:') {
      return location.host || DEFAULT_HOST;
    }
    return DEFAULT_HOST;
  }

  function getNumberParam(name, defaultValue) {
    const params = getUrlParams();
    const value = Number(params.get(name));
    return Number.isFinite(value) && value > 0 ? value : defaultValue;
  }

  function getNumberParamAllowZero(name, defaultValue) {
    const params = getUrlParams();
    const raw = params.get(name);
    if (raw === null) {
      return defaultValue;
    }
    const value = Number(raw);
    return Number.isFinite(value) && value >= 0 ? value : defaultValue;
  }

  function getIntegerParam(name, defaultValue) {
    const params = getUrlParams();
    const raw = params.get(name);
    if (raw === null) {
      return defaultValue;
    }
    const value = Number(raw);
    return Number.isInteger(value) ? value : defaultValue;
  }

  function getBooleanParam(name, defaultValue) {
    const params = getUrlParams();
    const value = params.get(name);
    if (value === null) {
      return defaultValue;
    }
    return value !== '0' && value !== 'false';
  }

  function getStringParam(names, defaultValue = '') {
    const params = getUrlParams();
    for (const name of names) {
      const value = params.get(name);
      if (value !== null && value.trim() !== '') {
        return value.trim();
      }
    }
    return defaultValue;
  }

  function getStringListParam(names, defaultValue = []) {
    const raw = getStringParam(names, '');
    if (!raw) {
      return defaultValue;
    }
    return raw
      .split(',')
      .map((value) => value.trim())
      .filter(Boolean);
  }

  function normalizeIceMode(value) {
    const mode = String(value || '').toLowerCase();
    return ['auto', 'turn', 'stun', 'none'].includes(mode) ? mode : 'auto';
  }

  function normalizeBaseUrl(value) {
    return String(value || '').replace(/\/+$/, '');
  }

  function defaultRoomLockUrl() {
    if (!AYAME_SIGNALING_URL) {
      return '';
    }
    try {
      const url = new URL(AYAME_SIGNALING_URL);
      url.protocol = url.protocol === 'wss:' ? 'https:' : 'http:';
      url.pathname = '/fpv-lock';
      url.search = '';
      url.hash = '';
      return url.toString().replace(/\/+$/, '');
    } catch (_) {
      return '';
    }
  }

  function createRandomIdPart() {
    const bytes = new Uint8Array(6);
    if (globalThis.crypto && typeof globalThis.crypto.getRandomValues === 'function') {
      globalThis.crypto.getRandomValues(bytes);
      return Array.from(bytes, (value) => value.toString(36).padStart(2, '0')).join('');
    }
    return Math.random().toString(36).slice(2, 12);
  }

  function createAyameClientId() {
    return `fpv-viewer-${Date.now().toString(36)}-${createRandomIdPart()}`;
  }

  function getAyameClientId() {
    const configured = getStringParam(['clientId', 'ayameClientId'], '');
    if (configured && configured.toLowerCase() !== 'auto') {
      return configured;
    }
    return createAyameClientId();
  }

  function isAyameSignaling() {
    return SIGNALING_MODE === 'ayame';
  }

  function isRelaySignaling() {
    return SIGNALING_MODE === 'relay';
  }

  function usesRelayTransport() {
    return RELAY_TRANSPORT;
  }

  function getRelayDevice() {
    return getStringParam(['device'], '');
  }

  function getDeviceStatusMode() {
    const params = getUrlParams();
    const value = (params.get('deviceStatus') || '').toLowerCase();
    if (value === '') {
      return 'off';
    }
    if (value === '1' || value === 'true' || value === 'poll' || value === 'on') {
      return 'poll';
    }
    if (value === 'debug') {
      return 'debug';
    }
    if (value === 'once') {
      return 'once';
    }
    return 'off';
  }

  function isDebugEnabledByDefault() {
    const params = getUrlParams();
    const debug = params.get('debug');
    return debug === '1' || debug === 'true';
  }

  function isFlipEnabledByDefault() {
    const params = getUrlParams();
    const flip = params.get('flip');
    return flip !== '0' && flip !== 'false';
  }

  function isMirrorEnabledByDefault() {
    const params = getUrlParams();
    const mirror = params.get('mirror');
    return mirror === '1' || mirror === 'true';
  }

  function setDebugOsd(enabled) {
    document.body.classList.toggle('debug-osd', enabled);
    btnDebug.textContent = enabled ? 'Debug On' : 'Debug';
    btnDebug.setAttribute('aria-pressed', enabled ? 'true' : 'false');
    updateHostUi();
    updateDebugDeviceState();
  }

  function toggleDebugOsd() {
    setDebugOsd(!document.body.classList.contains('debug-osd'));
  }

  function setVideoFlip(enabled) {
    document.body.classList.toggle('flip-video', enabled);
    btnFlip.textContent = enabled ? 'Flip On' : 'Flip';
    btnFlip.setAttribute('aria-pressed', enabled ? 'true' : 'false');
  }

  function toggleVideoFlip() {
    setVideoFlip(!document.body.classList.contains('flip-video'));
  }

  function setVideoMirror(enabled) {
    document.body.classList.toggle('mirror-video', enabled);
    btnMirror.textContent = enabled ? 'Mirror On' : 'Mirror';
    btnMirror.setAttribute('aria-pressed', enabled ? 'true' : 'false');
  }

  function toggleVideoMirror() {
    setVideoMirror(!document.body.classList.contains('mirror-video'));
  }

  function ensureAudioGraph() {
    if (audioContext && audioSourceNode && audioGainNode) {
      return true;
    }
    const AudioContextCtor = window.AudioContext || window.webkitAudioContext;
    if (!AudioContextCtor) {
      recordEvent('audio filter unavailable', 'no AudioContext');
      return false;
    }
    try {
      audioContext = audioContext || new AudioContextCtor();
      audioSourceNode = audioSourceNode || audioContext.createMediaElementSource(remoteVideo);
      audioGainNode = audioGainNode || audioContext.createGain();
      audioFilterNodes = AUDIO_FILTER_FREQS.map((frequency) => {
        const filter = audioContext.createBiquadFilter();
        filter.type = 'notch';
        filter.frequency.value = frequency;
        filter.Q.value = AUDIO_FILTER_Q;
        return filter;
      });
      connectAudioGraph();
      return true;
    } catch (error) {
      recordEvent('audio filter failed', error.message || String(error));
      return false;
    }
  }

  function connectAudioGraph() {
    if (!audioSourceNode || !audioGainNode || !audioContext) {
      return;
    }
    try {
      audioSourceNode.disconnect();
      audioFilterNodes.forEach((node) => node.disconnect());
      audioGainNode.disconnect();
    } catch (_) {
    }

    let node = audioSourceNode;
    if (audioFilterEnabled) {
      for (const filter of audioFilterNodes) {
        node.connect(filter);
        node = filter;
      }
    }
    node.connect(audioGainNode);
    audioGainNode.connect(audioContext.destination);
    audioGainNode.gain.value = remoteVideo.muted ? 0 : 1;
  }

  function setAudioFilterEnabled(enabled) {
    audioFilterEnabled = Boolean(enabled);
    if (btnAudioFilter) {
      btnAudioFilter.textContent = audioFilterEnabled ? 'Filter On' : 'Filter';
      btnAudioFilter.setAttribute('aria-pressed', audioFilterEnabled ? 'true' : 'false');
    }
    if (!audioFilterEnabled && !audioContext) {
      return;
    }
    if (ensureAudioGraph()) {
      connectAudioGraph();
    }
  }

  function toggleAudioFilter() {
    setAudioFilterEnabled(!audioFilterEnabled);
    audioContext?.resume?.().catch(() => {});
    remoteVideo.play?.().catch(() => {});
  }

  function setAudioEnabled(enabled) {
    remoteVideo.muted = !enabled;
    remoteVideo.volume = enabled ? 1 : 0;
    if (audioGainNode) {
      audioGainNode.gain.value = enabled ? 1 : 0;
    }
    btnAudio.textContent = enabled ? 'Audio On' : 'Audio';
    btnAudio.setAttribute('aria-pressed', enabled ? 'true' : 'false');
  }

  function toggleAudio() {
    setAudioEnabled(remoteVideo.muted);
    audioContext?.resume?.().catch(() => {});
    remoteVideo.play?.().catch(() => {});
  }

  function canUseMicrophone() {
    return Boolean(navigator.mediaDevices && navigator.mediaDevices.getUserMedia);
  }

  function isMicrophoneOriginAllowed() {
    return window.isSecureContext || location.hostname === 'localhost' || location.hostname === '127.0.0.1';
  }

  function getMicVolume() {
    const value = Number(micVolumeInput?.value ?? MIC_DEFAULT_VOLUME);
    return Number.isFinite(value) ? Math.max(0, Math.min(200, value)) : MIC_DEFAULT_VOLUME;
  }

  function setMicMeterLevel(level) {
    if (!micMeter) {
      return;
    }
    micMeter.dataset.level = String(Math.max(0, Math.min(4, Math.round(level))));
  }

  function updateMicUi(detail = '') {
    if (!btnMic) {
      return;
    }
    const blocked = !canUseMicrophone() || !isMicrophoneOriginAllowed();
    btnMic.disabled = blocked;
    if (blocked) {
      btnMic.textContent = 'Mic Block';
      btnMic.title = 'Microphone requires HTTPS, localhost, or a browser insecure-origin exception.';
      btnMic.setAttribute('aria-pressed', 'false');
      setMicMeterLevel(0);
      return;
    }
    btnMic.textContent = micEnabled ? 'Mic On' : 'Mic';
    btnMic.title = detail || (micEnabled ? 'Sending browser microphone to the car speaker.' : 'Start sending browser microphone to the car speaker.');
    btnMic.setAttribute('aria-pressed', micEnabled ? 'true' : 'false');
    if (!micEnabled) {
      setMicMeterLevel(0);
    }
  }

  function setMicVolume(value = getMicVolume()) {
    const volume = Math.max(0, Math.min(200, Number(value) || 0));
    if (micVolumeInput && micVolumeInput.value !== String(volume)) {
      micVolumeInput.value = String(volume);
    }
    if (micGainNode) {
      micGainNode.gain.value = volume / 100;
    }
  }

  function startMicMeter() {
    if (micMeterTimer || !micAnalyserNode) {
      return;
    }
    const samples = new Uint8Array(micAnalyserNode.fftSize);
    micMeterTimer = window.setInterval(() => {
      if (!micEnabled || !micAnalyserNode) {
        setMicMeterLevel(0);
        return;
      }
      micAnalyserNode.getByteTimeDomainData(samples);
      let peak = 0;
      for (const value of samples) {
        peak = Math.max(peak, Math.abs(value - 128));
      }
      setMicMeterLevel(Math.min(4, Math.ceil((peak / 128) * 5)));
    }, MIC_METER_INTERVAL_MS);
  }

  function stopMicMeter() {
    if (micMeterTimer) {
      window.clearInterval(micMeterTimer);
      micMeterTimer = null;
    }
    setMicMeterLevel(0);
  }

  function stopLocalMic() {
    if (micStream) {
      for (const track of micStream.getTracks()) {
        try { track.stop(); } catch (_) {}
      }
    }
    if (micOutputTrack) {
      try { micOutputTrack.stop(); } catch (_) {}
    }
    try { micSourceNode?.disconnect(); } catch (_) {}
    try { micGainNode?.disconnect(); } catch (_) {}
    try { micAnalyserNode?.disconnect(); } catch (_) {}
    micStream = null;
    micSourceNode = null;
    micGainNode = null;
    micAnalyserNode = null;
    micDestinationNode = null;
    micOutputTrack = null;
    stopMicMeter();
  }

  async function attachMicTrackToSender() {
    if (!audioSender) {
      return;
    }
    await audioSender.replaceTrack(micEnabled ? micOutputTrack : null);
  }

  async function ensureLocalMic() {
    if (micOutputTrack && micOutputTrack.readyState === 'live') {
      setMicVolume();
      return;
    }
    if (!canUseMicrophone()) {
      throw new Error('microphone API unavailable');
    }
    if (!isMicrophoneOriginAllowed()) {
      throw new Error('microphone requires HTTPS or localhost');
    }

    const AudioContextCtor = window.AudioContext || window.webkitAudioContext;
    if (!AudioContextCtor) {
      throw new Error('AudioContext unavailable');
    }

    micStream = await navigator.mediaDevices.getUserMedia({
      audio: {
        echoCancellation: true,
        noiseSuppression: true,
        autoGainControl: true,
      },
      video: false,
    });
    micAudioContext = micAudioContext || new AudioContextCtor();
    await micAudioContext.resume?.();
    micSourceNode = micAudioContext.createMediaStreamSource(micStream);
    micGainNode = micAudioContext.createGain();
    micAnalyserNode = micAudioContext.createAnalyser();
    micAnalyserNode.fftSize = 256;
    micDestinationNode = micAudioContext.createMediaStreamDestination();
    micSourceNode.connect(micGainNode);
    micGainNode.connect(micAnalyserNode);
    micAnalyserNode.connect(micDestinationNode);
    micOutputTrack = micDestinationNode.stream.getAudioTracks()[0] || null;
    if (!micOutputTrack) {
      throw new Error('microphone output track unavailable');
    }
    micOutputTrack.enabled = true;
    setMicVolume();
    startMicMeter();
  }

  async function setMicEnabled(enabled) {
    if (!btnMic) {
      return;
    }
    if (!enabled) {
      micEnabled = false;
      await attachMicTrackToSender().catch(() => {});
      stopLocalMic();
      updateMicUi();
      return;
    }

    try {
      await ensureLocalMic();
      micEnabled = true;
      await attachMicTrackToSender();
      updateMicUi();
      recordEvent('mic on');
    } catch (error) {
      micEnabled = false;
      stopLocalMic();
      updateMicUi(error.message || String(error));
      recordEvent('mic failed', error.message || String(error));
    }
  }

  function toggleMic() {
    setMicEnabled(!micEnabled);
  }

  function setText(element, value) {
    if (!element) {
      return;
    }
    element.textContent = value;
  }

  function normalizeRaceNumber(value) {
    const number = Number(value);
    return Number.isFinite(number) && number >= 0 ? Math.round(number) : null;
  }

  function formatRaceTime(milliseconds) {
    const value = normalizeRaceNumber(milliseconds);
    if (value === null) {
      return '--:--.---';
    }
    const minutes = Math.floor(value / 60000);
    const seconds = Math.floor((value % 60000) / 1000);
    const millis = value % 1000;
    return `${minutes}:${String(seconds).padStart(2, '0')}.${String(millis).padStart(3, '0')}`;
  }

  function getDisplayedRaceTime(milliseconds) {
    if (!raceState.clockRunning || raceState.sampledAt === 0) {
      return milliseconds;
    }
    const base = normalizeRaceNumber(milliseconds);
    return base === null ? null : base + Math.max(0, performance.now() - raceState.sampledAt);
  }

  function normalizeRaceLaps(laps) {
    if (!Array.isArray(laps)) {
      return null;
    }
    return laps
      .map((entry, index) => {
        const value = typeof entry === 'number' ? entry : entry?.timeMs;
        const timeMs = normalizeRaceNumber(value);
        if (timeMs === null) {
          return null;
        }
        const lap = normalizeRaceNumber(typeof entry === 'number' ? index + 1 : entry.lap) || index + 1;
        return { lap, timeMs };
      })
      .filter((entry) => entry !== null)
      .sort((left, right) => right.lap - left.lap);
  }

  function normalizeRaceLapDelta(value) {
    const number = Number(value);
    return Number.isInteger(number) ? number : null;
  }

  function normalizeRaceRivals(rivals) {
    if (!Array.isArray(rivals)) {
      return null;
    }
    return rivals
      .map((entry) => {
        const carId = typeof entry?.carId === 'string' ? entry.carId.trim() : '';
        const position = normalizeRaceNumber(entry?.position);
        if (!carId || position === null || position < 1) {
          return null;
        }
        const driver = typeof entry.driver === 'string' && entry.driver.trim()
          ? entry.driver.trim()
          : '';
        return {
          carId,
          driver,
          position,
          lap: normalizeRaceNumber(entry.lap),
          intervalToAheadMs: normalizeRaceNumber(entry.intervalToAheadMs),
          lapDeltaToAhead: normalizeRaceLapDelta(entry.lapDeltaToAhead),
        };
      })
      .filter((entry) => entry !== null)
      .sort((left, right) => left.position - right.position);
  }

  function formatRaceInterval(intervalToAheadMs, lapDeltaToAhead) {
    if (lapDeltaToAhead !== null && lapDeltaToAhead !== 0) {
      return `+${Math.abs(lapDeltaToAhead)} LAP`;
    }
    const milliseconds = normalizeRaceNumber(intervalToAheadMs);
    if (milliseconds === null) {
      return '--';
    }
    const seconds = milliseconds / 1000;
    return `+${seconds < 10 ? seconds.toFixed(3) : seconds.toFixed(1)}s`;
  }

  function getRaceBattleOffset(intervalToAheadMs, lapDeltaToAhead) {
    if (lapDeltaToAhead !== null && lapDeltaToAhead !== 0) {
      return RACE_BATTLE_MAX_OFFSET_PX;
    }
    const milliseconds = normalizeRaceNumber(intervalToAheadMs);
    if (milliseconds === null) {
      return 40;
    }
    const steppedMilliseconds = Math.round(milliseconds / RACE_BATTLE_GAP_STEP_MS)
      * RACE_BATTLE_GAP_STEP_MS;
    const gapRatio = Math.min(1, steppedMilliseconds / RACE_BATTLE_MAX_GAP_MS);
    return RACE_BATTLE_MIN_OFFSET_PX + Math.round(
      gapRatio * (RACE_BATTLE_MAX_OFFSET_PX - RACE_BATTLE_MIN_OFFSET_PX),
    );
  }

  function getRaceRivalLabel(rival, fallback) {
    if (!rival) {
      return fallback;
    }
    return rival.driver || rival.carId;
  }

  function getRaceBattle() {
    const rivals = raceState.rivals;
    if (!Array.isArray(rivals) || rivals.length === 0) {
      return { self: null, ahead: null, behind: null, state: 'waiting' };
    }
    const self = rivals.find((rival) => rival.carId === raceState.carId)
      || rivals.find((rival) => rival.position === raceState.position)
      || null;
    if (!self) {
      return { self: null, ahead: null, behind: null, state: 'waiting' };
    }
    const selfIndex = rivals.indexOf(self);
    const ahead = selfIndex > 0 ? rivals[selfIndex - 1] : null;
    const behind = selfIndex >= 0 && selfIndex < rivals.length - 1 ? rivals[selfIndex + 1] : null;
    const hasInterval = (ahead && (
      self.intervalToAheadMs !== null || self.lapDeltaToAhead !== null
    )) || (behind && (
      behind.intervalToAheadMs !== null || behind.lapDeltaToAhead !== null
    ));
    return { self, ahead, behind, state: hasInterval ? 'live' : 'waiting' };
  }

  function renderRaceBattleRival(element, positionElement, nameElement, gapElement, rival, intervalToAheadMs, lapDeltaToAhead, fallback) {
    if (!element) {
      return;
    }
    const isAvailable = rival !== null;
    element.classList.toggle('is-missing', !isAvailable);
    element.style.setProperty('--battle-offset', `${getRaceBattleOffset(intervalToAheadMs, lapDeltaToAhead)}px`);
    setText(positionElement, isAvailable ? `P${rival.position}` : '--');
    setText(nameElement, getRaceRivalLabel(rival, fallback));
    setText(gapElement, isAvailable ? formatRaceInterval(intervalToAheadMs, lapDeltaToAhead) : '--');
  }

  function renderRaceBattle() {
    if (!raceBattle) {
      return;
    }
    raceBattle.hidden = !RACE_BATTLE_ENABLED;
    if (!RACE_BATTLE_ENABLED) {
      return;
    }
    const battle = getRaceBattle();
    const state = RACE_BATTLE_DEMO ? 'demo' : battle.state;
    raceBattle.dataset.state = state;
    setText(raceBattleState, state === 'live' ? 'LIVE' : state === 'demo' ? 'DEMO' : 'WAITING');
    setText(raceBattleSelfPosition, battle.self ? `P${battle.self.position}` : '--');
    setText(raceBattleSelfName, getRaceRivalLabel(battle.self, 'YOU'));
    renderRaceBattleRival(
      raceBattleAhead,
      raceBattleAheadPosition,
      raceBattleAheadName,
      raceBattleAheadGap,
      battle.ahead,
      battle.self?.intervalToAheadMs ?? null,
      battle.self?.lapDeltaToAhead ?? null,
      'NO AHEAD',
    );
    renderRaceBattleRival(
      raceBattleBehind,
      raceBattleBehindPosition,
      raceBattleBehindName,
      raceBattleBehindGap,
      battle.behind,
      battle.behind?.intervalToAheadMs ?? null,
      battle.behind?.lapDeltaToAhead ?? null,
      'NO BEHIND',
    );
  }

  function normalizeRacePhaseCode(phase) {
    const value = String(phase || '').trim().toLowerCase();
    switch (value) {
      case 'standby':
        return 'idle';
      case 'running':
        return 'green';
      default:
        return value || 'idle';
    }
  }

  function updateRaceClockOffset(state) {
    if (Number.isFinite(state?.serverTimeMs)) {
      raceServerClockOffsetMs = Number(state.serverTimeMs) - Date.now();
    }
  }

  function getRaceDisplayNowMs() {
    return Date.now() + raceServerClockOffsetMs;
  }

  function getRaceCountdownSeconds() {
    if (Number.isFinite(raceState.startAtMs)) {
      return Math.ceil((Number(raceState.startAtMs) - getRaceDisplayNowMs()) / 1000);
    }
    return null;
  }

  function getRaceStartSignalState() {
    if (raceState.phaseCode === 'green') {
      return {
        visible: raceStartSignalGreenUntil > performance.now(),
        mode: 'green',
        litCount: RACE_START_SIGNAL_LIGHT_COUNT,
      };
    }
    if (raceState.phaseCode === 'ready') {
      return { visible: true, mode: 'ready', litCount: 0 };
    }
    if (raceState.phaseCode === 'countdown') {
      const remaining = getRaceCountdownSeconds();
      if (!Number.isFinite(remaining)) {
        return { visible: true, mode: 'red', litCount: 0 };
      }
      if (remaining <= 0) {
        return { visible: true, mode: 'red', litCount: RACE_START_SIGNAL_LIGHT_COUNT };
      }
      const litCount = remaining > RACE_START_SIGNAL_LIGHT_COUNT
        ? 0
        : RACE_START_SIGNAL_LIGHT_COUNT - Math.max(1, remaining) + 1;
      return {
        visible: true,
        mode: 'red',
        litCount: Math.max(0, Math.min(RACE_START_SIGNAL_LIGHT_COUNT, litCount)),
      };
    }
    return { visible: false, mode: 'off', litCount: 0 };
  }

  function renderRaceStartSignal() {
    if (!raceStartSignal) {
      return;
    }
    const signal = getRaceStartSignalState();
    raceStartSignal.dataset.mode = signal.mode;
    raceStartSignal.dataset.lit = String(signal.litCount);
    raceStartSignal.classList.toggle('race-start-signal-hidden', !signal.visible);
    raceStartSignalLights.forEach((light, index) => {
      light.classList.toggle('is-lit', index < signal.litCount);
    });
  }

  function renderRaceHud() {
    const lap = raceState.lap === null ? '--' : String(raceState.lap);
    const lapCount = raceState.lapCount === null ? '--' : String(raceState.lapCount);
    const position = raceState.position === null ? '--' : String(raceState.position);
    const fieldSize = raceState.fieldSize === null ? '--' : String(raceState.fieldSize);
    setText(racePhase, raceState.phase);
    setText(raceLapCount, `LAP ${lap} / ${lapCount}`);
    setText(raceCurrentLap, formatRaceTime(getDisplayedRaceTime(raceState.currentLapMs)));
    setText(raceLastLap, formatRaceTime(raceState.lastLapMs));
    setText(raceBestLap, formatRaceTime(raceState.bestLapMs));
    setText(raceTotalTime, formatRaceTime(getDisplayedRaceTime(raceState.totalTimeMs)));
    renderRaceStartSignal();
    if (racePosition) {
      racePosition.replaceChildren(document.createTextNode(position));
      const total = document.createElement('em');
      total.textContent = `/${fieldSize}`;
      racePosition.append(total);
    }
    renderRaceBattle();
    if (!raceLapHistory) {
      return;
    }
    raceLapHistory.replaceChildren();
    if (raceState.laps.length === 0) {
      const empty = document.createElement('li');
      empty.className = 'is-empty';
      empty.textContent = 'WAITING FOR RACE DATA';
      raceLapHistory.append(empty);
      return;
    }
    for (const entry of raceState.laps) {
      const item = document.createElement('li');
      if (raceState.bestLapMs !== null && entry.timeMs === raceState.bestLapMs) {
        item.classList.add('is-best');
      }
      const label = document.createElement('span');
      label.textContent = `LAP ${entry.lap}`;
      const value = document.createElement('strong');
      value.textContent = formatRaceTime(entry.timeMs);
      item.append(label, value);
      raceLapHistory.append(item);
    }
  }

  function displayRacePhase(phase) {
    switch (String(phase || '').toLowerCase()) {
      case 'idle': return 'STANDBY';
      case 'ready': return 'READY';
      case 'countdown': return 'COUNTDOWN';
      case 'green': return 'RUNNING';
      case 'paused': return 'PAUSED';
      case 'finished': return 'FINISHED';
      default: return String(phase || 'STANDBY').toUpperCase().slice(0, 24);
    }
  }

  function adaptRaceStateV2(state) {
    if (state?.type !== 'race_state' || state?.version !== 2 || !Array.isArray(state.standings)) {
      return null;
    }
    const carId = String(state.viewerCarId || RACE_CAR_ID || '').trim();
    const standing = carId ? state.standings.find((item) => item?.carId === carId) : null;
    const runId = typeof state.raceRunId === 'string' ? state.raceRunId : '';
    const isNewRun = Boolean(runId && runId !== activeRaceRunId);
    if (isNewRun) {
      receivedRaceLapHistory.clear();
    }
    if (runId) {
      activeRaceRunId = runId;
    }
    const lap = normalizeRaceNumber(standing?.lap);
    const lastLapMs = normalizeRaceNumber(standing?.lapTimeMs);
    // MADSYSTEM はラップ確定後に LapNum を次周回へ進めてから snapshot を送る。
    // 例: lap=2 と lapTimeMs は「1 周目の確定タイム」を表す。
    const completedLap = lap === null ? null : Math.max(1, lap - 1);
    if (completedLap !== null && lastLapMs !== null && lastLapMs > 0) {
      receivedRaceLapHistory.set(completedLap, lastLapMs);
    }
    const laps = Array.from(receivedRaceLapHistory, ([completedLap, timeMs]) => ({
      lap: completedLap,
      timeMs,
    }));
    return {
      reset: isNewRun,
      phase: displayRacePhase(state.phase),
      phaseCode: normalizeRacePhaseCode(state.phase),
      carId,
      lap,
      lapCount: normalizeRaceNumber(state.raceInfo?.totalLaps),
      position: normalizeRaceNumber(standing?.position) || null,
      fieldSize: state.standings.length,
      totalTimeMs: normalizeRaceNumber(standing?.allTimeMs),
      currentLapMs: normalizeRaceNumber(standing?.currentLapMs),
      lastLapMs,
      bestLapMs: normalizeRaceNumber(standing?.bestLapMs),
      startAtMs: normalizeRaceNumber(state.startAtMs),
      serverTimeMs: normalizeRaceNumber(state.serverTimeMs),
      clockRunning: state.phase === 'green' && standing?.status === 'racing',
      laps,
      rivals: normalizeRaceRivals(state.standings) || [],
    };
  }

  function supportsRaceAnnouncement() {
    return typeof window.speechSynthesis !== 'undefined' &&
      typeof window.SpeechSynthesisUtterance === 'function';
  }

  function prepareRaceAnnouncement() {
    if (!RACE_ANNOUNCE_ENABLED || !supportsRaceAnnouncement()) {
      return false;
    }
    try {
      window.speechSynthesis.getVoices();
      return true;
    } catch (error) {
      recordEvent('race announce unavailable', error.message || String(error));
      return false;
    }
  }

  function stopRaceAnnouncement() {
    if (!supportsRaceAnnouncement()) {
      return;
    }
    window.speechSynthesis.cancel();
  }

  function getRaceLapAnnouncement() {
    const lapTimeMs = normalizeRaceNumber(raceState.lastLapMs);
    const latestLap = raceState.laps[0] || null;
    const lap = normalizeRaceNumber(latestLap?.lap) ?? normalizeRaceNumber(raceState.lap);
    if (lap === null || lap < 1 || lapTimeMs === null || lapTimeMs <= 0) {
      return null;
    }
    const roundedLapTimeMs = Math.round(lapTimeMs);
    const seconds = Math.floor(roundedLapTimeMs / 1000);
    const milliseconds = String(roundedLapTimeMs % 1000).padStart(3, '0');
    const bestLapMs = normalizeRaceNumber(raceState.bestLapMs);
    const isBestLap = bestLapMs !== null && Math.round(bestLapMs) === roundedLapTimeMs;
    return {
      key: `${activeRaceRunId || 'race'}:${Math.floor(lap)}:${roundedLapTimeMs}`,
      lap: Math.floor(lap),
      text: `ラップ ${Math.floor(lap)}、${seconds}秒${milliseconds}${isBestLap ? '。ベストラップです。' : '。'}`,
    };
  }

  function speakRaceLapAnnouncement(announcement) {
    if (!RACE_ANNOUNCE_ENABLED || !announcement) {
      return false;
    }
    if (!prepareRaceAnnouncement()) {
      recordEvent('race announce unavailable', 'SpeechSynthesis');
      return false;
    }
    try {
      const utterance = new window.SpeechSynthesisUtterance(announcement.text);
      utterance.lang = RACE_ANNOUNCE_LANGUAGE;
      utterance.rate = RACE_ANNOUNCE_RATE;
      utterance.volume = RACE_ANNOUNCE_VOLUME;
      if (RACE_ANNOUNCE_VOICE) {
        const voice = window.speechSynthesis.getVoices()
          .find((candidate) => candidate.name === RACE_ANNOUNCE_VOICE);
        if (voice) {
          utterance.voice = voice;
        } else {
          recordEvent('race announce voice unavailable', RACE_ANNOUNCE_VOICE);
        }
      }
      utterance.onerror = (event) => {
        if (event.error !== 'canceled' && event.error !== 'interrupted') {
          recordEvent('race announce failed', event.error || 'unknown');
        }
      };
      window.speechSynthesis.cancel();
      window.speechSynthesis.speak(utterance);
      recordEvent('race announce', announcement.text);
      return true;
    } catch (error) {
      recordEvent('race announce failed', error.message || String(error));
      return false;
    }
  }

  function announceRaceLapIfChanged(previousAnnouncement, hadPreviousRaceState) {
    if (raceState.phaseCode === 'idle' || raceState.phaseCode === 'ready') {
      lastRaceLapAnnouncementKey = '';
      stopRaceAnnouncement();
      return;
    }
    const nextAnnouncement = getRaceLapAnnouncement();
    if (!nextAnnouncement) {
      return;
    }
    if (!hadPreviousRaceState) {
      lastRaceLapAnnouncementKey = nextAnnouncement.key;
      return;
    }
    if (previousAnnouncement?.key === nextAnnouncement.key) {
      return;
    }
    if (previousAnnouncement && previousAnnouncement.lap === nextAnnouncement.lap) {
      lastRaceLapAnnouncementKey = nextAnnouncement.key;
      return;
    }
    if (raceState.phaseCode !== 'green' && raceState.phaseCode !== 'finished') {
      return;
    }
    if (nextAnnouncement.key === lastRaceLapAnnouncementKey) {
      return;
    }
    lastRaceLapAnnouncementKey = nextAnnouncement.key;
    speakRaceLapAnnouncement(nextAnnouncement);
  }

  function setRaceState(nextState) {
    if (!nextState || typeof nextState !== 'object') {
      return false;
    }
    const hadPreviousRaceState = raceState.sampledAt > 0;
    const previousAnnouncement = getRaceLapAnnouncement();
    updateRaceClockOffset(nextState);
    const v2State = adaptRaceStateV2(nextState);
    if (v2State !== null) {
      nextState = v2State;
    }
    const previousPhaseCode = raceState.phaseCode;
    if (nextState.reset === true) {
      raceState.phase = 'STANDBY';
      raceState.phaseCode = 'idle';
      raceState.carId = '';
      raceState.lap = null;
      raceState.lapCount = null;
      raceState.position = null;
      raceState.fieldSize = null;
      raceState.totalTimeMs = null;
      raceState.currentLapMs = null;
      raceState.lastLapMs = null;
      raceState.bestLapMs = null;
      raceState.startAtMs = null;
      raceState.serverTimeMs = null;
      raceState.laps = [];
      raceState.rivals = [];
      raceState.clockRunning = false;
      raceStartSignalGreenUntil = 0;
      lastRaceLapAnnouncementKey = '';
      stopRaceAnnouncement();
    }
    if (typeof nextState.phase === 'string' && nextState.phase.trim()) {
      raceState.phase = nextState.phase.trim().toUpperCase().slice(0, 24);
      raceState.phaseCode = normalizeRacePhaseCode(nextState.phase);
    }
    if (typeof nextState.phaseCode === 'string' && nextState.phaseCode.trim()) {
      raceState.phaseCode = normalizeRacePhaseCode(nextState.phaseCode);
    }
    if (typeof nextState.carId === 'string') {
      raceState.carId = nextState.carId.trim();
    }
    raceState.clockRunning = Object.prototype.hasOwnProperty.call(nextState, 'clockRunning')
      ? nextState.clockRunning === true
      : raceState.phase === 'RUNNING';
    for (const field of ['lap', 'lapCount', 'position', 'fieldSize', 'totalTimeMs',
      'currentLapMs', 'lastLapMs', 'bestLapMs', 'startAtMs', 'serverTimeMs']) {
      if (Object.prototype.hasOwnProperty.call(nextState, field)) {
        raceState[field] = nextState[field] === null ? null : normalizeRaceNumber(nextState[field]);
      }
    }
    if (previousPhaseCode !== 'green' && raceState.phaseCode === 'green') {
      raceStartSignalGreenUntil = performance.now() + Math.max(0, RACE_START_SIGNAL_GREEN_MS);
    } else if (raceState.phaseCode !== 'green') {
      raceStartSignalGreenUntil = 0;
    }
    if (Object.prototype.hasOwnProperty.call(nextState, 'laps')) {
      const laps = normalizeRaceLaps(nextState.laps);
      if (laps !== null) {
        raceState.laps = laps;
      }
    }
    if (Object.prototype.hasOwnProperty.call(nextState, 'rivals')) {
      const rivals = normalizeRaceRivals(nextState.rivals);
      if (rivals !== null) {
        raceState.rivals = rivals;
      }
    }
    raceState.sampledAt = performance.now();
    renderRaceHud();
    announceRaceLapIfChanged(previousAnnouncement, hadPreviousRaceState && nextState.reset !== true);
    return true;
  }

  function createRaceBattleDemoState() {
    return {
      phase: 'RUNNING',
      phaseCode: 'green',
      carId: 'FPV-02',
      lap: 3,
      lapCount: 5,
      position: 2,
      fieldSize: 4,
      totalTimeMs: 72430,
      currentLapMs: 9420,
      lastLapMs: 23860,
      bestLapMs: 23580,
      clockRunning: false,
      rivals: [
        { carId: 'FPV-01', driver: 'AYA', position: 1, lap: 3 },
        { carId: 'FPV-02', driver: 'MOMO', position: 2, lap: 3, intervalToAheadMs: 840 },
        { carId: 'FPV-03', driver: 'RIN', position: 3, lap: 3, intervalToAheadMs: 5200 },
        { carId: 'FPV-04', driver: 'KAI', position: 4, lap: 3, intervalToAheadMs: 2810 },
      ],
    };
  }

  function startRaceBattleDemo() {
    if (RACE_BATTLE_DEMO) {
      setRaceState(createRaceBattleDemoState());
    }
  }

  function handleRaceStateMessage(message) {
    if (typeof message !== 'string') {
      return false;
    }
    const payload = message.startsWith('RACE:') ? message.slice(5) : message;
    if (!message.startsWith('RACE:') && !payload.trimStart().startsWith('{')) {
      return false;
    }
    try {
      const state = JSON.parse(payload);
      if (!message.startsWith('RACE:') && state?.type !== 'race_state') {
        return false;
      }
      return setRaceState(state);
    } catch (_) {
      return false;
    }
  }

  function updateConnectionUi() {
    setText(wsState, ws ? ['connecting', 'open', 'closing', 'closed'][ws.readyState] : 'closed');
    setText(iceState, peerConnection ? peerConnection.iceConnectionState : 'new');
    setText(dcState, dataChannel ? dataChannel.readyState : 'closed');
    setText(linkState, getLinkStatus());
    setText(videoState, getVideoStatus());

    const canSend = dataChannel && dataChannel.readyState === 'open';
    const active = isConnectionActive();
    const lockedByOther = isRoomLockedByOther();
    if (btnReconnect) {
      if (active) {
        btnReconnect.textContent = reconnectTimer ? 'CANCEL' : 'DISCONNECT';
        btnReconnect.dataset.state = 'connected';
        btnReconnect.disabled = false;
      } else if (roomLockBusy) {
        btnReconnect.textContent = 'CONNECTING';
        btnReconnect.dataset.state = 'connecting';
        btnReconnect.disabled = true;
      } else if (lockedByOther) {
        btnReconnect.textContent = 'BUSY';
        btnReconnect.dataset.state = 'busy';
        btnReconnect.disabled = true;
      } else {
        btnReconnect.textContent = 'CONNECT';
        btnReconnect.dataset.state = 'idle';
        btnReconnect.disabled = false;
      }
    }
    if (btnSend) {
      btnSend.disabled = !canSend;
    }
    if (btnNeutral) {
      btnNeutral.disabled = !canSend;
    }
    btnDrive.disabled = !canSend && !rcDriveEnabled;
    if (btnDisconnect) {
      btnDisconnect.disabled = !active;
    }
    if (driveHudMode) {
      driveHudMode.disabled = !canSend && !rcDriveEnabled;
    }
    if (driveHudConnection) {
      driveHudConnection.disabled = !active;
    }
    updateMicUi();
    updateDriveHud();
  }

  function updateTimerUi() {
    updateDeviceTimeUi();
    setText(videoState, getVideoStatus());
    setText(uptimeState, getUptimeStatus());
    setText(retryState, getRetryStatus());
    setText(lastEventState, lastEvent);
    setText(diagState, getDiagnosticStatus());
    setText(videoAgeState, getVideoAgeStatus());
    setText(dcRttState, getDcRttStatus());
  }

  function updateRcUi() {
    setText(rcState, getRcStatus());
    const canSend = dataChannel && dataChannel.readyState === 'open';
    if (btnSend) {
      btnSend.disabled = !canSend;
    }
    if (btnNeutral) {
      btnNeutral.disabled = !canSend;
    }
    btnDrive.disabled = !canSend && !rcDriveEnabled;
  }

  function updateTelemetryUi() {
    setText(telemetryState, getTelemetryStatus());
  }

  function updateUiState() {
    updateConnectionUi();
    updateTimerUi();
    updateRcUi();
    updateTelemetryUi();
  }

  function updateDecodedFps(value) {
    setText(fpsState, value.toFixed(1));
  }

  function updateRenderFps(value) {
    setText(renderFpsState, value.toFixed(1));
  }

  function getLinkStatus() {
    if (reconnectTimer) {
      return 'reconnecting';
    }
    if (roomLockBusy) {
      return 'locking';
    }
    if (isRoomLockedByOther()) {
      return 'room busy';
    }
    if (!shouldReconnect && !peerConnection && !ws) {
      return 'stopped';
    }
    if (!ws || ws.readyState !== WebSocket.OPEN) {
      return 'signaling';
    }
    if (!peerConnection) {
      return 'starting';
    }
    if (peerConnection.connectionState === 'failed') {
      return 'failed';
    }
    if (peerConnection.iceConnectionState === 'connected' ||
        peerConnection.iceConnectionState === 'completed') {
      return 'connected';
    }
    return peerConnection.iceConnectionState;
  }

  function getVideoStatus() {
    if (remoteVideo.videoWidth <= 0) {
      return 'waiting';
    }
    if (isVideoFrozen()) {
      return 'VIDEO LOST';
    }
    return `${remoteVideo.videoWidth}x${remoteVideo.videoHeight}`;
  }

  function getRetryStatus() {
    if (!shouldReconnect) {
      return 'off';
    }
    if (!reconnectTimer) {
      return reconnectCount === 0 ? 'standby' : `${reconnectCount}x`;
    }
    const remaining = Math.max(0, reconnectAfter - performance.now());
    return `${reconnectCount}x ${reconnectReason || 'link'} ${Math.ceil(remaining / 1000)}s`;
  }

  function getUptimeStatus() {
    if (connectedAt === 0) {
      return '0s';
    }
    return formatDuration((performance.now() - connectedAt) / 1000);
  }

  function getDiagnosticStatus() {
    return [
      `rc${reconnectCount}`,
      `vl${eventCounters.videoLost}`,
      `nv${eventCounters.noVideo}`,
      `ws${eventCounters.wsClosed}`,
      `dc${eventCounters.dcClosed}`,
      `rf${eventCounters.roomFull}`,
      `ice${eventCounters.iceFailed}`,
      `pc${eventCounters.pcFailed}`,
    ].join(' ');
  }

  function getVideoAgeStatus() {
    const lastMediaFrameAt = Math.max(lastVideoFrameAt, lastDecodedFrameAt);
    if (lastMediaFrameAt === 0) {
      return 'n/a';
    }
    return `${Math.max(0, performance.now() - lastMediaFrameAt).toFixed(0)}ms`;
  }

  function getTransportSummary() {
    const wsStatus = ws ? ['connecting', 'open', 'closing', 'closed'][ws.readyState] : 'none';
    const iceStatus = peerConnection ? peerConnection.iceConnectionState : 'none';
    const pcStatus = peerConnection ? peerConnection.connectionState : 'none';
    const dcStatus = dataChannel ? dataChannel.readyState : 'none';
    return `ws=${wsStatus} ice=${iceStatus} pc=${pcStatus} dc=${dcStatus} video=${getVideoAgeStatus()}`;
  }

  function recordEvent(type, detail = '') {
    const elapsedMs = performance.now();
    const entry = {
      at: new Date().toISOString(),
      elapsedMs: Math.round(elapsedMs),
      type,
      detail,
      transport: getTransportSummary(),
      reconnectCount,
    };
    eventLog.push(entry);
    if (eventLog.length > 100) {
      eventLog.shift();
    }
    lastEvent = detail ? `${type}: ${detail}` : type;
    window.localStorage?.setItem('fpvViewerLastEvents', JSON.stringify(eventLog.slice(-20)));
    console.info('[FPV]', entry);
  }

  function formatDuration(seconds) {
    const totalSeconds = Math.max(0, Math.floor(seconds));
    const minutes = Math.floor(totalSeconds / 60);
    const rest = totalSeconds % 60;
    if (minutes === 0) {
      return `${rest}s`;
    }
    return `${minutes}m${rest.toString().padStart(2, '0')}s`;
  }

  function isVideoFrozen() {
    const lastMediaFrameAt = Math.max(lastVideoFrameAt, lastDecodedFrameAt);
    if (!peerConnection || remoteVideo.videoWidth <= 0 || lastMediaFrameAt === 0) {
      return false;
    }
    return performance.now() - lastMediaFrameAt > VIDEO_FREEZE_TIMEOUT_MS;
  }

  function startFpsMonitor() {
    if ('requestVideoFrameCallback' in remoteVideo) {
      const onVideoFrame = () => {
        lastVideoFrameAt = performance.now();
        fpsFrameCount += 1;
        const now = performance.now();
        const elapsed = now - fpsStartedAt;
        if (elapsed >= 1000) {
          updateRenderFps((fpsFrameCount * 1000) / elapsed);
          fpsFrameCount = 0;
          fpsStartedAt = now;
        }
        remoteVideo.requestVideoFrameCallback(onVideoFrame);
      };
      remoteVideo.requestVideoFrameCallback(onVideoFrame);
      return;
    }

    window.setInterval(() => {
      if (!remoteVideo.getVideoPlaybackQuality) {
        return;
      }
      const quality = remoteVideo.getVideoPlaybackQuality();
      const now = performance.now();
      const elapsed = now - lastQualitySampleAt;
      if (elapsed <= 0) {
        return;
      }
      const frameDelta = quality.totalVideoFrames - lastTotalVideoFrames;
      if (frameDelta > 0) {
        lastVideoFrameAt = now;
      }
      updateRenderFps((frameDelta * 1000) / elapsed);
      lastTotalVideoFrames = quality.totalVideoFrames;
      lastQualitySampleAt = now;
    }, 1000);
  }

  function buildCommand() {
    return `S:${steeringInput.value},T:${throttleInput.value}`;
  }

  function syncCommandFromSliders() {
    steeringValue.value = steeringInput.value;
    throttleValue.value = throttleInput.value;
    lastRcCommand = buildCommand();
    if (dataTextInput) {
      dataTextInput.value = lastRcCommand;
    }
    updateRcUi();
    sendFfbSteering();
  }

  function updateFfbSpeedProxy() {
    const now = performance.now();
    const elapsedSec = Math.max(0, Math.min(0.25, (now - ffbSpeedProxyAt) / 1000));
    ffbSpeedProxyAt = now;
    const throttleValue = Number(throttleInput?.value || 1500);
    const forward = Math.max(0, Math.min(1, (throttleValue - 1500) / 500));
    const braking = Math.max(0, Math.min(1, (1500 - throttleValue) / 500));
    const rate = forward > ffbSpeedProxy
      ? FFB_SPEED_PROXY_ACCEL_PER_SEC
      : braking > 0.01
        ? FFB_SPEED_PROXY_BRAKE_DECEL_PER_SEC
        : FFB_SPEED_PROXY_COAST_DECEL_PER_SEC;
    const step = rate * elapsedSec;
    ffbSpeedProxy += Math.max(-step, Math.min(step, forward - ffbSpeedProxy));
    return ffbSpeedProxy;
  }

  function scheduleFfbReconnect() {
    if (!FFB_ENABLED || ffbShuttingDown || ffbReconnectTimer) return;
    ffbReconnectTimer = window.setTimeout(() => {
      ffbReconnectTimer = 0;
      const state = ffbClient?.snapshot();
      if (state && !state.connected && !state.connecting) ffbClient.connect();
    }, FFB_RECONNECT_DELAY_MS);
  }

  function updateFfbState(snapshot = ffbClient?.snapshot?.()) {
    if (!FFB_ENABLED) return;
    const state = snapshot || { connected: false, connecting: false, acquired: false, devices: [], lastError: '' };
    const devices = Array.isArray(state.devices) ? state.devices : [];
    if (!state.connected) {
      ffbAcquireRequestedDeviceId = '';
      ffbForceActive = false;
      if (!state.connecting) scheduleFfbReconnect();
      return;
    }
    if (!state.acquired) {
      const device = devices.find((candidate) => candidate?.isFfbCapable && supportsConstantForce(candidate.capabilities));
      const deviceId = String(device?.id || '');
      if (deviceId && ffbAcquireRequestedDeviceId !== deviceId) {
        ffbAcquireRequestedDeviceId = deviceId;
        ffbClient?.acquire(deviceId);
      }
      return;
    }
    ffbAcquireRequestedDeviceId = String(state.selectedDeviceId || ffbAcquireRequestedDeviceId);
    sendFfbSteering();
  }

  function sendFfbSteering() {
    if (!ffbClient) return;
    const snapshot = ffbClient.snapshot();
    if (!ffbOutputEnabled || !rcDriveEnabled || !snapshot.acquired) {
      if (ffbForceActive) {
        ffbClient.stopAll();
        ffbForceActive = false;
      }
      return;
    }
    const speedProxy = updateFfbSpeedProxy();
    const preset = FFB_PRESETS[activeFfbPreset];
    const capabilities = getFfbCapabilities(snapshot.deviceCapabilities);
    ffbClient.sendFfb({
      torque: 0,
      gain: 1,
      enabled: true,
      effectMode: 'baseline',
      speedProxy,
      baseFriction: capabilities.friction ? FFB_BASE_FRICTION * preset.scale : 0,
      parkingFriction: capabilities.friction ? FFB_PARKING_FRICTION * preset.scale : 0,
      baseDamper: capabilities.damper ? FFB_BASE_DAMPER * preset.scale : 0,
      speedDamper: capabilities.damper ? FFB_SPEED_DAMPER * preset.scale : 0,
      damper: 0,
      friction: 0,
      inertia: 0,
    });
    ffbForceActive = true;
  }

  function stopFfbOutput() {
    ffbOutputEnabled = false;
    ffbForceActive = false;
    ffbSpeedProxy = 0;
    ffbSpeedProxyAt = performance.now();
    ffbClient?.stopAll();
  }

  function normalizeFfbPreset(value) {
    const preset = String(value || '').toLowerCase();
    return Object.prototype.hasOwnProperty.call(FFB_PRESETS, preset) ? preset : 'medium';
  }

  function getFfbCapabilities(value) {
    if (!value || value.effectsEnumerated !== true) {
      return { constantForce: true, friction: true, damper: true };
    }
    return {
      constantForce: value.constantForce === true,
      friction: value.friction === true,
      damper: value.damper === true,
    };
  }

  function supportsConstantForce(value) {
    return getFfbCapabilities(value).constantForce;
  }

  function updateFfbPresetControls() {
    if (ffbPresetControls) {
      ffbPresetControls.hidden = !FFB_ENABLED;
    }
    for (const button of ffbPresetButtons) {
      const selected = button.dataset.ffbPreset === activeFfbPreset;
      button.setAttribute('aria-pressed', selected ? 'true' : 'false');
      button.title = `FFB ${FFB_PRESETS[button.dataset.ffbPreset]?.label || button.dataset.ffbPreset}`;
    }
  }

  function setFfbPreset(preset, source = 'ui') {
    if (!FFB_ENABLED) return;
    const next = normalizeFfbPreset(preset);
    if (next === activeFfbPreset) return;
    activeFfbPreset = next;
    updateFfbPresetControls();
    sendFfbSteering();
    recordEvent('ffb preset', `${next} via ${source}`);
  }

  function cycleFfbPreset() {
    const presets = Object.keys(FFB_PRESETS);
    const currentIndex = presets.indexOf(activeFfbPreset);
    setFfbPreset(presets[(currentIndex + 1) % presets.length], 'gamepad');
  }

  function initializeFfb() {
    if (!FFB_ENABLED) return;
    if (!window.FpvFfbBridge?.FfbBridgeClient) {
      console.warn('FFB Bridge client script was not loaded');
      return;
    }
    ffbClient = new window.FpvFfbBridge.FfbBridgeClient({
      url: FFB_BRIDGE_URL,
      onState: updateFfbState,
    });
    ffbSendTimer = window.setInterval(sendFfbSteering, FFB_SEND_INTERVAL_MS);
    ffbClient.connect();
  }

  function syncCommandFromThrottleSlider() {
    cancelThrottleBrake();
    throttleInput.value = String(clampRcAxisValue('throttle', Number(throttleInput.value)));
    syncCommandFromSliders();
  }

  function sendCommand(command) {
    if (!dataChannel || dataChannel.readyState !== 'open') {
      return false;
    }
    dataChannel.send(`${command}\n`);
    lastRcCommand = command;
    return true;
  }

  function getRcStatus() {
    const mode = rcDriveEnabled ? 'drive' : 'manual';
    const link = dataChannel && dataChannel.readyState === 'open' ? 'open' : 'wait';
    const gamepad = GAMEPAD_ENABLED ? ` ${lastGamepadStatus}` : '';
    return `${mode} ${link} g${currentGear} ${lastRcCommand}${gamepad}`;
  }

  function getTelemetryStatus() {
    return lastTelemetry;
  }

  function getDcRttStatus() {
    if (dcRttMs === null) {
      return 'n/a';
    }
    const ageMs = lastDcPongAt > 0 ? performance.now() - lastDcPongAt : 0;
    return `${dcRttMs.toFixed(1)}ms ${formatDuration(ageMs / 1000)} ago`;
  }

  function parseTelemetryFields(message) {
    const fields = {};
    for (const token of message.split(/\s+/)) {
      const pos = token.indexOf('=');
      if (pos <= 0) {
        continue;
      }
      fields[token.slice(0, pos)] = token.slice(pos + 1);
    }
    return fields;
  }

  function formatTelemetryDeviceStatus(fields) {
    const parts = [];
    if (fields.host) {
      updateHostUi(fields.host);
    }
    if (fields.temp) {
      parts.push(fields.temp);
    }
    if (fields.thr) {
      parts.push(fields.thr);
    }
    if (fields.uv === '1') {
      parts.push('UV!');
    } else if (fields.uv_seen === '1') {
      parts.push('uvSeen');
    } else if (fields.uv || fields.uv_seen || fields.thr) {
      parts.push('PWRok');
    }
    if (fields.vcore) {
      parts.push(`core${fields.vcore}`);
    }
    if (fields.rssi) {
      parts.push(fields.rssi);
    }
    if (fields.freq) {
      parts.push(fields.freq);
    }
    if (fields.ssid) {
      parts.push(fields.ssid);
    }
    return parts.join(' ');
  }

  function stripPort(host) {
    const hostWithoutPort = String(host || '').split(':')[0];
    return hostWithoutPort || host || '';
  }

  function formatDebugHost(host) {
    return stripPort(host) || 'n/a';
  }

  function sanitizeDeviceLabel(label) {
    return String(label || '')
      .trim()
      .replace(/[^\w.-]/g, '')
      .slice(0, 24);
  }

  function formatPublicDeviceId(host) {
    const configured = sanitizeDeviceLabel(getStringParam(['id', 'deviceId', 'device', 'label']));
    if (configured) {
      return configured;
    }

    const hostWithoutPort = stripPort(host);
    const match = hostWithoutPort.match(/^192\.168\.11\.(\d+)$/);
    if (match) {
      return `FPV-${match[1].padStart(2, '0')}`;
    }

    const firstLabel = hostWithoutPort.split('.')[0];
    const namedMatch = firstLabel.match(/^momo-fpv-(\d+)$/i);
    if (namedMatch) {
      return `FPV-${namedMatch[1].padStart(2, '0')}`;
    }

    if (/^\d+\.\d+\.\d+\.\d+$/.test(hostWithoutPort) || hostWithoutPort.includes(':')) {
      return 'FPV';
    }

    if (hostWithoutPort.includes('.')) {
      return sanitizeDeviceLabel(firstLabel) || 'FPV';
    }

    return sanitizeDeviceLabel(hostWithoutPort) || 'FPV';
  }

  function updateHostUi(host) {
    const hostHint = host || lastDeviceHostHint || getEndpointHostName();
    lastDeviceHostHint = hostHint;
    const display = isDebugOsdEnabled() ? formatDebugHost(hostHint) : formatPublicDeviceId(hostHint);
    setText(hostState, display);
  }

  function applyTelemetry(message) {
    lastTelemetry = message;
    updateTelemetryUi();

    const deviceStatus = formatTelemetryDeviceStatus(parseTelemetryFields(message));
    if (deviceStatus) {
      setText(deviceState, deviceStatus);
    }
  }

  function handleDcPong(message) {
    const parts = message.split(':');
    if (parts.length < 3) {
      return;
    }
    const seq = parts[1];
    const fallbackSentAt = Number(parts[2]);
    const sentAt = pendingDcPings.get(seq) ?? fallbackSentAt;
    pendingDcPings.delete(seq);
    if (!Number.isFinite(sentAt)) {
      return;
    }
    dcRttMs = Math.max(0, performance.now() - sentAt);
    lastDcPongAt = performance.now();
    setText(dcRttState, getDcRttStatus());
  }

  function handleDataChannelMessage(message) {
    if (handleRaceStateMessage(message)) {
      return;
    }
    if (typeof message === 'string' && message.startsWith('PONG:')) {
      handleDcPong(message);
      return;
    }
    if (typeof message === 'string' && message.startsWith('TEL:')) {
      applyTelemetry(message);
      return;
    }
    console.log('DataChannel RX:', message);
  }

  function clampRcValue(value, minValue = 1000, maxValue = 2000) {
    return Math.max(minValue, Math.min(maxValue, Math.round(value)));
  }

  function getThrottleGearMin() {
    return RC_THROTTLE_GEAR_MIN_VALUES[currentGear - 1] || RC_THROTTLE_MIN;
  }

  function getThrottleGearMax() {
    return RC_THROTTLE_GEAR_MAX_VALUES[currentGear - 1] || 2000;
  }

  function updateGearUi() {
    const throttleMin = getThrottleGearMin();
    const throttleMax = getThrottleGearMax();
    throttleInput.min = String(throttleMin);
    throttleInput.max = String(throttleMax);
    if (gearState) {
      gearState.textContent = `Gear ${currentGear}`;
    }
    for (const button of gearButtons) {
      const gear = Number(button.dataset.gear);
      button.hidden = gear > RC_GEAR_COUNT;
      button.setAttribute('aria-pressed', gear === currentGear ? 'true' : 'false');
    }
    updateDriveHud();
  }

  function isGamepadDriveActive() {
    return GAMEPAD_ENABLED && rcDriveEnabled && gamepadSeen && performance.now() - lastGamepadAt <= 500;
  }

  function isDriveUiVisible() {
    if (CONTROL_UI_MODE === 'manual') {
      return false;
    }
    if (CONTROL_UI_MODE === 'drive') {
      return true;
    }
    return isGamepadDriveActive();
  }

  function updateControlUiMode() {
    const driveUiVisible = isDriveUiVisible();
    document.body.classList.toggle('drive-ui', driveUiVisible);
    if (driveHud) {
      driveHud.hidden = !driveUiVisible;
    }
    updateDriveHud();
    updateOsdScale();
  }

  function setDriveHudLevel(element, value) {
    if (!element) {
      return;
    }
    element.style.setProperty('--drive-level', String(Math.max(0, Math.min(1, value))));
  }

  function updateDriveHud() {
    const steering = Math.max(-1, Math.min(1, driveHudState.steering));
    const throttle = Math.max(0, Math.min(1, driveHudState.throttle));
    const brake = Math.max(0, Math.min(1, driveHudState.brake));
    const gamepadActive = isGamepadDriveActive();
    if (driveHudMode) {
      driveHudMode.textContent = rcDriveEnabled ? 'DRIVE ON' : 'DRIVE OFF';
      driveHudMode.setAttribute('aria-pressed', rcDriveEnabled ? 'true' : 'false');
    }
    if (driveHudSteeringMarker) {
      driveHudSteeringMarker.style.left = `${(50 + steering * 45).toFixed(1)}%`;
    }
    if (driveHudSteering) {
      driveHudSteering.textContent = `${Math.round(steering * 100)}%`;
    }
    setDriveHudLevel(driveHudThrottle, throttle);
    setDriveHudLevel(driveHudBrake, brake);
    if (driveHudThrottleValue) {
      driveHudThrottleValue.textContent = `${Math.round(throttle * 100)}%`;
    }
    if (driveHudBrakeValue) {
      driveHudBrakeValue.textContent = `${Math.round(brake * 100)}%`;
    }
    if (driveHudGear) {
      driveHudGear.setAttribute('aria-label', `Throttle gear ${currentGear} of ${RC_GEAR_COUNT}`);
    }
    for (const step of driveHudGearSteps) {
      const gear = Number(step.dataset.gear);
      const available = gear <= RC_GEAR_COUNT;
      step.hidden = !available;
      step.classList.toggle('is-active', available && gear === currentGear);
      step.setAttribute('aria-current', available && gear === currentGear ? 'true' : 'false');
    }
    if (driveHudConnection) {
      const connected = dataChannel && dataChannel.readyState === 'open';
      driveHudConnection.textContent = connected ? 'DISCONNECT' : 'DISCONNECTED';
      driveHudConnection.dataset.active = connected ? 'true' : 'false';
    }
  }

  function setThrottleGear(gear) {
    const nextGear = Math.max(1, Math.min(RC_GEAR_COUNT, Number(gear) || 1));
    if (nextGear === currentGear) {
      updateGearUi();
      return;
    }
    currentGear = nextGear;
    updateGearUi();
    const throttle = Number(throttleInput.value);
    const limitedThrottle = clampRcAxisValue('throttle', throttle);
    if (limitedThrottle !== throttle) {
      throttleInput.value = String(limitedThrottle);
      syncCommandFromSliders();
      if (rcDriveEnabled) {
        sendCurrentRcCommand();
      }
    } else {
      syncCommandFromSliders();
    }
    recordEvent('gear', String(currentGear));
  }

  function applyNeutralDeadband(value, deadbandUs) {
    const pulse = clampRcValue(value);
    return Math.abs(pulse - 1500) <= deadbandUs ? 1500 : pulse;
  }

  function clampRcAxisValue(axis, value) {
    if (axis === 'throttle') {
      const pulse = applyNeutralDeadband(value, RC_THROTTLE_NEUTRAL_DEADBAND_US);
      const minValue = pulse < 1500 ? getThrottleGearMin() : RC_THROTTLE_MIN;
      const maxValue = pulse > 1500 ? getThrottleGearMax() : 2000;
      return clampRcValue(pulse, minValue, maxValue);
    }
    return clampRcValue(applyNeutralDeadband(value, RC_STEERING_NEUTRAL_DEADBAND_US));
  }

  function cancelThrottleBrake() {
    if (!rcBrakeTimer) {
      return;
    }
    window.clearTimeout(rcBrakeTimer);
    rcBrakeTimer = null;
  }

  function setRcInputs(steering, throttle) {
    steeringInput.value = String(clampRcValue(steering));
    throttleInput.value = String(clampRcAxisValue('throttle', throttle));
    syncCommandFromSliders();
    updateDriveHud();
  }

  function setRcAxis(axis, value) {
    if (axis === 'steering') {
      steeringInput.value = String(clampRcAxisValue(axis, value));
    } else if (axis === 'throttle') {
      throttleInput.value = String(clampRcAxisValue(axis, value));
    }
    syncCommandFromSliders();
  }

  function setThrottleNeutral() {
    throttleInput.value = '1500';
    syncCommandFromSliders();
    if (rcDriveEnabled) {
      sendCurrentRcCommand();
    }
  }

  function startThrottleBrake() {
    cancelThrottleBrake();
    setRcAxis('throttle', RC_BRAKE_VALUE);
    if (rcDriveEnabled) {
      sendCurrentRcCommand();
    }
    rcBrakeTimer = window.setTimeout(() => {
      rcBrakeTimer = null;
      setThrottleNeutral();
    }, RC_BRAKE_DURATION_MS);
  }

  function resetRcAxis(axis) {
    if (axis === 'throttle') {
      if (Number(throttleInput.value) > RC_BRAKE_THRESHOLD) {
        startThrottleBrake();
      } else {
        setThrottleNeutral();
      }
      return;
    }
    setRcAxis(axis, 1500);
    if (rcDriveEnabled) {
      sendCurrentRcCommand();
    }
  }

  function onRcPointerDown(axis, event) {
    if (axis === 'throttle') {
      cancelThrottleBrake();
    }
    activeRcPointers.set(event.pointerId, axis);
    event.currentTarget.setPointerCapture?.(event.pointerId);
  }

  function onRcPointerEnd(event) {
    const axis = activeRcPointers.get(event.pointerId);
    if (!axis) {
      return;
    }
    activeRcPointers.delete(event.pointerId);
    if (event.type !== 'lostpointercapture') {
      try {
        event.currentTarget.releasePointerCapture?.(event.pointerId);
      } catch (error) {
        console.debug('releasePointerCapture failed:', error);
      }
    }
    resetRcAxis(axis);
  }

  function onRcControlBlur(axis) {
    for (const [pointerId, activeAxis] of activeRcPointers) {
      if (activeAxis === axis) {
        activeRcPointers.delete(pointerId);
      }
    }
    resetRcAxis(axis);
  }

  function setNeutralCommand() {
    cancelThrottleBrake();
    setRcInputs(1500, 1500);
  }

  function sendCurrentRcCommand() {
    sendCommand(buildCommand());
  }

  function cleanupDcPings(now) {
    for (const [seq, sentAt] of pendingDcPings) {
      if (now - sentAt > 5000) {
        pendingDcPings.delete(seq);
      }
    }
  }

  function sendDcPing() {
    if (
      isAyameSignaling()
      || !DC_PING_ENABLED
      || !isDebugOsdEnabled()
      || !dataChannel
      || dataChannel.readyState !== 'open'
    ) {
      return;
    }
    const now = performance.now();
    cleanupDcPings(now);
    dcPingSeq = (dcPingSeq + 1) % 1000000;
    const seq = String(dcPingSeq);
    pendingDcPings.set(seq, now);
    dataChannel.send(`PING:${seq}:${now.toFixed(3)}`);
  }

  function startDcPingMonitor() {
    window.setInterval(sendDcPing, DC_PING_INTERVAL_MS);
  }

  function startRcTx() {
    if (rcTxTimer) {
      return;
    }
    rcTxTimer = window.setInterval(sendCurrentRcCommand, RC_TX_INTERVAL_MS);
    sendCurrentRcCommand();
  }

  function stopRcTx() {
    if (!rcTxTimer) {
      return;
    }
    window.clearInterval(rcTxTimer);
    rcTxTimer = null;
  }

  function getActiveGamepad() {
    if (!GAMEPAD_ENABLED || !navigator.getGamepads) {
      return null;
    }
    const gamepad = navigator.getGamepads()[GAMEPAD_INDEX];
    return gamepad && gamepad.connected ? gamepad : null;
  }

  function setDriveEnabled(enabled) {
    rcDriveEnabled = enabled;
    btnDrive.textContent = enabled ? 'Drive On' : 'Drive Off';
    btnDrive.setAttribute('aria-pressed', enabled ? 'true' : 'false');
    pressedControlKeys.clear();

    if (enabled) {
      ffbOutputEnabled = FFB_ENABLED;
      setNeutralCommand();
      captureGamepadPedalIdle(getActiveGamepad());
      startRcTx();
    } else {
      stopFfbOutput();
      cancelThrottleBrake();
      stopRcTx();
      setNeutralCommand();
      sendCurrentRcCommand();
    }
    if (roomLease) {
      heartbeatRoomLease();
    }
    updateRcUi();
    updateControlUiMode();
    sendFfbSteering();
  }

  function toggleDrive() {
    setDriveEnabled(!rcDriveEnabled);
  }

  function isTextEditingTarget(target) {
    if (!target) {
      return false;
    }
    const tagName = target.tagName;
    return tagName === 'INPUT' || tagName === 'TEXTAREA' || tagName === 'SELECT';
  }

  function getControlKey(code) {
    switch (code) {
      case 'ArrowLeft':
      case 'KeyA':
        return 'left';
      case 'ArrowRight':
      case 'KeyD':
        return 'right';
      case 'ArrowUp':
      case 'KeyW':
        return 'forward';
      case 'ArrowDown':
      case 'KeyS':
        return 'backward';
      case 'Space':
        return 'neutral';
      default:
        return '';
    }
  }

  function applyKeyboardCommand() {
    if (pressedControlKeys.has('neutral')) {
      setNeutralCommand();
      return;
    }

    const steeringOffset =
      (pressedControlKeys.has('right') ? RC_STEERING_THROW : 0) -
      (pressedControlKeys.has('left') ? RC_STEERING_THROW : 0);
    const throttleOffset =
      (pressedControlKeys.has('forward') ? RC_THROTTLE_THROW : 0) -
      (pressedControlKeys.has('backward') ? RC_THROTTLE_THROW : 0);

    if (throttleOffset !== 0) {
      cancelThrottleBrake();
    }
    setRcInputs(1500 + steeringOffset, 1500 + throttleOffset);
  }

  function applyDeadzone(value, deadzone) {
    if (Math.abs(value) <= deadzone) {
      return 0;
    }
    const sign = value < 0 ? -1 : 1;
    return sign * ((Math.abs(value) - deadzone) / (1 - deadzone));
  }

  function getGamepadAxis(gamepad, axis, fallback = 0) {
    if (!gamepad || axis < 0 || axis >= gamepad.axes.length) {
      return fallback;
    }
    const value = Number(gamepad.axes[axis]);
    return Number.isFinite(value) ? value : fallback;
  }

  function formatRawGamepadAxes(gamepad) {
    if (!gamepad) {
      return 'raw n/a';
    }
    return `raw[${gamepad.axes.map((value, index) => `${index}:${Number(value).toFixed(2)}`).join(' ')}]`;
  }

  function getGamepadButtonPressed(gamepad, buttonIndex) {
    if (!gamepad || buttonIndex < 0 || buttonIndex >= gamepad.buttons.length) {
      return false;
    }
    const button = gamepad.buttons[buttonIndex];
    if (typeof button === 'number') {
      return button >= 0.5;
    }
    return button.pressed || button.value >= 0.5;
  }

  function getGamepadButtonRisingEdge(gamepad, buttonIndex) {
    const pressed = getGamepadButtonPressed(gamepad, buttonIndex);
    const previous = gamepadButtonState.get(buttonIndex) === true;
    gamepadButtonState.set(buttonIndex, pressed);
    return pressed && !previous;
  }

  function captureGamepadPedalIdle(gamepad) {
    if (!gamepad) {
      return;
    }
    if (GAMEPAD_THROTTLE_AXIS >= 0 && !GAMEPAD_THROTTLE_IDLE_CONFIGURED) {
      gamepadPedalIdle.throttle = getGamepadAxis(gamepad, GAMEPAD_THROTTLE_AXIS, gamepadPedalIdle.throttle);
    }
    if (GAMEPAD_BRAKE_AXIS >= 0 && !GAMEPAD_BRAKE_IDLE_CONFIGURED) {
      gamepadPedalIdle.brake = getGamepadAxis(gamepad, GAMEPAD_BRAKE_AXIS, gamepadPedalIdle.brake);
    }
    recordEvent(
      'gamepad idle',
      `throttle=${gamepadPedalIdle.throttle.toFixed(3)} brake=${gamepadPedalIdle.brake.toFixed(3)}`
    );
  }

  function normalizePedalAxis(value, invert, idleValue, pressedValue) {
    const raw = invert ? -value : value;
    const idle = invert ? -idleValue : idleValue;
    const defaultPressed = idleValue >= 0 ? -1 : 1;
    const pressed = invert ? -pressedValue : pressedValue;
    const fallbackPressed = invert ? -defaultPressed : defaultPressed;
    const span = Math.abs(pressed - idle) >= 0.001
      ? pressed - idle
      : fallbackPressed - idle;
    const normalized = (raw - idle) / (Math.abs(span) >= 0.001 ? span : 1);
    return applyDeadzone(Math.max(0, Math.min(1, normalized)), GAMEPAD_PEDAL_DEADZONE);
  }

  function normalizeSteeringAxis(value) {
    const raw = GAMEPAD_STEERING_INVERT ? -value : value;
    const center = GAMEPAD_STEERING_INVERT ? -GAMEPAD_STEERING_CENTER : GAMEPAD_STEERING_CENTER;
    const left = GAMEPAD_STEERING_INVERT ? -GAMEPAD_STEERING_RIGHT : GAMEPAD_STEERING_LEFT;
    const right = GAMEPAD_STEERING_INVERT ? -GAMEPAD_STEERING_LEFT : GAMEPAD_STEERING_RIGHT;
    const leftSpan = Math.max(0.001, Math.abs(center - left));
    const rightSpan = Math.max(0.001, Math.abs(right - center));
    const normalized = raw < center
      ? -Math.min(1, Math.abs(raw - center) / leftSpan)
      : Math.min(1, Math.abs(raw - center) / rightSpan);
    return Math.max(-1, Math.min(1, applyDeadzone(normalized, GAMEPAD_STEERING_DEADZONE) * GAMEPAD_STEERING_GAIN));
  }

  function formatGamepadStatus(gamepad, steering, throttle, brake) {
    const ageMs = lastGamepadAt > 0 ? performance.now() - lastGamepadAt : 0;
    return `gp#${gamepad.index} s${steering.toFixed(2)} t${throttle.toFixed(2)} b${brake.toFixed(2)} idle${gamepadPedalIdle.throttle.toFixed(2)}/${gamepadPedalIdle.brake.toFixed(2)} ${Math.round(ageMs)}ms ${formatRawGamepadAxes(gamepad)}`;
  }

  function applyGamepadCommand(gamepad) {
    const rawSteering = getGamepadAxis(gamepad, GAMEPAD_STEERING_AXIS);
    const steering = normalizeSteeringAxis(rawSteering);
    const throttle = GAMEPAD_THROTTLE_AXIS >= 0
      ? normalizePedalAxis(
        getGamepadAxis(gamepad, GAMEPAD_THROTTLE_AXIS, gamepadPedalIdle.throttle),
        GAMEPAD_THROTTLE_INVERT,
        gamepadPedalIdle.throttle,
        GAMEPAD_THROTTLE_PRESSED
      )
      : 0;
    const brake = GAMEPAD_BRAKE_AXIS >= 0
      ? normalizePedalAxis(
        getGamepadAxis(gamepad, GAMEPAD_BRAKE_AXIS, gamepadPedalIdle.brake),
        GAMEPAD_BRAKE_INVERT,
        gamepadPedalIdle.brake,
        GAMEPAD_BRAKE_PRESSED
      )
      : 0;

    const steeringPwm = 1500 + steering * RC_STEERING_THROW;
    const throttlePwm = brake > 0
      ? 1500 - brake * (1500 - getThrottleGearMin())
      : 1500 + throttle * (getThrottleGearMax() - 1500);

    cancelThrottleBrake();
    setRcInputs(steeringPwm, throttlePwm);
    driveHudState.steering = Math.max(
      -1,
      Math.min(1, (steeringPwm - 1500) / Math.max(1, Math.abs(RC_STEERING_THROW)))
    );
    driveHudState.throttle = throttle;
    driveHudState.brake = brake;
    updateDriveHud();
    lastGamepadStatus = formatGamepadStatus(gamepad, steering, throttle, brake);
  }

  function pollGamepad() {
    if (!GAMEPAD_ENABLED) {
      return;
    }
    const gamepad = getActiveGamepad();
    if (!gamepad) {
      if (gamepadSeen && performance.now() - lastGamepadAt > 500) {
        lastGamepadStatus = 'gamepad lost';
      }
      updateControlUiMode();
      return;
    }

    gamepadSeen = true;
    lastGamepadAt = performance.now();
    if (GAMEPAD_DRIVE_BUTTON_ENABLED && getGamepadButtonRisingEdge(gamepad, GAMEPAD_DRIVE_BUTTON)) {
      toggleDrive();
    }
    if (getGamepadButtonRisingEdge(gamepad, GAMEPAD_PADDLE_LEFT_BUTTON)) {
      setThrottleGear(currentGear - 1);
      recordEvent('gamepad paddle', 'left');
    }
    if (getGamepadButtonRisingEdge(gamepad, GAMEPAD_PADDLE_RIGHT_BUTTON)) {
      setThrottleGear(currentGear + 1);
      recordEvent('gamepad paddle', 'right');
    }
    if (GAMEPAD_FFB_PRESET_BUTTON >= 0 && getGamepadButtonRisingEdge(gamepad, GAMEPAD_FFB_PRESET_BUTTON)) {
      cycleFfbPreset();
    }
    if (rcDriveEnabled) {
      applyGamepadCommand(gamepad);
    } else {
      lastGamepadStatus = `gp#${gamepad.index} ready`;
    }
    updateControlUiMode();
  }

  function startGamepadPoller() {
    if (!GAMEPAD_ENABLED) {
      return;
    }
    window.setInterval(pollGamepad, RC_TX_INTERVAL_MS);
  }

  function onControlKeyDown(event) {
    if (!rcDriveEnabled || isTextEditingTarget(event.target) || event.repeat) {
      return;
    }
    const key = getControlKey(event.code);
    if (!key) {
      return;
    }
    event.preventDefault();
    pressedControlKeys.add(key);
    applyKeyboardCommand();
  }

  function onControlKeyUp(event) {
    if (!rcDriveEnabled || isTextEditingTarget(event.target)) {
      return;
    }
    const key = getControlKey(event.code);
    if (!key) {
      return;
    }
    event.preventDefault();
    pressedControlKeys.delete(key);
    applyKeyboardCommand();
  }

  function clearReconnectTimer() {
    if (!reconnectTimer) {
      return;
    }
    window.clearTimeout(reconnectTimer);
    reconnectTimer = null;
    reconnectAfter = 0;
  }

  function isConnectionActive() {
    return Boolean(ws || peerConnection || reconnectTimer);
  }

  function isDataChannelOpen() {
    return Boolean(dataChannel && dataChannel.readyState === 'open');
  }

  function roomLockActive() {
    return ROOM_LOCK_ENABLED && isAyameSignaling() && Boolean(ROOM_LOCK_URL) && Boolean(AYAME_ROOM_ID);
  }

  function roomLockEndpoint(suffix = '') {
    const room = encodeURIComponent(AYAME_ROOM_ID);
    return `${ROOM_LOCK_URL}/rooms/${room}${suffix}`;
  }

  function getRoomLeaseToken() {
    return roomLease?.token || '';
  }

  function isRoomLockedByOther() {
    if (!roomLockActive() || !roomLockStatus || roomLease) {
      return false;
    }
    const holder = roomLockStatus.lease;
    return Boolean(roomLockStatus.locked && holder?.clientId && holder.clientId !== AYAME_CLIENT_ID);
  }

  async function fetchRoomLockJson(url, options = {}) {
    const response = await fetch(url, {
      cache: 'no-store',
      ...options,
      headers: {
        ...(options.headers || {}),
      },
    });
    const payload = await response.json().catch(() => ({}));
    if (!response.ok || payload.ok === false) {
      const error = new Error(payload.error || `HTTP ${response.status}`);
      error.payload = payload;
      error.status = response.status;
      throw error;
    }
    return payload;
  }

  async function refreshRoomLockStatus() {
    if (!roomLockActive()) {
      return;
    }
    if (isDataChannelOpen()) {
      if (!roomLease && !roomLockBusy) {
        recordEvent('room lease recover', 'active connection');
        acquireRoomLease();
      }
      return;
    }
    if (isConnectionActive()) {
      return;
    }
    try {
      roomLockStatus = await fetchRoomLockJson(roomLockEndpoint());
      if (isRoomLockedByOther()) {
        const holder = roomLockStatus.lease?.clientId || 'other';
        recordEvent('room busy', holder);
      }
    } catch (error) {
      roomLockStatus = { ok: false, locked: false, error: error.message || String(error) };
      recordEvent('room lock status failed', roomLockStatus.error);
    } finally {
      updateUiState();
    }
  }

  function startRoomLockStatusMonitor() {
    if (!roomLockActive() || roomLockStatusTimer) {
      return;
    }
    refreshRoomLockStatus();
    roomLockStatusTimer = window.setInterval(refreshRoomLockStatus, ROOM_LOCK_POLL_MS);
  }

  function stopRoomLockHeartbeat() {
    if (!roomLockHeartbeatTimer) {
      return;
    }
    window.clearInterval(roomLockHeartbeatTimer);
    roomLockHeartbeatTimer = null;
  }

  function clearRoomLease(reason) {
    if (!roomLease) {
      return;
    }
    stopRoomLockHeartbeat();
    roomLease = null;
    roomLockHeartbeatFailures = 0;
    recordEvent('room lease cleared', reason);
  }

  async function heartbeatRoomLease() {
    if (!roomLockActive() || !roomLease) {
      return false;
    }
    const leaseToken = getRoomLeaseToken();
    try {
      const payload = await fetchRoomLockJson(roomLockEndpoint('/heartbeat'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          clientId: AYAME_CLIENT_ID,
          token: leaseToken,
          ttlSec: ROOM_LOCK_TTL_SEC,
          driveEnabled: rcDriveEnabled,
        }),
      });
      if (!roomLease || getRoomLeaseToken() !== leaseToken) {
        return true;
      }
      const token = getRoomLeaseToken();
      roomLease = payload.lease ? { ...payload.lease, token } : roomLease;
      roomLockStatus = payload;
      roomLockHeartbeatFailures = 0;
      return true;
    } catch (error) {
      if (!roomLease || getRoomLeaseToken() !== leaseToken) {
        return true;
      }
      recordEvent('room heartbeat failed', error.message || String(error));
      roomLockStatus = error.payload || roomLockStatus;
      roomLockHeartbeatFailures += 1;
      if (isConnectionActive()) {
        if (error.status === 409) {
          stopRoomLockHeartbeat();
          roomLease = null;
          roomLockHeartbeatFailures = 0;
          recordEvent('room lease recover', 'heartbeat mismatch');
          acquireRoomLease();
        }
        return true;
      }
      if (error.status === 409 || roomLockHeartbeatFailures >= ROOM_LOCK_HEARTBEAT_MAX_FAILURES) {
        clearRoomLease(error.message || 'heartbeat failed');
      }
      return false;
    }
  }

  function startRoomLockHeartbeat() {
    if (!roomLockActive() || roomLockHeartbeatTimer) {
      return;
    }
    const intervalMs = Math.max(3000, Math.min(10000, Math.floor((ROOM_LOCK_TTL_SEC * 1000) / 3)));
    roomLockHeartbeatTimer = window.setInterval(heartbeatRoomLease, intervalMs);
  }

  async function acquireRoomLease() {
    if (!roomLockActive()) {
      return true;
    }
    if (roomLease) {
      const valid = await heartbeatRoomLease();
      if (valid) {
        startRoomLockHeartbeat();
        return true;
      }
    }

    roomLockBusy = true;
    recordEvent('room lock', 'acquire');
    updateUiState();
    try {
      const payload = await fetchRoomLockJson(roomLockEndpoint('/lease'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          clientId: AYAME_CLIENT_ID,
          ttlSec: ROOM_LOCK_TTL_SEC,
          displayName: getStringParam(['id'], AYAME_CLIENT_ID),
          userAgent: navigator.userAgent,
          driveEnabled: rcDriveEnabled,
        }),
      });
      roomLease = payload.lease || null;
      roomLockStatus = payload;
      roomLockHeartbeatFailures = 0;
      startRoomLockHeartbeat();
      recordEvent('room lock', 'acquired');
      return true;
    } catch (error) {
      roomLockStatus = error.payload || { ok: false, locked: true, error: error.message || String(error) };
      const holder = roomLockStatus.lease?.clientId || 'other';
      recordEvent('room lock denied', holder);
      return false;
    } finally {
      roomLockBusy = false;
      updateUiState();
    }
  }

  function releaseRoomLease(options = {}) {
    if (!roomLockActive() || !roomLease) {
      return;
    }
    const payload = {
      clientId: AYAME_CLIENT_ID,
      token: getRoomLeaseToken(),
    };
    const url = roomLockEndpoint('/release');
    stopRoomLockHeartbeat();
    roomLease = null;
    roomLockStatus = null;

    if (options.beacon && navigator.sendBeacon) {
      const blob = new Blob([JSON.stringify(payload)], { type: 'application/json' });
      navigator.sendBeacon(url, blob);
      return;
    }
    fetchRoomLockJson(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    }).then((status) => {
      roomLockStatus = status;
      updateUiState();
    }).catch((error) => {
      recordEvent('room release failed', error.message || String(error));
    });
  }

  function closeTransport(options = {}) {
    const sendSignalingClose = options.sendSignalingClose === true;
    const currentWs = ws;
    const currentDataChannel = dataChannel;
    const currentTelemetryChannel = telemetryChannel;
    const currentRaceChannel = raceChannel;
    const currentPeerConnection = peerConnection;

    if (currentWs) {
      currentWs.onopen = null;
      currentWs.onerror = null;
      currentWs.onclose = null;
      currentWs.onmessage = null;
    }
    if (currentDataChannel) {
      currentDataChannel.onopen = null;
      currentDataChannel.onclose = null;
      currentDataChannel.onmessage = null;
    }
    if (currentTelemetryChannel) {
      currentTelemetryChannel.onopen = null;
      currentTelemetryChannel.onclose = null;
      currentTelemetryChannel.onmessage = null;
    }
    if (currentRaceChannel) {
      currentRaceChannel.onopen = null;
      currentRaceChannel.onclose = null;
      currentRaceChannel.onmessage = null;
    }
    if (currentPeerConnection) {
      currentPeerConnection.ontrack = null;
      currentPeerConnection.onicecandidate = null;
      currentPeerConnection.oniceconnectionstatechange = null;
      currentPeerConnection.onconnectionstatechange = null;
    }

    if (sendSignalingClose &&
        currentWs &&
        currentWs.readyState === WebSocket.OPEN) {
      try {
        currentWs.send(JSON.stringify({ type: isAyameSignaling() ? 'bye' : 'close' }));
      } catch (_) {
      }
    }
    if (currentDataChannel) {
      try {
        currentDataChannel.close();
      } catch (_) {
      }
    }
    if (currentTelemetryChannel) {
      try {
        currentTelemetryChannel.close();
      } catch (_) {
      }
    }
    if (currentRaceChannel) {
      try {
        currentRaceChannel.close();
      } catch (_) {
      }
    }
    if (currentPeerConnection) {
      try {
        currentPeerConnection.close();
      } catch (_) {
      }
    }
    if (currentWs &&
        (currentWs.readyState === WebSocket.CONNECTING ||
         currentWs.readyState === WebSocket.OPEN)) {
      try {
        currentWs.close(1000, 'viewer closing');
      } catch (_) {
      }
    }
    dataChannel = null;
    telemetryChannel = null;
    raceChannel = null;
    audioSender = null;
    peerConnection = null;
    ws = null;
    candidates = [];
    hasReceivedSdp = false;
    lastStatsSampleAt = 0;
    lastBytesReceived = 0;
    lastPacketsReceived = 0;
    lastPacketsLost = 0;
    lastFramesDropped = 0;
    lastJitterBufferDelay = 0;
    lastJitterBufferEmittedCount = 0;
    lastTotalProcessingDelay = 0;
    lastFramesDecoded = 0;
    lastDecodedFrameAt = 0;
    decodedFrameHistory = [];
    remoteVideo.pause();
    remoteVideo.srcObject = null;
    updateUiState();
  }

  function disconnect() {
    shouldReconnect = false;
    reconnectAttempt = 0;
    reconnectReason = '';
    lastEvent = 'manual stop';
    connectedAt = 0;
    clearReconnectTimer();
    setDriveEnabled(false);
    micEnabled = false;
    stopLocalMic();
    closeTransport({ sendSignalingClose: true });
    releaseRoomLease();
  }

  function shutdownForPageHide() {
    stopFfbOutput();
    ffbShuttingDown = true;
    ffbClient?.disconnect();
    if (ffbReconnectTimer) {
      window.clearTimeout(ffbReconnectTimer);
      ffbReconnectTimer = 0;
    }
    shouldReconnect = false;
    reconnectAttempt = 0;
    reconnectReason = '';
    lastEvent = 'page hide';
    clearReconnectTimer();
    setDriveEnabled(false);
    micEnabled = false;
    stopLocalMic();
    closeTransport({ sendSignalingClose: true });
    releaseRoomLease({ beacon: true });
  }

  function scheduleReconnect(reason, options = {}) {
    const force = options.force === true;
    if (!AUTO_RECONNECT && !force) {
      recordEvent('reconnect blocked', reason);
      return;
    }
    if (!shouldReconnect || reconnectTimer) {
      return;
    }
    if (reason === 'video lost') {
      eventCounters.videoLost += 1;
    } else if (reason === 'no video') {
      eventCounters.noVideo += 1;
    } else if (reason === 'ws closed') {
      eventCounters.wsClosed += 1;
    } else if (reason === 'peer closed') {
      eventCounters.peerClosed += 1;
    } else if (reason === 'dc closed') {
      eventCounters.dcClosed += 1;
    } else if (reason === 'room full') {
      eventCounters.roomFull += 1;
    } else if (reason === 'ice failed') {
      eventCounters.iceFailed += 1;
    } else if (reason === 'pc failed') {
      eventCounters.pcFailed += 1;
    }
    lastReconnectAt = performance.now();
    lastReconnectReason = reason;
    reconnectCount += 1;
    reconnectAttempt += 1;
    reconnectReason = reason;
    const baseDelayMs = options.baseDelayMs || RECONNECT_BASE_DELAY_MS;
    const maxDelayMs = options.maxDelayMs || RECONNECT_MAX_DELAY_MS;
    const delay = Math.min(
      baseDelayMs * (2 ** Math.min(reconnectAttempt - 1, 4)),
      maxDelayMs,
    );
    reconnectAfter = performance.now() + delay;
    console.warn('Scheduling reconnect:', reason, `${delay}ms`);
    recordEvent('reconnect', `${reason} ${delay}ms ${getTransportSummary()}`);
    closeTransport({ sendSignalingClose: true });
    updateUiState();
    reconnectTimer = window.setTimeout(() => {
      reconnectTimer = null;
      reconnectAfter = 0;
      connect({ isAutoReconnect: true }).catch((error) => {
        recordEvent('connect failed', error.message || String(error));
        updateUiState();
      });
    }, delay);
  }

  function createWebSocketUrl() {
    const host = endpointInput.value.trim() || DEFAULT_HOST;
    const protocol = location.protocol === 'https:' ? 'wss://' : 'ws://';
    if (!isRelaySignaling()) {
      return `${protocol}${host}/ws`;
    }
    const query = new URLSearchParams({ role: 'pilot' });
    const device = getRelayDevice();
    if (device) {
      query.set('device', device);
    }
    return `${protocol}${host}/ws?${query}`;
  }

  function createSignalingWebSocketUrl() {
    return isAyameSignaling() ? AYAME_SIGNALING_URL : createWebSocketUrl();
  }

  function sendAyameRegister() {
    if (!AYAME_ROOM_ID) {
      lastEvent = 'ayame room missing';
      recordEvent('ayame error', 'roomId is required');
      updateUiState();
      return;
    }
    const message = {
      type: 'register',
      roomId: AYAME_ROOM_ID,
      clientId: AYAME_CLIENT_ID,
    };
    if (AYAME_SIGNALING_KEY) {
      message.key = AYAME_SIGNALING_KEY;
    }
    ws.send(JSON.stringify(message));
  }

  function normalizeIceServers(iceServers) {
    if (!Array.isArray(iceServers)) {
      return [];
    }
    return iceServers
      .map((server) => {
        if (!server || !server.urls) {
          return null;
        }
        return {
          urls: server.urls,
          username: server.username,
          credential: server.credential,
        };
      })
      .filter(Boolean);
  }

  function hasTurnUrl(server) {
    const urls = Array.isArray(server.urls) ? server.urls : [server.urls];
    return urls.some((url) => {
      const normalized = String(url || '').toLowerCase();
      return normalized.startsWith('turn:') || normalized.startsWith('turns:');
    });
  }

  function configuredStunIceServers() {
    return STUN_URLS.length > 0 ? [{ urls: STUN_URLS }] : [];
  }

  function configuredTurnIceServers() {
    if (TURN_URLS.length === 0) {
      return [];
    }
    const server = { urls: TURN_URLS };
    if (TURN_USERNAME && TURN_CREDENTIAL) {
      server.username = TURN_USERNAME;
      server.credential = TURN_CREDENTIAL;
    }
    return [server];
  }

  function defaultIceServers() {
    return getBooleanParam('stun', isAyameSignaling())
      ? configuredStunIceServers()
      : [];
  }

  function resolveIceServers(iceServers) {
    const normalizedIceServers = normalizeIceServers(iceServers);
    if (ICE_MODE === 'none') {
      return [];
    }
    if (ICE_MODE === 'stun') {
      return configuredStunIceServers();
    }
    if (ICE_MODE === 'turn') {
      const configuredTurnServers = configuredTurnIceServers();
      if (configuredTurnServers.length > 0) {
        return configuredTurnServers;
      }
      return normalizedIceServers.filter(hasTurnUrl);
    }
    return normalizedIceServers.length > 0
      ? normalizedIceServers
      : defaultIceServers();
  }

  function sendSignalingDescription(description) {
    if (!ws || ws.readyState !== WebSocket.OPEN || !description) {
      return;
    }
    ws.send(JSON.stringify({
      type: description.type,
      sdp: description.sdp,
    }));
  }

  async function setOfferAndAnswer(offer) {
    peerConnection = createPeerConnection({ iceServers: ayameIceServers });
    updateUiState();
    try {
      await peerConnection.setRemoteDescription(offer);
      hasReceivedSdp = true;
      const answer = await peerConnection.createAnswer();
      await peerConnection.setLocalDescription(answer);
      sendSignalingDescription(peerConnection.localDescription);
      for (const candidate of candidates) {
        addIceCandidate(candidate);
      }
      candidates = [];
      updateUiState();
    } catch (error) {
      console.error('setOfferAndAnswer failed:', error);
      recordEvent('answer failed', error?.message || 'unknown');
    }
  }

  function getAyameRejectReason(message) {
    return String(message.reason || message.error || message.message || 'unknown');
  }

  function isRoomFullReject(message) {
    const reason = getAyameRejectReason(message).toLowerCase();
    return reason === 'full' ||
      reason === 'roomfilled' ||
      reason.includes('room full') ||
      reason.includes('roomfilled');
  }

  function isRoomLockReject(message) {
    const reason = getAyameRejectReason(message).toLowerCase();
    return reason.includes('room lock') || reason.includes('lock required');
  }

  function handleAyameMessage(message) {
    switch (message.type) {
      case 'accept':
        ayameIceServers = normalizeIceServers(message.iceServers);
        recordEvent('ayame accept', message.isExistUser ? 'peer exists' : 'waiting');
        if (message.isExistUser || typeof message.isExistUser === 'undefined') {
          makeOffer({ iceServers: ayameIceServers });
        }
        break;
      case 'offer':
        setOfferAndAnswer(new RTCSessionDescription({
          type: 'offer',
          sdp: message.sdp,
        }));
        break;
      case 'answer':
        setAnswer(new RTCSessionDescription({
          type: 'answer',
          sdp: message.sdp,
        }));
        break;
      case 'candidate': {
        const candidate = new RTCIceCandidate(message.ice);
        if (hasReceivedSdp) {
          addIceCandidate(candidate);
        } else {
          candidates.push(candidate);
        }
        break;
      }
      case 'ping':
        ws.send(JSON.stringify({ type: 'pong' }));
        break;
      case 'bye':
        recordEvent('ayame bye');
        scheduleReconnect('peer closed');
        break;
      case 'reject':
        recordEvent('ayame reject', getAyameRejectReason(message));
        if (isRoomLockReject(message)) {
          clearRoomLease(getAyameRejectReason(message));
          scheduleReconnect('room lock lost', {
            force: true,
            baseDelayMs: 1000,
            maxDelayMs: 5000,
          });
        } else if (isRoomFullReject(message)) {
          scheduleReconnect('room full', {
            force: true,
            baseDelayMs: ROOM_FULL_RETRY_BASE_DELAY_MS,
            maxDelayMs: ROOM_FULL_RETRY_MAX_DELAY_MS,
          });
        }
        break;
      default:
        console.warn('Unknown Ayame message:', message.type);
    }
  }

  async function connect(options = {}) {
    if (roomLockBusy) {
      return;
    }
    if (roomLockActive()) {
      const acquired = await acquireRoomLease();
      if (!acquired) {
        shouldReconnect = false;
        return;
      }
    }
    shouldReconnect = true;
    if (!options.isAutoReconnect) {
      reconnectAttempt = 0;
      reconnectCount = 0;
    }
    recordEvent(options.isAutoReconnect ? 'connect auto' : 'connect manual');
    reconnectReason = '';
    clearReconnectTimer();
    closeTransport({ sendSignalingClose: false });
    connectStartedAt = performance.now();
    lastVideoFrameAt = 0;
    lastDecodedFrameAt = 0;

    const wsUrl = createSignalingWebSocketUrl();
    ws = new WebSocket(wsUrl);
    updateUiState();

    ws.onopen = () => {
      console.log('WebSocket open:', wsUrl);
      lastEvent = 'ws open';
      if (isAyameSignaling()) {
        sendAyameRegister();
      } else {
        makeOffer();
      }
      updateUiState();
    };

    ws.onerror = (error) => {
      console.error('WebSocket error:', error);
      eventCounters.wsError += 1;
      recordEvent('ws error', error?.message || 'unknown');
      updateUiState();
    };

    ws.onclose = (event) => {
      lastWsClose = `${event.code || 0} ${event.reason || ''}`.trim();
      recordEvent('ws close', lastWsClose);
      scheduleReconnect('ws closed');
      updateUiState();
    };

    ws.onmessage = (event) => {
      const message = JSON.parse(event.data);
      if (isAyameSignaling()) {
        handleAyameMessage(message);
        return;
      }
      switch (message.type) {
        case 'answer':
          setAnswer(new RTCSessionDescription(message));
          break;
        case 'candidate': {
          const candidate = new RTCIceCandidate(message.ice);
          if (hasReceivedSdp) {
            addIceCandidate(candidate);
          } else {
            candidates.push(candidate);
          }
          break;
        }
        case 'close':
          console.log('Peer closed');
          recordEvent('peer close');
          scheduleReconnect('peer closed');
          break;
        default:
          console.warn('Unknown signaling message:', message.type);
      }
    };
  }

  function createPeerConnection(options = {}) {
    candidates = [];
    hasReceivedSdp = false;

    const rtcConfig = {
      iceServers: resolveIceServers(options.iceServers),
    };
    if (ICE_MODE === 'turn') {
      rtcConfig.iceTransportPolicy = 'relay';
    }
    const peer = new RTCPeerConnection(rtcConfig);
    const attachDataChannel = (channel) => {
      if (dataChannel && dataChannel !== channel) {
        dataChannel.onopen = null;
        dataChannel.onclose = null;
        dataChannel.onmessage = null;
      }
      dataChannel = channel;
      dataChannel.onopen = () => {
        connectedAt = performance.now();
        pendingDcPings.clear();
        dcRttMs = null;
        lastDcPongAt = 0;
        recordEvent('dc open');
        sendDcPing();
        updateUiState();
      };
      dataChannel.onclose = () => {
        recordEvent('dc close');
        scheduleReconnect('dc closed');
        updateUiState();
      };
      dataChannel.onmessage = usesRelayTransport()
        ? () => {}
        : (event) => handleDataChannelMessage(event.data);
      if (dataChannel.readyState === 'open') {
        connectedAt = performance.now();
        pendingDcPings.clear();
        dcRttMs = null;
        lastDcPongAt = 0;
        recordEvent('dc open');
        sendDcPing();
      }
      updateUiState();
    };

    const attachTelemetryChannel = (channel) => {
      if (telemetryChannel && telemetryChannel !== channel) {
        telemetryChannel.onopen = null;
        telemetryChannel.onclose = null;
        telemetryChannel.onmessage = null;
      }
      telemetryChannel = channel;
      telemetryChannel.onopen = () => {
        recordEvent('telemetry dc open');
        updateUiState();
      };
      telemetryChannel.onclose = () => {
        recordEvent('telemetry dc close');
        scheduleReconnect('dc closed');
        updateUiState();
      };
      telemetryChannel.onmessage = (event) => handleDataChannelMessage(event.data);
      updateUiState();
    };

    const attachRaceChannel = (channel) => {
      if (raceChannel && raceChannel !== channel) {
        raceChannel.onopen = null;
        raceChannel.onclose = null;
        raceChannel.onmessage = null;
      }
      raceChannel = channel;
      raceChannel.onopen = () => {
        recordEvent('race dc open');
      };
      raceChannel.onclose = () => {
        recordEvent('race dc close');
      };
      raceChannel.onmessage = (event) => handleRaceStateMessage(event.data);
    };

    peer.ondatachannel = (event) => {
      if (event.channel.label === 'momo-race') {
        attachRaceChannel(event.channel);
      } else if (event.channel.label === 'momo-telemetry') {
        attachTelemetryChannel(event.channel);
      } else {
        attachDataChannel(event.channel);
      }
    };

    attachDataChannel(peer.createDataChannel(usesRelayTransport() ? 'momo-command' : 'serial', {
      ordered: false,
      maxRetransmits: 0,
    }));
    if (usesRelayTransport()) {
      attachTelemetryChannel(peer.createDataChannel('momo-telemetry', {
        ordered: false,
        maxRetransmits: 0,
      }));
      attachRaceChannel(peer.createDataChannel('momo-race', {
        ordered: true,
      }));
    }

    const mediaStream = new MediaStream();
    remoteVideo.srcObject = mediaStream;
    peer.ontrack = (event) => {
      mediaStream.addTrack(event.track);
      remoteVideo.play().catch((error) => console.warn('video play failed:', error));
      updateUiState();
    };

    peer.onicecandidate = (event) => {
      if (!event.candidate || !ws || ws.readyState !== WebSocket.OPEN) {
        return;
      }
      ws.send(JSON.stringify({ type: 'candidate', ice: event.candidate }));
    };

    peer.oniceconnectionstatechange = () => {
      if (peer.iceConnectionState === 'connected' ||
          peer.iceConnectionState === 'completed') {
        recordEvent('ice connected', peer.iceConnectionState);
      }
      if (peer.iceConnectionState === 'failed') {
        recordEvent('ice failed');
        scheduleReconnect('ice failed');
      }
      updateUiState();
    };
    peer.onconnectionstatechange = () => {
      if (peer.connectionState === 'failed') {
        recordEvent('pc failed');
        scheduleReconnect('pc failed');
      }
      updateUiState();
    };

    const videoTransceiver = peer.addTransceiver('video', { direction: 'recvonly' });
    preferVideoCodec(videoTransceiver, getPreferredVideoCodec());
    if (!usesRelayTransport()) {
      const audioTransceiver = peer.addTransceiver('audio', { direction: 'sendrecv' });
      audioSender = audioTransceiver.sender;
      attachMicTrackToSender().catch((error) => {
        recordEvent('mic attach failed', error.message || String(error));
      });
    }

    return peer;
  }

  function getPreferredVideoCodec() {
    const params = getUrlParams();
    const codec = (params.get('codec') || 'h264').toLowerCase();
    if (codec === 'vp8') {
      return 'video/vp8';
    }
    if (codec === 'vp9') {
      return 'video/vp9';
    }
    return 'video/h264';
  }

  function preferVideoCodec(transceiver, mimeType) {
    if (!transceiver || typeof transceiver.setCodecPreferences !== 'function') {
      return;
    }
    if (typeof RTCRtpSender === 'undefined' || !RTCRtpSender.getCapabilities) {
      return;
    }
    const capabilities = RTCRtpSender.getCapabilities('video');
    if (!capabilities || !Array.isArray(capabilities.codecs)) {
      return;
    }
    const primaryCodecs = capabilities.codecs.filter((codec) => {
      const mime = (codec.mimeType || '').toLowerCase();
      return mime.startsWith('video/') && !/(\/rtx|\/red|\/ulpfec|\/flexfec)/.test(mime);
    });
    const selected = primaryCodecs.filter((codec) => {
      return (codec.mimeType || '').toLowerCase() === mimeType;
    });
    if (selected.length === 0) {
      return;
    }
    const others = primaryCodecs.filter((codec) => {
      return (codec.mimeType || '').toLowerCase() !== mimeType;
    });
    transceiver.setCodecPreferences(selected.concat(others));
  }

  async function makeOffer(options = {}) {
    peerConnection = createPeerConnection(options);
    updateUiState();
    try {
      const offer = await peerConnection.createOffer();
      await peerConnection.setLocalDescription(offer);
      sendSignalingDescription(peerConnection.localDescription);
    } catch (error) {
      console.error('makeOffer failed:', error);
    }
  }

  async function setAnswer(answer) {
    if (!peerConnection) {
      return;
    }
    try {
      await peerConnection.setRemoteDescription(answer);
      hasReceivedSdp = true;
      for (const candidate of candidates) {
        addIceCandidate(candidate);
      }
      candidates = [];
      updateUiState();
    } catch (error) {
      console.error('setAnswer failed:', error);
    }
  }

  function addIceCandidate(candidate) {
    if (!peerConnection) {
      return;
    }
    peerConnection.addIceCandidate(candidate).catch((error) => {
      console.warn('addIceCandidate failed:', error);
    });
  }

  function startLinkMonitor() {
    window.setInterval(() => {
      updateTimerUi();
      if (!shouldReconnect || reconnectTimer || !peerConnection) {
        return;
      }
      if (document.hidden) {
        return;
      }
      const now = performance.now();
      if (now - visibleSince < CONNECT_GRACE_MS) {
        return;
      }
      if (isVideoFrozen()) {
        if (AUTO_RECONNECT_ON_VIDEO_LOST) {
          scheduleReconnect('video lost');
        } else {
          recordEvent('video lost', 'auto reconnect disabled');
        }
        return;
      }
      const hasNoVideo =
        remoteVideo.videoWidth <= 0 &&
        connectStartedAt > 0 &&
        now - connectStartedAt > CONNECT_GRACE_MS;
      if (hasNoVideo) {
        scheduleReconnect('no video');
      }
    }, 500);
  }

  async function sampleWebRtcStats() {
    if (!peerConnection) {
      setText(netState, '0kbps');
      setText(jitterState, '0ms');
      setText(rttState, 'n/a');
      setText(latencyState, 'n/a');
      updateDecodedFps(0);
      decodedFrameHistory = [];
      return;
    }

    const stats = await peerConnection.getStats();
    let inboundVideo = null;
    let selectedPair = null;
    stats.forEach((report) => {
      if (report.type === 'inbound-rtp' && report.kind === 'video') {
        inboundVideo = report;
      }
      if (report.type === 'transport' && report.selectedCandidatePairId) {
        selectedPair = stats.get(report.selectedCandidatePairId);
      }
      if (report.type === 'candidate-pair' && report.nominated && report.state === 'succeeded') {
        selectedPair = report;
      }
    });
    if (!inboundVideo) {
      return;
    }

    const now = performance.now();
    const bytesReceived = inboundVideo.bytesReceived || 0;
    const packetsReceived = inboundVideo.packetsReceived || 0;
    const packetsLost = inboundVideo.packetsLost || 0;
    const framesDropped = inboundVideo.framesDropped || 0;
    const jitterBufferDelay = Number.isFinite(inboundVideo.jitterBufferDelay)
      ? inboundVideo.jitterBufferDelay
      : null;
    const jitterBufferEmittedCount = Number.isFinite(inboundVideo.jitterBufferEmittedCount)
      ? inboundVideo.jitterBufferEmittedCount
      : null;
    const totalProcessingDelay = Number.isFinite(inboundVideo.totalProcessingDelay)
      ? inboundVideo.totalProcessingDelay
      : null;
    const framesDecoded = Number.isFinite(inboundVideo.framesDecoded)
      ? inboundVideo.framesDecoded
      : null;

    if (lastStatsSampleAt === 0) {
      lastStatsSampleAt = now;
      lastBytesReceived = bytesReceived;
      lastPacketsReceived = packetsReceived;
      lastPacketsLost = packetsLost;
      lastFramesDropped = framesDropped;
      lastJitterBufferDelay = jitterBufferDelay || 0;
      lastJitterBufferEmittedCount = jitterBufferEmittedCount || 0;
      lastTotalProcessingDelay = totalProcessingDelay || 0;
      lastFramesDecoded = framesDecoded || 0;
      lastDecodedFrameAt = framesDecoded && framesDecoded > 0 ? now : 0;
      decodedFrameHistory = framesDecoded === null ? [] : [{ now, framesDecoded }];
      return;
    }

    const elapsedSeconds = Math.max((now - lastStatsSampleAt) / 1000, 0.001);
    const bitrateKbps =
      Math.max(0, ((bytesReceived - lastBytesReceived) * 8) / elapsedSeconds / 1000);
    const receivedDelta = Math.max(0, packetsReceived - lastPacketsReceived);
    const lostDelta = Math.max(0, packetsLost - lastPacketsLost);
    const totalDelta = receivedDelta + lostDelta;
    const lossPercent = totalDelta > 0 ? (lostDelta * 100) / totalDelta : 0;
    const jitterMs = Math.max(0, (inboundVideo.jitter || 0) * 1000);
    const droppedDelta = Math.max(0, framesDropped - lastFramesDropped);
    const totalLossPercent =
      packetsReceived + packetsLost > 0 ? (packetsLost * 100) / (packetsReceived + packetsLost) : 0;
    const rttMs = selectedPair && Number.isFinite(selectedPair.currentRoundTripTime)
      ? selectedPair.currentRoundTripTime * 1000
      : null;
    const jitterBufferDelayDelta = jitterBufferDelay === null
      ? null
      : jitterBufferDelay - lastJitterBufferDelay;
    const jitterBufferEmittedDelta = jitterBufferEmittedCount === null
      ? null
      : jitterBufferEmittedCount - lastJitterBufferEmittedCount;
    const processingDelayDelta = totalProcessingDelay === null
      ? null
      : totalProcessingDelay - lastTotalProcessingDelay;
    const framesDecodedDelta = framesDecoded === null
      ? null
      : framesDecoded - lastFramesDecoded;
    if (framesDecodedDelta !== null && framesDecodedDelta > 0) {
      lastDecodedFrameAt = now;
    }
    const decodedFps = updateDecodedFrameHistory(now, framesDecoded);
    const playoutDelayMs =
      jitterBufferDelayDelta !== null &&
      jitterBufferEmittedDelta !== null &&
      jitterBufferEmittedDelta > 0
        ? (jitterBufferDelayDelta / jitterBufferEmittedDelta) * 1000
        : null;
    const decodeDelayMs =
      processingDelayDelta !== null &&
      framesDecodedDelta !== null &&
      framesDecodedDelta > 0
        ? (processingDelayDelta / framesDecodedDelta) * 1000
        : null;
    const estimatedDelayParts = [];
    if (rttMs !== null) {
      estimatedDelayParts.push(rttMs / 2);
    }
    if (playoutDelayMs !== null) {
      estimatedDelayParts.push(playoutDelayMs);
    }
    if (decodeDelayMs !== null) {
      estimatedDelayParts.push(decodeDelayMs);
    }
    const estimatedDelayMs = estimatedDelayParts.length > 0
      ? estimatedDelayParts.reduce((sum, value) => sum + value, 0)
      : null;

    setText(netState, `${bitrateKbps.toFixed(0)}kbps ${lossPercent.toFixed(1)}%`);
    if (decodedFps !== null) {
      updateDecodedFps(decodedFps);
    }
    setText(jitterState, `${jitterMs.toFixed(1)}ms d${droppedDelta}`);
    setText(rttState, rttMs === null ? 'n/a' : `${rttMs.toFixed(1)}ms`);
    setText(latencyState, formatLatency(estimatedDelayMs, playoutDelayMs, decodeDelayMs));
    setText(dropState, `${framesDropped} L${packetsLost} ${totalLossPercent.toFixed(1)}%`);

    lastStatsSampleAt = now;
    lastBytesReceived = bytesReceived;
    lastPacketsReceived = packetsReceived;
    lastPacketsLost = packetsLost;
    lastFramesDropped = framesDropped;
    lastJitterBufferDelay = jitterBufferDelay || 0;
    lastJitterBufferEmittedCount = jitterBufferEmittedCount || 0;
    lastTotalProcessingDelay = totalProcessingDelay || 0;
    lastFramesDecoded = framesDecoded || 0;
  }

  function formatLatency(estimatedDelayMs, playoutDelayMs, decodeDelayMs) {
    if (estimatedDelayMs === null) {
      return 'n/a';
    }
    const parts = [`~${estimatedDelayMs.toFixed(0)}ms`];
    if (playoutDelayMs !== null) {
      parts.push(`buf${playoutDelayMs.toFixed(0)}`);
    }
    if (decodeDelayMs !== null) {
      parts.push(`dec${decodeDelayMs.toFixed(0)}`);
    }
    return parts.join(' ');
  }

  function updateDecodedFrameHistory(now, framesDecoded) {
    if (framesDecoded === null) {
      return null;
    }
    decodedFrameHistory.push({ now, framesDecoded });
    const cutoff = now - 3500;
    while (decodedFrameHistory.length > 2 && decodedFrameHistory[0].now < cutoff) {
      decodedFrameHistory.shift();
    }
    const first = decodedFrameHistory[0];
    const last = decodedFrameHistory[decodedFrameHistory.length - 1];
    if (!first || !last || first === last) {
      return null;
    }
    const elapsedSeconds = (last.now - first.now) / 1000;
    const frameDelta = last.framesDecoded - first.framesDecoded;
    if (elapsedSeconds <= 0 || frameDelta < 0) {
      return null;
    }
    return frameDelta / elapsedSeconds;
  }

  function startStatsMonitor() {
    window.setInterval(() => {
      sampleWebRtcStats().catch((error) => {
        console.warn('getStats failed:', error);
      });
    }, 1000);
  }

  function startOsdMonitor() {
    window.setInterval(updateTimerUi, OSD_UPDATE_INTERVAL_MS);
  }

  function getEndpointHostName() {
    return (endpointInput.value.trim() || DEFAULT_HOST).split(':')[0];
  }

  function createDeviceStatusUrl() {
    const params = getUrlParams();
    const statusUrl = params.get('statusUrl');
    if (statusUrl) {
      return statusUrl;
    }
    const host = getStatusApiHost();
    return `http://${host}:8090/status`;
  }

  function getStatusApiHost() {
    const configured = getStringParam(['statusHost', 'deviceHost']);
    if (configured) {
      return configured.split(':')[0];
    }
    return getEndpointHostName();
  }

  function formatDeviceTime(date) {
    const pad = (value) => String(value).padStart(2, '0');
    const month = pad(date.getMonth() + 1);
    const day = pad(date.getDate());
    const hour = pad(date.getHours());
    const minute = pad(date.getMinutes());
    const second = pad(date.getSeconds());
    return `${month}/${day} ${hour}:${minute}:${second}`;
  }

  function updateDeviceTimeUi() {
    if (!deviceClock) {
      return;
    }
    const elapsed = performance.now() - deviceClock.sampledAt;
    setText(timeState, formatDeviceTime(new Date(deviceClock.epochMs + elapsed)));
  }

  function applyDeviceInfo(status) {
    updateHostUi(status.hostname || getEndpointHostName());

    if (Number.isFinite(status.unix_time)) {
      deviceClock = {
        epochMs: status.unix_time * 1000,
        sampledAt: performance.now(),
      };
      updateDeviceTimeUi();
      return;
    }

    if (status.local_time) {
      const parsed = Date.parse(status.local_time);
      if (Number.isFinite(parsed)) {
        deviceClock = {
          epochMs: parsed,
          sampledAt: performance.now(),
        };
        updateDeviceTimeUi();
      }
    }
  }

  async function sampleDeviceStatus() {
    const statusUrl = createDeviceStatusUrl();
    if (!statusUrl) {
      setText(deviceState, 'n/a');
      return;
    }

    try {
      const response = await fetch(statusUrl, { cache: 'no-store' });
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      const status = await response.json();
      const temp = Number.isFinite(status.temp_c) ? `${status.temp_c.toFixed(1)}C` : 'temp?';
      const powerStatusAvailable = status.power_status_available !== false;
      const throttled = status.throttled || (powerStatusAvailable ? 'thr?' : 'thr n/a');
      const power = !powerStatusAvailable
        ? ' PWR?'
        : status.undervoltage_now
          ? ' UV!'
          : status.undervoltage_seen
            ? ' uvSeen'
            : ' PWRok';
      const volts = Number.isFinite(status.core_volts)
        ? ` core${status.core_volts.toFixed(2)}V`
        : '';
      const rssi = Number.isFinite(status.rssi_dbm) ? ` ${status.rssi_dbm}dBm` : '';
      const signal = Number.isFinite(status.signal_percent) ? ` ${status.signal_percent}%` : '';
      const freq = Number.isFinite(status.freq_mhz) ? ` ${status.freq_mhz}MHz` : '';
      if (status.mode) {
        setText(modeState, formatMode(status.mode));
      }
      applyDeviceInfo(status);
      setText(deviceState, `${temp} ${throttled}${power}${volts}${rssi}${signal}${freq}`);
    } catch (_) {
      setText(deviceState, 'n/a');
    }
  }

  async function sampleDeviceInfo() {
    updateHostUi(getEndpointHostName());
    setText(timeState, 'n/a');
  }

  function startDeviceStatusMonitor() {
    sampleDeviceInfo();
    if (DEVICE_STATUS_MODE === 'off') {
      setText(deviceState, 'off');
      return;
    }
    if (DEVICE_STATUS_MODE === 'once') {
      sampleDeviceStatus();
      return;
    }
    if (DEVICE_STATUS_MODE === 'poll') {
      sampleDeviceStatus();
      return;
    }
    setText(deviceState, 'off');
  }

  function isDebugOsdEnabled() {
    return document.body.classList.contains('debug-osd');
  }

  function updateDebugDeviceState() {
    const enabled = isDebugOsdEnabled();
    btnRefreshMode.disabled = !enabled;
    btnRefreshDevice.disabled = !enabled;
    setModeUiEnabled(enabled && modeOptions.length > 0);

    if (DEVICE_STATUS_MODE === 'off' || DEVICE_STATUS_MODE === 'debug') {
      setText(deviceState, 'off');
    }
  }

  async function refreshDebugModeStatus() {
    if (!isDebugOsdEnabled()) {
      return;
    }
    setText(modeState, 'updating');
    await loadModeStatus();
  }

  async function refreshDebugDeviceStatus() {
    if (!isDebugOsdEnabled()) {
      return;
    }
    setText(deviceState, 'updating');
    await sampleDeviceStatus();
  }

  function createStatusApiUrl(path) {
    const params = getUrlParams();
    const baseUrl = params.get('statusBaseUrl');
    if (baseUrl) {
      return `${baseUrl.replace(/\/$/, '')}${path}`;
    }
    const host = getStatusApiHost();
    return `http://${host}:8090${path}`;
  }

  function createModeUrl() {
    const params = getUrlParams();
    const modeUrl = params.get('modeUrl');
    if (modeUrl) {
      return modeUrl;
    }
    return createStatusApiUrl('/mode');
  }

  function openInputSetup() {
    setDriveEnabled(false);
    // Relay 配布版は pilot.html と gamepad.html が同じ階層、GitHub Pages 正本は
    // variants/relay/pilot.html からリポジトリ直下の gamepad.html を参照する。
    const inputSetupPath = /\/variants\/relay\/pilot\.html$/i.test(location.pathname)
      ? '../../gamepad.html'
      : 'gamepad.html';
    const url = new URL(inputSetupPath, location.href);
    const device = getRelayDevice();
    if (device) {
      url.searchParams.set('device', device);
    }
    url.searchParams.set('viewer', 'relay-pilot');
    url.searchParams.set('relayPilotPath', 'flat');
    // Ayame 接続など、現在の Pilot URL の接続パラメータを設定画面から戻す時にも維持する。
    url.searchParams.set('returnUrl', location.href);
    window.open(url.toString(), '_blank', 'noopener');
  }

  function setElementHidden(element, hidden) {
    if (!element) {
      return;
    }
    element.hidden = hidden;
    element.style.display = hidden ? 'none' : '';
  }

  function applyMediaControlsVisibility() {
    const hidden = !MEDIA_CONTROLS_VISIBLE;
    setElementHidden(btnAudio, hidden);
    setElementHidden(btnAudioFilter, hidden);
    setElementHidden(micControl, hidden);
  }

  function formatMode(mode) {
    if (!mode) {
      return 'n/a';
    }
    const label = mode.label || `${mode.resolution || '?'} ${mode.framerate || '?'}fps`;
    if (mode.audio_enabled && !/audio/i.test(label)) {
      return `${label} + Audio`;
    }
    return label;
  }

  function setModeUiEnabled(enabled) {
    modeSelect.disabled = !enabled;
    btnApplyMode.disabled = !enabled || modeSelect.value === '';
  }

  async function loadModeStatus() {
    if (!isDebugOsdEnabled()) {
      return;
    }
    try {
      const response = await fetch(createModeUrl(), { cache: 'no-store' });
      if (!response.ok) {
        throw new Error(`HTTP ${response.status}`);
      }
      const payload = await response.json();
      modeOptions = Array.isArray(payload.modes) ? payload.modes : [];
      modeSelect.replaceChildren();
      for (const mode of modeOptions) {
        const option = document.createElement('option');
        option.value = mode.name;
        option.textContent = mode.label || mode.name;
        modeSelect.appendChild(option);
      }
      if (payload.active && payload.active.name) {
        modeSelect.value = payload.active.name;
        setText(modeState, formatMode(payload.active));
      }
      setModeUiEnabled(modeOptions.length > 0);
    } catch (error) {
      console.warn('load mode failed:', error);
      modeSelect.replaceChildren(new Option('unavailable', ''));
      setModeUiEnabled(false);
      setText(modeState, 'n/a');
    }
  }

  async function applySelectedMode() {
    const mode = modeSelect.value;
    if (!mode) {
      return;
    }
    const selected = modeOptions.find((item) => item.name === mode);
    const label = selected ? formatMode(selected) : mode;
    const confirmed = window.confirm(`${label} に切り替えます。Momo が数秒再起動します。`);
    if (!confirmed) {
      return;
    }

    setModeUiEnabled(false);
    setText(modeState, 'switching');
    recordEvent('mode switch', mode);
    try {
      const response = await fetch(createModeUrl(), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ mode }),
      });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok || payload.ok === false) {
        throw new Error(payload.error || `HTTP ${response.status}`);
      }
      if (payload.active) {
        setText(modeState, formatMode(payload.active));
      }
      shouldReconnect = true;
      scheduleReconnect('mode switch');
    } catch (error) {
      console.error('apply mode failed:', error);
      setText(modeState, 'switch failed');
      window.alert(`モード切替に失敗しました: ${error.message || error}`);
    } finally {
      window.setTimeout(loadModeStatus, 3500);
    }
  }

  endpointInput.value = getInitialHost();
  syncCommandFromSliders();
  updateFfbPresetControls();
  initializeFfb();

  steeringInput.addEventListener('input', syncCommandFromSliders);
  throttleInput.addEventListener('input', syncCommandFromThrottleSlider);
  steeringInput.addEventListener('pointerdown', (event) => onRcPointerDown('steering', event));
  throttleInput.addEventListener('pointerdown', (event) => onRcPointerDown('throttle', event));
  steeringInput.addEventListener('pointerup', onRcPointerEnd);
  throttleInput.addEventListener('pointerup', onRcPointerEnd);
  steeringInput.addEventListener('pointercancel', onRcPointerEnd);
  throttleInput.addEventListener('pointercancel', onRcPointerEnd);
  steeringInput.addEventListener('lostpointercapture', onRcPointerEnd);
  throttleInput.addEventListener('lostpointercapture', onRcPointerEnd);
  steeringInput.addEventListener('blur', () => onRcControlBlur('steering'));
  throttleInput.addEventListener('blur', () => onRcControlBlur('throttle'));
  btnDrive.addEventListener('click', toggleDrive);
  if (driveHudMode) {
    driveHudMode.addEventListener('click', toggleDrive);
  }
  if (driveHudConnection) {
    driveHudConnection.addEventListener('click', disconnect);
  }
  for (const button of gearButtons) {
    button.addEventListener('click', () => setThrottleGear(button.dataset.gear));
  }
  if (btnSend) {
    btnSend.addEventListener('click', () => sendCommand(lastRcCommand));
  }
  if (dataTextInput) {
    dataTextInput.addEventListener('keydown', (event) => {
      if (event.key === 'Enter') {
        sendCommand(lastRcCommand);
      }
    });
  }
  if (btnNeutral) {
    btnNeutral.addEventListener('click', () => {
      setNeutralCommand();
      sendCommand(lastRcCommand);
    });
  }
  if (btnDisconnect) {
    btnDisconnect.addEventListener('click', disconnect);
  }
  btnReconnect.addEventListener('click', () => {
    if (isConnectionActive()) {
      disconnect();
      return;
    }
    connect().catch((error) => {
      recordEvent('connect failed', error.message || String(error));
      updateUiState();
    });
  });
  btnFullscreen.addEventListener('click', () => {
    document.documentElement.requestFullscreen?.();
  });
  btnFlip.addEventListener('click', toggleVideoFlip);
  btnMirror.addEventListener('click', toggleVideoMirror);
  btnAudio.addEventListener('click', toggleAudio);
  btnAudioFilter?.addEventListener('click', toggleAudioFilter);
  btnMic?.addEventListener('click', toggleMic);
  micVolumeInput?.addEventListener('input', () => setMicVolume());
  btnDebug.addEventListener('click', toggleDebugOsd);
  modeSelect.addEventListener('change', () => setModeUiEnabled(isDebugOsdEnabled() && modeOptions.length > 0));
  btnApplyMode.addEventListener('click', applySelectedMode);
  btnRefreshMode.addEventListener('click', () => {
    refreshDebugModeStatus().catch((error) => {
      console.warn('refresh mode failed:', error);
    });
  });
  btnRefreshDevice.addEventListener('click', () => {
    refreshDebugDeviceStatus().catch((error) => {
      console.warn('refresh device failed:', error);
    });
  });
  btnInputSetup.addEventListener('click', openInputSetup);
  for (const button of ffbPresetButtons) {
    button.addEventListener('click', () => setFfbPreset(button.dataset.ffbPreset));
  }
  window.addEventListener('keydown', onControlKeyDown);
  window.addEventListener('keyup', onControlKeyUp);
  remoteVideo.addEventListener('loadedmetadata', updateUiState);
  remoteVideo.addEventListener('resize', updateUiState);
  if (micVolumeInput) {
    micVolumeInput.value = String(MIC_DEFAULT_VOLUME);
  }
  setMicVolume(MIC_DEFAULT_VOLUME);
  updateMicUi();

  window.fpvViewer = {
    connect,
    disconnect,
    scheduleReconnect,
    sendCommand,
    setDriveEnabled,
    setDebugOsd,
    setVideoFlip,
    setVideoMirror,
    setMicEnabled,
    getDiagnostics: () => ({
      reconnectCount,
      lastReconnectAt,
      lastReconnectReason,
      lastWsClose,
      eventCounters: { ...eventCounters },
      eventLog: eventLog.slice(),
      videoFreezeTimeoutMs: VIDEO_FREEZE_TIMEOUT_MS,
      autoReconnectOnVideoLost: AUTO_RECONNECT_ON_VIDEO_LOST,
      autoReconnect: AUTO_RECONNECT,
      deviceStatusMode: DEVICE_STATUS_MODE,
      audioFilter: {
        enabled: audioFilterEnabled,
        frequencies: AUDIO_FILTER_FREQS.slice(),
        q: AUDIO_FILTER_Q,
        contextState: audioContext?.state || 'none',
      },
      gamepad: {
        enabled: GAMEPAD_ENABLED,
        index: GAMEPAD_INDEX,
        steeringAxis: GAMEPAD_STEERING_AXIS,
        steeringInvert: GAMEPAD_STEERING_INVERT,
        steeringGain: GAMEPAD_STEERING_GAIN,
        steeringDeadzone: GAMEPAD_STEERING_DEADZONE,
        throttleAxis: GAMEPAD_THROTTLE_AXIS,
        throttleInvert: GAMEPAD_THROTTLE_INVERT,
        brakeAxis: GAMEPAD_BRAKE_AXIS,
        brakeInvert: GAMEPAD_BRAKE_INVERT,
        pedalDeadzone: GAMEPAD_PEDAL_DEADZONE,
        driveButton: GAMEPAD_DRIVE_BUTTON,
        paddleLeftButton: GAMEPAD_PADDLE_LEFT_BUTTON,
        paddleRightButton: GAMEPAD_PADDLE_RIGHT_BUTTON,
        ffbPresetButton: GAMEPAD_FFB_PRESET_BUTTON,
        profileId: GAMEPAD_PROFILE.id || '',
      },
      ffb: {
        enabled: FFB_ENABLED,
        activePreset: activeFfbPreset,
        bridge: ffbClient?.snapshot?.() || null,
      },
    }),
    getPeerConnection: () => peerConnection,
  };

  setVideoFlip(isFlipEnabledByDefault());
  setVideoMirror(isMirrorEnabledByDefault());
  setAudioEnabled(false);
  setAudioFilterEnabled(AUDIO_FILTER_DEFAULT);
  applyMediaControlsVisibility();
  window.momoRaceHud = {
    setState: setRaceState,
    reset: () => setRaceState({ reset: true }),
    getState: () => ({
      ...raceState,
      laps: raceState.laps.slice(),
      rivals: raceState.rivals.map((rival) => ({ ...rival })),
    }),
    testBattle: () => setRaceState(createRaceBattleDemoState()),
    testAnnouncement: () => speakRaceLapAnnouncement({
      key: 'manual-test',
      lap: 1,
      text: 'ラップ 1、18秒320。ベストラップです。',
    }),
    getAnnouncementDiagnostics: () => ({
      enabled: RACE_ANNOUNCE_ENABLED,
      supported: supportsRaceAnnouncement(),
      language: RACE_ANNOUNCE_LANGUAGE,
      voice: RACE_ANNOUNCE_VOICE || null,
      rate: RACE_ANNOUNCE_RATE,
      volume: RACE_ANNOUNCE_VOLUME,
      lastKey: lastRaceLapAnnouncementKey || null,
    }),
  };
  window.addEventListener('momo-race-state', (event) => setRaceState(event.detail));
  window.addEventListener('pointerdown', prepareRaceAnnouncement);
  window.addEventListener('keydown', prepareRaceAnnouncement);
  renderRaceHud();
  startRaceBattleDemo();
  window.setInterval(() => {
    if (
      raceState.clockRunning
      || raceState.phaseCode === 'countdown'
      || raceState.phaseCode === 'green'
    ) {
      renderRaceHud();
    }
  }, 100);
  if (usesRelayTransport()) {
    document.body.classList.add('relay-mode');
    const device = getRelayDevice();
    if (device) {
      document.title = `${document.title} - ${device}`;
    }
    for (const control of [btnAudio, btnAudioFilter, btnMic, micVolumeInput,
      btnApplyMode, btnRefreshMode, btnRefreshDevice]) {
      if (control) {
        control.disabled = true;
      }
    }
    recordEvent('relay mode', 'video + telemetry + RC command');
  }
  setDebugOsd(isDebugEnabledByDefault());
  updateOsdScale();
  window.addEventListener('resize', updateOsdScale);
  window.visualViewport?.addEventListener('resize', updateOsdScale);
  document.addEventListener('visibilitychange', () => {
    if (!document.hidden) {
      visibleSince = performance.now();
    }
  });
  window.addEventListener('pagehide', () => {
    stopRaceAnnouncement();
    shutdownForPageHide();
  });
  startFpsMonitor();
  startLinkMonitor();
  startStatsMonitor();
  startOsdMonitor();
  startDcPingMonitor();
  startDeviceStatusMonitor();
  startRoomLockStatusMonitor();
  startGamepadPoller();
  updateGearUi();
  updateControlUiMode();
  if (AUTO_START) {
    connect().catch((error) => {
      recordEvent('connect failed', error.message || String(error));
      updateUiState();
    });
  } else {
    shouldReconnect = false;
    recordEvent('manual connect required');
    updateUiState();
  }
})();
