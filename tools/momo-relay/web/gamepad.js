"use strict";

const padsEl = document.getElementById("pads");
const statusEl = document.getElementById("status");
const mappingOutputEl = document.getElementById("mappingOutput");
const resetCalibrationEl = document.getElementById("resetCalibration");
const copyMappingEl = document.getElementById("copyMapping");
const saveMappingEl = document.getElementById("saveMapping");
const clearMappingEl = document.getElementById("clearMapping");
const openViewerEl = document.getElementById("openViewer");
const assignStatusEl = document.getElementById("assignStatus");
const profileStatusEl = document.getElementById("profileStatus");
const captureButtons = Array.from(document.querySelectorAll(".captureButton"));
const languageButtons = Array.from(document.querySelectorAll(".languageButton"));
const optionInputs = {
  steeringInvert: document.getElementById("steeringInvert"),
  throttleInvert: document.getElementById("throttleInvert"),
  brakeInvert: document.getElementById("brakeInvert"),
  steeringGain: document.getElementById("steeringGain"),
  steeringDeadzone: document.getElementById("steeringDeadzone"),
  pedalDeadzone: document.getElementById("pedalDeadzone"),
  ffbEnabled: document.getElementById("ffbEnabled"),
  ffbPreset: document.getElementById("ffbPreset"),
  ffbBaseFriction: document.getElementById("ffbBaseFriction"),
  ffbParkingFriction: document.getElementById("ffbParkingFriction"),
  ffbBaseDamper: document.getElementById("ffbBaseDamper"),
  ffbSpeedDamper: document.getElementById("ffbSpeedDamper"),
  ffbBridgeUrl: document.getElementById("ffbBridgeUrl")
};

const profileApi = window.FpvGamepadProfiles;
const legacyStorageKey = "fpvGamepadMapping";
const pageParams = new URLSearchParams(location.search);
const targetDevice = pageParams.get("device")?.trim() || "";
const relayPilotTarget = pageParams.get("viewer") === "relay-pilot";
// Relay に同期される gamepad.html は web/ 直下に配置されるため、Pilot も同じ階層を使う。
const relayPilotPath = pageParams.get("relayPilotPath") === "flat"
  ? "./pilot.html"
  : "./variants/relay/pilot.html";
const returnViewerUrl = getReturnViewerUrl();
const profileScope = targetDevice ? `device:${targetDevice}` : "";
const scopedLegacyStorageKey = targetDevice
  ? `${legacyStorageKey}:${encodeURIComponent(targetDevice)}`
  : legacyStorageKey;
const languageStorageKey = "fpvGamepadLanguage";
const axisDeadzone = 0.003;
const recentMs = 700;
const axisChangeThreshold = 0.01;
const buttonChangeThreshold = 0.05;
const states = new Map();

let lastSeen = 0;
let selectedGamepadIndex = null;
let selectedProfileKey = "";
let profileStore = profileApi.load(window.localStorage, profileScope);
let legacyMapping = null;
let currentLanguage = loadLanguage();

const translations = {
  en: {
    title: "FPV RC Gamepad Test",
    pageTitle: "FPV RC Gamepad Test",
    guideTitle: "Setup flow",
    guideLead: "Use this page to find which wheel axes and buttons should control the FPV RC Viewer.",
    step1: "Connect the wheel or controller to this device, then click this page once.",
    step2: "Move only one control at a time. The row that lights up is the axis or button to assign.",
    step3: "Set Steering, Throttle, Brake, Drive, and paddle buttons from the buttons under each row.",
    step4: "Use invert, gain, and deadzone if the direction or sensitivity is wrong.",
    step5: "Press Save for Viewer, then open the Viewer. The saved mapping is used automatically.",
    notice: "Move only one control at a time. Use min/max/recent changes to identify steering, throttle, brake, and drive buttons.",
    resetCalibration: "Reset calibration",
    copyMapping: "Copy mapping",
    copied: "Copied",
    copyFailed: "Copy failed",
    saveMapping: "Save for Viewer",
    clearMapping: "Clear saved",
    clearSaved: "Clear saved",
    openViewer: "Open Viewer",
    steeringInvert: "Steering invert",
    throttleInvert: "Throttle invert",
    brakeInvert: "Brake invert",
    steeringGain: "Steering gain",
    steeringDeadzone: "Steering deadzone",
    pedalDeadzone: "Pedal deadzone",
    ffbOptionsTitle: "Force feedback",
    ffbEnabled: "Enable FFB while Drive On",
    ffbPreset: "FFB strength",
    ffbPresetWeak: "Weak",
    ffbPresetMedium: "Medium",
    ffbPresetStrong: "Strong",
    ffbBaseFriction: "Base friction",
    ffbParkingFriction: "Low-speed friction",
    ffbBaseDamper: "Base damper",
    ffbSpeedDamper: "Speed damper",
    ffbBridgeUrl: "Bridge URL",
    captureIdle: "Capture idle",
    captureSteeringLeft: "Capture steering left",
    captureSteeringRight: "Capture steering right",
    captureThrottleReleased: "Capture throttle released",
    captureThrottlePressed: "Capture throttle pressed",
    captureBrakeReleased: "Capture brake released",
    captureBrakePressed: "Capture brake pressed",
    captured: "Captured",
    captureFailed: "Capture failed",
    noGamepad: "No gamepad reported by the browser yet. Click this page and move the wheel or press a button.",
    generatedMapping: "Generated mapping",
    input: "input",
    current: "current",
    min: "min",
    max: "max",
    delta: "delta",
    range: "range",
    unknownGamepad: "Unknown gamepad",
    set: "Set",
    steering: "Steering",
    throttle: "Throttle",
    brake: "Brake",
    drive: "Drive",
    paddleLeft: "Paddle L",
    paddleRight: "Paddle R",
    ffbPresetButton: "FFB level",
    savedForViewer: "saved for Viewer",
    selectedDevice: "Selected device",
    selectDevice: "Use this device",
    profileLoaded: "profile loaded",
    profileNew: "new profile",
    savedMappingCleared: "saved mapping cleared",
    gamepads: "gamepad(s)",
    lastSeen: "last seen",
    msAgo: "ms ago",
    notSeen: "not seen"
  },
  ja: {
    title: "FPV RC ゲームパッド設定",
    pageTitle: "FPV RC ゲームパッド設定",
    guideTitle: "設定手順",
    guideLead: "このページでは、ハンドルコントローラーやゲームパッドの軸 / ボタンを FPV RC Viewer の操作へ割り当てます。",
    step1: "ハンコンまたはゲームパッドを接続し、このページを一度クリックします。",
    step2: "一度に 1 つだけ操作します。光った行が割り当て対象の軸またはボタンです。",
    step3: "各行の下にあるボタンで Steering、Throttle、Brake、Drive、パドルを割り当てます。",
    step4: "向きや感度が合わない場合は invert、gain、deadzone を調整します。",
    step5: "Save for Viewer を押してから Viewer を開きます。保存した割り当てが自動で使われます。",
    notice: "一度に 1 つだけ操作してください。min / max / recent の変化を見て steering、throttle、brake、drive ボタンを特定します。",
    resetCalibration: "キャリブレーションリセット",
    copyMapping: "設定をコピー",
    copied: "コピー済み",
    copyFailed: "コピー失敗",
    saveMapping: "Viewer 用に保存",
    clearMapping: "保存を削除",
    clearSaved: "保存を削除",
    openViewer: "Viewer を開く",
    steeringInvert: "ステアリング反転",
    throttleInvert: "スロットル反転",
    brakeInvert: "ブレーキ反転",
    steeringGain: "ステアリング感度",
    steeringDeadzone: "ステアリング遊び",
    pedalDeadzone: "ペダル遊び",
    ffbOptionsTitle: "フォースフィードバック",
    ffbEnabled: "Drive On 中に FFB を有効化",
    ffbPreset: "FFB 強度",
    ffbPresetWeak: "弱",
    ffbPresetMedium: "中",
    ffbPresetStrong: "強",
    ffbBaseFriction: "基礎フリクション",
    ffbParkingFriction: "低速フリクション",
    ffbBaseDamper: "基礎ダンパー",
    ffbSpeedDamper: "速度ダンパー",
    ffbBridgeUrl: "Bridge URL",
    captureIdle: "中立を記録",
    captureSteeringLeft: "左ステアを記録",
    captureSteeringRight: "右ステアを記録",
    captureThrottleReleased: "スロットル未入力を記録",
    captureThrottlePressed: "スロットル踏み込みを記録",
    captureBrakeReleased: "ブレーキ未入力を記録",
    captureBrakePressed: "ブレーキ踏み込みを記録",
    captured: "記録済み",
    captureFailed: "記録失敗",
    noGamepad: "ブラウザがまだゲームパッドを認識していません。このページをクリックしてから、ハンドルを動かすかボタンを押してください。",
    generatedMapping: "生成された設定",
    input: "入力",
    current: "現在値",
    min: "最小",
    max: "最大",
    delta: "変化",
    range: "範囲",
    unknownGamepad: "不明なゲームパッド",
    set: "割り当て",
    steering: "ステアリング",
    throttle: "スロットル",
    brake: "ブレーキ",
    drive: "Drive",
    paddleLeft: "左パドル",
    paddleRight: "右パドル",
    ffbPresetButton: "FFB 強度",
    savedForViewer: "Viewer 用に保存しました",
    selectedDevice: "設定対象",
    selectDevice: "このデバイスを設定",
    profileLoaded: "保存済みプロファイルを読込",
    profileNew: "新規プロファイル",
    savedMappingCleared: "保存した設定を削除しました",
    gamepads: "gamepad",
    lastSeen: "最終認識",
    msAgo: "ms 前",
    notSeen: "未認識"
  }
};

function createDefaultMapping() {
  return {
    id: "",
    index: 0,
    profileKey: "",
    vendorId: "",
    productId: "",
    steeringAxis: null,
    steeringInvert: false,
    steeringGain: 1.0,
    steeringDeadzone: 0.03,
    steeringCenter: 0,
    steeringLeft: -1,
    steeringRight: 1,
    throttleAxis: null,
    throttleButton: null,
    throttleInvert: false,
    throttleIdle: 1,
    throttlePressed: -1,
    throttleMin: 1500,
    throttleMax: 2000,
    brakeAxis: null,
    brakeButton: null,
    brakeInvert: false,
    brakeIdle: 1,
    brakePressed: -1,
    pedalDeadzone: 0.05,
    ffbEnabled: false,
    ffbPreset: "medium",
    ffbBaseFriction: 0.28,
    ffbParkingFriction: 0.08,
    ffbBaseDamper: 0.05,
    ffbSpeedDamper: 0.15,
    ffbBridgeUrl: "ws://127.0.0.1:24725",
    reverseMin: 1300,
    driveButton: null,
    paddleLeftButton: null,
    paddleRightButton: null,
    ffbPresetButton: null
  };
}

const mapping = createDefaultMapping();

function loadLanguage() {
  const saved = window.localStorage?.getItem(languageStorageKey);
  if (saved === "ja" || saved === "en") {
    return saved;
  }
  return navigator.language?.toLowerCase().startsWith("ja") ? "ja" : "en";
}

function t(key) {
  return translations[currentLanguage]?.[key] || translations.en[key] || key;
}

function applyLanguage() {
  document.documentElement.lang = currentLanguage;
  document.title = t("pageTitle");
  document.querySelectorAll("[data-i18n]").forEach((element) => {
    const key = element.dataset.i18n;
    element.textContent = t(key);
  });
  languageButtons.forEach((button) => {
    const active = button.dataset.lang === currentLanguage;
    button.classList.toggle("active", active);
    button.setAttribute("aria-pressed", active ? "true" : "false");
  });
}

function setLanguage(language) {
  currentLanguage = language === "ja" ? "ja" : "en";
  window.localStorage?.setItem(languageStorageKey, currentLanguage);
  applyLanguage();
  updateMappingOutput();
}

function formatValue(value) {
  const normalized = Math.abs(value) < axisDeadzone ? 0 : value;
  return normalized.toFixed(4);
}

function clamp01(value) {
  return Math.max(0, Math.min(1, value));
}

function normalizeFfbPreset(value) {
  const preset = String(value || "").toLowerCase();
  return ["weak", "medium", "strong"].includes(preset) ? preset : "medium";
}

function axisPercent(value) {
  return clamp01((value + 1) / 2) * 100;
}

function buttonPercent(value) {
  return clamp01(value) * 100;
}

function loadSavedMapping() {
  try {
    const raw = window.localStorage?.getItem(scopedLegacyStorageKey);
    if (raw) {
      const saved = JSON.parse(raw);
      if (saved && typeof saved === "object") {
        legacyMapping = { ...saved };
      }
    }
    const active = profileStore.profiles[profileStore.activeProfileKey];
    Object.assign(mapping, active || legacyMapping || {});
    migrateSavedMapping();
  } catch (error) {
    console.warn("load saved mapping failed", error);
  }
}

function legacyMappingMatches(identity) {
  if (!legacyMapping) {
    return false;
  }
  if (!legacyMapping.id) {
    return Object.keys(profileStore.profiles).length === 0;
  }
  return profileApi.parseGamepadIdentity(legacyMapping.id).key === identity.key;
}

function selectGamepad(gamepad, force = false) {
  if (!gamepad) {
    return;
  }
  const identity = profileApi.parseGamepadIdentity(gamepad.id);
  if (!force && selectedGamepadIndex === gamepad.index && selectedProfileKey === identity.key) {
    return;
  }

  const saved = profileStore.profiles[identity.key];
  const source = saved || (legacyMappingMatches(identity) ? legacyMapping : null);
  Object.assign(mapping, createDefaultMapping(), source || {}, {
    id: gamepad.id || "",
    index: gamepad.index,
    profileKey: identity.key,
    vendorId: identity.vendorId,
    productId: identity.productId
  });
  selectedGamepadIndex = gamepad.index;
  selectedProfileKey = identity.key;
  migrateSavedMapping();
  syncOptionsFromMapping();
  profileStatusEl.textContent = `${identity.label} / ${saved ? t("profileLoaded") : t("profileNew")}`;
  updateMappingOutput();
}

function hasSteeringCalibration(value = mapping) {
  return Number.isFinite(value.steeringCenter)
    && Number.isFinite(value.steeringLeft)
    && Number.isFinite(value.steeringRight)
    && Math.abs(value.steeringLeft - value.steeringRight) >= 0.001;
}

function isLegacySteeringGain(value) {
  return Math.abs(Number(value) - 4.0) < 0.001 || Math.abs(Number(value) - 3.75) < 0.001;
}

function migrateSavedMapping() {
  if (hasSteeringCalibration() && isLegacySteeringGain(mapping.steeringGain)) {
    mapping.steeringGain = 1.0;
  }
}

function numberFromInput(input, fallback) {
  const value = Number(input?.value);
  return Number.isFinite(value) ? value : fallback;
}

function stringFromInput(input, fallback) {
  const value = String(input?.value || "").trim();
  return value || fallback;
}

function syncOptionsFromMapping() {
  optionInputs.steeringInvert.checked = Boolean(mapping.steeringInvert);
  optionInputs.throttleInvert.checked = Boolean(mapping.throttleInvert);
  optionInputs.brakeInvert.checked = Boolean(mapping.brakeInvert);
  optionInputs.steeringGain.value = String(mapping.steeringGain ?? 1.0);
  optionInputs.steeringDeadzone.value = String(mapping.steeringDeadzone ?? 0.03);
  optionInputs.pedalDeadzone.value = String(mapping.pedalDeadzone ?? 0.05);
  optionInputs.ffbEnabled.checked = Boolean(mapping.ffbEnabled);
  optionInputs.ffbPreset.value = normalizeFfbPreset(mapping.ffbPreset);
  optionInputs.ffbBaseFriction.value = String(mapping.ffbBaseFriction ?? 0.28);
  optionInputs.ffbParkingFriction.value = String(mapping.ffbParkingFriction ?? 0.08);
  optionInputs.ffbBaseDamper.value = String(mapping.ffbBaseDamper ?? 0.05);
  optionInputs.ffbSpeedDamper.value = String(mapping.ffbSpeedDamper ?? 0.15);
  optionInputs.ffbBridgeUrl.value = mapping.ffbBridgeUrl || "ws://127.0.0.1:24725";
}

function syncMappingFromOptions() {
  mapping.steeringInvert = Boolean(optionInputs.steeringInvert.checked);
  mapping.throttleInvert = Boolean(optionInputs.throttleInvert.checked);
  mapping.brakeInvert = Boolean(optionInputs.brakeInvert.checked);
  mapping.steeringGain = numberFromInput(optionInputs.steeringGain, 1.0);
  mapping.steeringDeadzone = numberFromInput(optionInputs.steeringDeadzone, 0.03);
  mapping.pedalDeadzone = numberFromInput(optionInputs.pedalDeadzone, 0.05);
  mapping.ffbEnabled = Boolean(optionInputs.ffbEnabled.checked);
  mapping.ffbPreset = normalizeFfbPreset(optionInputs.ffbPreset.value);
  mapping.ffbBaseFriction = clamp01(numberFromInput(optionInputs.ffbBaseFriction, 0.28));
  mapping.ffbParkingFriction = clamp01(numberFromInput(optionInputs.ffbParkingFriction, 0.08));
  mapping.ffbBaseDamper = clamp01(numberFromInput(optionInputs.ffbBaseDamper, 0.05));
  mapping.ffbSpeedDamper = clamp01(numberFromInput(optionInputs.ffbSpeedDamper, 0.15));
  delete mapping.ffbRunningCentering;
  delete mapping.ffbCenteringReverse;
  mapping.ffbBridgeUrl = stringFromInput(optionInputs.ffbBridgeUrl, "ws://127.0.0.1:24725");
}

function buildViewerUrl() {
  const url = new URL(
    returnViewerUrl || (relayPilotTarget ? relayPilotPath : (openViewerEl?.getAttribute("href") || "./viewer.html")),
    location.href
  );
  const params = new URLSearchParams(url.hash.replace(/^#\??/, ""));
  const relayHost = pageParams.get("host") || "";
  if (targetDevice) {
    url.searchParams.set("device", targetDevice);
  }
  if (relayHost) {
    url.searchParams.set("host", relayHost);
  }
  params.set("gamepad", "1");
  params.set("gamepadIndex", String(mapping.index ?? 0));
  if (selectedProfileKey) {
    params.set("gamepadProfile", selectedProfileKey);
  }
  if (mapping.steeringAxis !== null) {
    params.set("gamepadSteeringAxis", String(mapping.steeringAxis));
  }
  params.set("gamepadSteeringInvert", mapping.steeringInvert ? "1" : "0");
  params.set("gamepadSteeringGain", String(mapping.steeringGain ?? 1.0));
  params.set("gamepadSteeringDeadzone", String(mapping.steeringDeadzone ?? 0.03));
  params.set("gamepadSteeringCenter", String(mapping.steeringCenter ?? 0));
  params.set("gamepadSteeringLeft", String(mapping.steeringLeft ?? -1));
  params.set("gamepadSteeringRight", String(mapping.steeringRight ?? 1));
  if (mapping.throttleAxis !== null) {
    params.set("gamepadThrottleAxis", String(mapping.throttleAxis));
  }
  if (mapping.throttleButton !== null) {
    params.set("gamepadThrottleButton", String(mapping.throttleButton));
  }
  params.set("gamepadThrottleInvert", mapping.throttleInvert ? "1" : "0");
  params.set("gamepadThrottleIdle", String(mapping.throttleIdle ?? 1));
  params.set("gamepadThrottlePressed", String(mapping.throttlePressed ?? -1));
  if (mapping.brakeAxis !== null) {
    params.set("gamepadBrakeAxis", String(mapping.brakeAxis));
  }
  if (mapping.brakeButton !== null) {
    params.set("gamepadBrakeButton", String(mapping.brakeButton));
  }
  params.set("gamepadBrakeInvert", mapping.brakeInvert ? "1" : "0");
  params.set("gamepadBrakeIdle", String(mapping.brakeIdle ?? 1));
  params.set("gamepadBrakePressed", String(mapping.brakePressed ?? -1));
  params.set("gamepadPedalDeadzone", String(mapping.pedalDeadzone ?? 0.05));
  if (mapping.driveButton !== null) {
    params.set("gamepadDriveButton", String(mapping.driveButton));
  }
  if (mapping.paddleLeftButton !== null) {
    params.set("gamepadPaddleLeftButton", String(mapping.paddleLeftButton));
  }
  if (mapping.paddleRightButton !== null) {
    params.set("gamepadPaddleRightButton", String(mapping.paddleRightButton));
  }
  if (mapping.ffbPresetButton !== null) {
    params.set("gamepadFfbPresetButton", String(mapping.ffbPresetButton));
  }
  params.set("ffbEnabled", mapping.ffbEnabled ? "1" : "0");
  if (mapping.ffbEnabled) {
    params.set("ffbPreset", normalizeFfbPreset(mapping.ffbPreset));
    params.set("ffbBaseFriction", String(mapping.ffbBaseFriction ?? 0.28));
    params.set("ffbParkingFriction", String(mapping.ffbParkingFriction ?? 0.08));
    params.set("ffbBaseDamper", String(mapping.ffbBaseDamper ?? 0.05));
    params.set("ffbSpeedDamper", String(mapping.ffbSpeedDamper ?? 0.15));
    params.set("ffbUrl", mapping.ffbBridgeUrl || "ws://127.0.0.1:24725");
  } else {
    params.delete("ffbBaseFriction");
    params.delete("ffbPreset");
    params.delete("ffbParkingFriction");
    params.delete("ffbBaseDamper");
    params.delete("ffbSpeedDamper");
    params.delete("ffbUrl");
  }
  if (!targetDevice && !relayHost) {
    url.search = "";
  }
  url.hash = params.toString();
  return url.toString();
}

function getReturnViewerUrl() {
  const candidate = pageParams.get("returnUrl");
  if (!candidate) {
    return "";
  }
  try {
    const url = new URL(candidate, location.href);
    if (url.origin !== location.origin || !/\/(?:variants\/relay\/)?pilot\.html$/i.test(url.pathname)) {
      return "";
    }
    return url.toString();
  } catch {
    return "";
  }
}

function updateViewerLink() {
  if (openViewerEl) {
    openViewerEl.href = buildViewerUrl();
  }
}

function getState(gamepad) {
  let state = states.get(gamepad.index);
  if (!state) {
    state = {
      id: gamepad.id || "",
      axes: [],
      buttons: []
    };
    states.set(gamepad.index, state);
  }
  state.id = gamepad.id || state.id;
  return state;
}

function updateState(gamepad) {
  const now = performance.now();
  const state = getState(gamepad);

  gamepad.axes.forEach((value, index) => {
    const previous = state.axes[index];
    const last = previous ? previous.current : value;
    const delta = value - last;
    const changed = Math.abs(delta) >= axisChangeThreshold;
    state.axes[index] = {
      current: value,
      min: previous ? Math.min(previous.min, value) : value,
      max: previous ? Math.max(previous.max, value) : value,
      delta,
      lastChange: changed ? now : previous?.lastChange || 0
    };
  });

  gamepad.buttons.forEach((button, index) => {
    const value = typeof button === "number" ? button : button.value;
    const pressed = typeof button === "object" && button.pressed;
    const previous = state.buttons[index];
    const last = previous ? previous.current : value;
    const delta = value - last;
    const changed = Math.abs(delta) >= buttonChangeThreshold || pressed !== Boolean(previous?.pressed);
    state.buttons[index] = {
      current: value,
      pressed,
      min: previous ? Math.min(previous.min, value) : value,
      max: previous ? Math.max(previous.max, value) : value,
      delta,
      lastChange: changed ? now : previous?.lastChange || 0
    };
  });

  return state;
}

function makeHeaderRow() {
  const row = document.createElement("div");
  row.className = "row header";
  for (const key of ["input", "current", "min", "max", "delta", "range"]) {
    const cell = document.createElement("div");
    cell.textContent = t(key);
    row.appendChild(cell);
  }
  return row;
}

function makeRow(label, valueText, minText, maxText, deltaText, percent, active, recent) {
  const row = document.createElement("div");
  row.className = ["row", active ? "button-on" : "", recent ? "recent" : ""].filter(Boolean).join(" ");

  const name = document.createElement("div");
  name.textContent = label;

  const value = document.createElement("div");
  value.textContent = valueText;

  const min = document.createElement("div");
  min.textContent = minText;

  const max = document.createElement("div");
  max.textContent = maxText;

  const delta = document.createElement("div");
  delta.textContent = deltaText;

  const bar = document.createElement("div");
  bar.className = "bar";
  const fill = document.createElement("div");
  fill.className = "fill";
  fill.style.width = `${percent}%`;
  bar.appendChild(fill);

  row.append(name, value, min, max, delta, bar);
  return row;
}

function makeActions(type, index) {
  const actions = document.createElement("div");
  actions.className = "actions";

  const labels = type === "axis"
    ? [
      ["steeringAxis", "steering"],
      ["throttleAxis", "throttle"],
      ["brakeAxis", "brake"]
    ]
    : [
      ["throttleButton", "throttle"],
      ["brakeButton", "brake"],
      ["driveButton", "drive"],
    ["paddleLeftButton", "paddleLeft"],
      ["paddleRightButton", "paddleRight"],
      ["ffbPresetButton", "ffbPresetButton"]
    ];

  for (const [field, labelKey] of labels) {
    const label = t(labelKey);
    const button = document.createElement("button");
    button.type = "button";
    button.dataset.field = field;
    button.dataset.index = String(index);
    if (mapping[field] === index) {
      button.classList.add("assigned");
      button.textContent = `${label}: ${index}`;
    } else {
      button.textContent = `${t("set")} ${label}`;
    }
    actions.appendChild(button);
  }

  return actions;
}

function renderGamepad(gamepad) {
  const now = performance.now();
  const state = updateState(gamepad);
  const identity = profileApi.parseGamepadIdentity(gamepad.id);
  const isSelected = gamepad.index === selectedGamepadIndex;
  const pad = document.createElement("section");
  pad.className = `pad${isSelected ? " selected" : ""}`;

  const heading = document.createElement("div");
  heading.className = "pad-heading";

  const title = document.createElement("h2");
  title.textContent = `#${gamepad.index} ${gamepad.id || t("unknownGamepad")}`;
  const selectButton = document.createElement("button");
  selectButton.type = "button";
  selectButton.className = `select-gamepad${isSelected ? " active" : ""}`;
  selectButton.dataset.selectGamepad = String(gamepad.index);
  selectButton.textContent = isSelected ? t("selectedDevice") : t("selectDevice");
  selectButton.disabled = isSelected;
  heading.append(title, selectButton);
  pad.appendChild(heading);

  const meta = document.createElement("div");
  meta.className = "meta";
  meta.textContent = [
    identity.label,
    `connected=${gamepad.connected}`,
    `mapping=${gamepad.mapping || "none"}`,
    `axes=${gamepad.axes.length}`,
    `buttons=${gamepad.buttons.length}`,
    `timestamp=${Math.round(gamepad.timestamp || 0)}`
  ].join("  ");
  pad.appendChild(meta);
  pad.appendChild(makeHeaderRow());

  state.axes.forEach((axis, index) => {
    const recent = now - axis.lastChange <= recentMs;
    pad.appendChild(makeRow(
      `axis ${index}`,
      formatValue(axis.current),
      formatValue(axis.min),
      formatValue(axis.max),
      axis.delta.toFixed(4),
      axisPercent(axis.current),
      false,
      recent
    ));
    if (isSelected) {
      pad.appendChild(makeActions("axis", index));
    }
  });

  state.buttons.forEach((button, index) => {
    const recent = now - button.lastChange <= recentMs;
    pad.appendChild(makeRow(
      `btn ${index}`,
      button.current.toFixed(3),
      button.min.toFixed(3),
      button.max.toFixed(3),
      button.delta.toFixed(3),
      buttonPercent(button.current),
      button.pressed,
      recent
    ));
    if (isSelected) {
      pad.appendChild(makeActions("button", index));
    }
  });

  return pad;
}

function updateMappingOutput() {
  syncMappingFromOptions();
  mappingOutputEl.textContent = JSON.stringify(mapping, null, 2);
  updateViewerLink();
}

function fieldLabel(field) {
  switch (field) {
    case "steeringAxis":
      return t("steering");
    case "throttleAxis":
    case "throttleButton":
      return t("throttle");
    case "brakeAxis":
    case "brakeButton":
      return t("brake");
    case "driveButton":
      return t("drive");
    case "paddleLeftButton":
      return t("paddleLeft");
    case "paddleRightButton":
      return t("paddleRight");
    case "ffbPresetButton":
      return t("ffbPresetButton");
    default:
      return field;
  }
}

function assignInput(field, index) {
  mapping[field] = index;
  if (field === "throttleAxis") {
    mapping.throttleButton = null;
  } else if (field === "throttleButton") {
    mapping.throttleAxis = null;
  } else if (field === "brakeAxis") {
    mapping.brakeButton = null;
  } else if (field === "brakeButton") {
    mapping.brakeAxis = null;
  }
  updateMappingOutput();
  assignStatusEl.textContent = `${fieldLabel(field)} = ${index}`;
}

function saveMapping() {
  syncMappingFromOptions();
  const gamepad = getSelectedGamepad();
  if (gamepad) {
    const identity = profileApi.parseGamepadIdentity(gamepad.id);
    selectedProfileKey = identity.key;
    Object.assign(mapping, {
      id: gamepad.id || "",
      index: gamepad.index,
      profileKey: identity.key,
      vendorId: identity.vendorId,
      productId: identity.productId
    });
    profileStore = profileApi.saveProfile(
      window.localStorage,
      profileStore,
      identity.key,
      mapping,
      profileScope
    );
    profileStatusEl.textContent = `${identity.label} / ${t("profileLoaded")}`;
  }
  window.localStorage?.setItem(scopedLegacyStorageKey, JSON.stringify(mapping));
  updateMappingOutput();
  assignStatusEl.textContent = t("savedForViewer");
}

function clearSavedMapping() {
  if (selectedProfileKey) {
    profileStore = profileApi.removeProfile(window.localStorage, profileStore, selectedProfileKey, profileScope);
  }
  window.localStorage?.removeItem(scopedLegacyStorageKey);
  const gamepad = getSelectedGamepad();
  if (gamepad) {
    legacyMapping = null;
    selectGamepad(gamepad, true);
  }
  assignStatusEl.textContent = t("savedMappingCleared");
}

function resetCalibration() {
  states.clear();
}

function getSelectedGamepad() {
  const gamepads = navigator.getGamepads ? Array.from(navigator.getGamepads()).filter(Boolean) : [];
  if (selectedGamepadIndex !== null) {
    const selected = gamepads.find((gamepad) => gamepad.index === selectedGamepadIndex);
    if (selected) {
      return selected;
    }
  }
  return gamepads[0] || null;
}

function getAxisValue(gamepad, axis) {
  if (!gamepad || axis === null || axis === undefined || axis < 0 || axis >= gamepad.axes.length) {
    return null;
  }
  const value = Number(gamepad.axes[axis]);
  return Number.isFinite(value) ? Number(value.toFixed(6)) : null;
}

function getButtonValue(gamepad, buttonIndex) {
  if (!gamepad || buttonIndex === null || buttonIndex === undefined || buttonIndex < 0 || buttonIndex >= gamepad.buttons.length) {
    return null;
  }
  const button = gamepad.buttons[buttonIndex];
  const value = typeof button === "number" ? button : button.value;
  return Number.isFinite(value) ? Number(value.toFixed(6)) : null;
}

function getPedalValue(gamepad, axis, buttonIndex) {
  if (buttonIndex !== null && buttonIndex !== undefined) {
    const buttonValue = getButtonValue(gamepad, buttonIndex);
    if (buttonValue !== null) {
      return buttonValue;
    }
  }
  return getAxisValue(gamepad, axis);
}

function setCapturedBoundary(label, gamepad) {
  const steering = getAxisValue(gamepad, mapping.steeringAxis);
  const throttle = getPedalValue(gamepad, mapping.throttleAxis, mapping.throttleButton);
  const brake = getPedalValue(gamepad, mapping.brakeAxis, mapping.brakeButton);
  const updated = [];

  if (label === "idle") {
    if (steering !== null) {
      mapping.steeringCenter = steering;
      updated.push(`steering center=${steering.toFixed(3)}`);
    }
    if (throttle !== null) {
      mapping.throttleIdle = throttle;
      updated.push(`throttle idle=${throttle.toFixed(3)}`);
    }
    if (brake !== null) {
      mapping.brakeIdle = brake;
      updated.push(`brake idle=${brake.toFixed(3)}`);
    }
  } else if (label === "steering_left" && steering !== null) {
    mapping.steeringLeft = steering;
    updated.push(`steering left=${steering.toFixed(3)}`);
  } else if (label === "steering_right" && steering !== null) {
    mapping.steeringRight = steering;
    updated.push(`steering right=${steering.toFixed(3)}`);
  } else if (label === "throttle_released" && throttle !== null) {
    mapping.throttleIdle = throttle;
    updated.push(`throttle idle=${throttle.toFixed(3)}`);
  } else if (label === "throttle_pressed" && throttle !== null) {
    mapping.throttlePressed = throttle;
    updated.push(`throttle pressed=${throttle.toFixed(3)}`);
  } else if (label === "brake_released" && brake !== null) {
    mapping.brakeIdle = brake;
    updated.push(`brake idle=${brake.toFixed(3)}`);
  } else if (label === "brake_pressed" && brake !== null) {
    mapping.brakePressed = brake;
    updated.push(`brake pressed=${brake.toFixed(3)}`);
  }

  updateMappingOutput();
  return updated;
}

function getCaptureUrl() {
  const params = new URLSearchParams(location.search);
  return params.get("captureUrl") || "http://127.0.0.1:18084/capture";
}

function snapshotGamepads(label) {
  const gamepads = navigator.getGamepads ? Array.from(navigator.getGamepads()).filter(Boolean) : [];
  return {
    label,
    at: new Date().toISOString(),
    pads: gamepads.map((gamepad) => ({
      index: gamepad.index,
      id: gamepad.id,
      mapping: gamepad.mapping || "",
      connected: gamepad.connected,
      axes: gamepad.axes.map((value) => Number(value.toFixed(6))),
      buttons: gamepad.buttons.map((button) => ({
        pressed: button.pressed,
        touched: button.touched,
        value: Number(button.value.toFixed(6))
      }))
    }))
  };
}

async function captureGamepad(label, button) {
  const gamepad = getSelectedGamepad();
  const payload = snapshotGamepads(label);
  if (!gamepad || payload.pads.length === 0) {
    assignStatusEl.textContent = `${t("captureFailed")}: ${label}`;
    return;
  }
  const updated = setCapturedBoundary(label, gamepad);
  if (updated.length === 0) {
    assignStatusEl.textContent = `${t("captureFailed")}: ${label}`;
    return;
  }
  try {
    await fetch(getCaptureUrl(), {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(payload)
    });
  } catch (error) {
    console.warn("capture server unavailable; saved locally", error);
  }
  assignStatusEl.textContent = `${t("captured")} ${label}: ${updated.join(", ")}`;
  if (button) {
    button.textContent = `${t("captured")} ${label}`;
    setTimeout(() => {
      button.textContent = t(button.dataset.i18n) || button.dataset.originalText || button.textContent;
    }, 900);
  }
}

function render() {
  const gamepads = navigator.getGamepads ? Array.from(navigator.getGamepads()).filter(Boolean) : [];
  padsEl.replaceChildren();

  if (gamepads.length === 0) {
    const empty = document.createElement("div");
    empty.className = "empty";
    empty.textContent = t("noGamepad");
    padsEl.appendChild(empty);
  } else {
    lastSeen = performance.now();
    if (selectedGamepadIndex === null || !gamepads.some((gamepad) => gamepad.index === selectedGamepadIndex)) {
      selectGamepad(gamepads[0]);
    }
    for (const gamepad of gamepads) {
      padsEl.appendChild(renderGamepad(gamepad));
    }
  }

  const ageText = lastSeen > 0
    ? `${t("lastSeen")} ${(performance.now() - lastSeen).toFixed(0)} ${t("msAgo")}`
    : t("notSeen");
  statusEl.textContent = `${gamepads.length} ${t("gamepads")}, ${ageText}`;
  updateMappingOutput();
  requestAnimationFrame(render);
}

window.addEventListener("gamepadconnected", (event) => {
  lastSeen = performance.now();
  if (selectedGamepadIndex === null) {
    selectGamepad(event.gamepad);
  }
  console.log("gamepadconnected", event.gamepad);
});

window.addEventListener("gamepaddisconnected", (event) => {
  if (selectedGamepadIndex === event.gamepad.index) {
    selectedGamepadIndex = null;
    selectedProfileKey = "";
  }
  console.log("gamepaddisconnected", event.gamepad);
});

padsEl.addEventListener("pointerdown", (event) => {
  const selectButton = event.target.closest("button[data-select-gamepad]");
  if (selectButton) {
    event.preventDefault();
    const gamepads = navigator.getGamepads ? Array.from(navigator.getGamepads()).filter(Boolean) : [];
    const gamepad = gamepads.find((candidate) => candidate.index === Number(selectButton.dataset.selectGamepad));
    selectGamepad(gamepad);
    return;
  }
  const button = event.target.closest("button[data-field][data-index]");
  if (!button) {
    return;
  }
  event.preventDefault();
  assignInput(button.dataset.field, Number(button.dataset.index));
});

resetCalibrationEl.addEventListener("click", () => {
  resetCalibration();
});

copyMappingEl.addEventListener("click", async () => {
  try {
    await navigator.clipboard.writeText(mappingOutputEl.textContent);
    copyMappingEl.textContent = t("copied");
    setTimeout(() => {
      copyMappingEl.textContent = t("copyMapping");
    }, 900);
  } catch (error) {
    console.error("copy failed", error);
    copyMappingEl.textContent = t("copyFailed");
    setTimeout(() => {
      copyMappingEl.textContent = t("copyMapping");
    }, 1200);
  }
});

saveMappingEl.addEventListener("click", () => {
  saveMapping();
});

clearMappingEl.addEventListener("click", () => {
  clearSavedMapping();
});

Object.values(optionInputs).forEach((input) => {
  input.addEventListener("input", updateMappingOutput);
  input.addEventListener("change", updateMappingOutput);
});

captureButtons.forEach((button) => {
  button.dataset.originalText = button.textContent;
  button.addEventListener("click", () => {
    captureGamepad(button.dataset.label, button);
  });
});

loadSavedMapping();
syncOptionsFromMapping();
applyLanguage();
languageButtons.forEach((button) => {
  button.addEventListener("click", () => {
    setLanguage(button.dataset.lang);
  });
});
render();
