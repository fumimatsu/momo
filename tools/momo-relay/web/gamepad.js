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
const captureButtons = Array.from(document.querySelectorAll(".captureButton"));
const languageButtons = Array.from(document.querySelectorAll(".languageButton"));
const optionInputs = {
  steeringInvert: document.getElementById("steeringInvert"),
  throttleInvert: document.getElementById("throttleInvert"),
  brakeInvert: document.getElementById("brakeInvert"),
  steeringGain: document.getElementById("steeringGain"),
  steeringDeadzone: document.getElementById("steeringDeadzone"),
  pedalDeadzone: document.getElementById("pedalDeadzone")
};

const legacyStorageKey = "fpvGamepadMapping";
const targetDevice = new URLSearchParams(location.search).get("device")?.trim() || "";
const storageKey = targetDevice
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
    savedForViewer: "saved for Viewer",
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
    savedForViewer: "Viewer 用に保存しました",
    savedMappingCleared: "保存した設定を削除しました",
    gamepads: "gamepad",
    lastSeen: "最終認識",
    msAgo: "ms 前",
    notSeen: "未認識"
  }
};

const mapping = {
  id: "",
  index: 0,
  steeringAxis: null,
  steeringInvert: false,
  steeringGain: 1.0,
  steeringDeadzone: 0.03,
  steeringCenter: 0,
  steeringLeft: -1,
  steeringRight: 1,
  throttleAxis: null,
  throttleInvert: false,
  throttleIdle: 1,
  throttlePressed: -1,
  throttleMin: 1500,
  throttleMax: 2000,
  brakeAxis: null,
  brakeInvert: false,
  brakeIdle: 1,
  brakePressed: -1,
  pedalDeadzone: 0.05,
  reverseMin: 1300,
  driveButton: null,
  paddleLeftButton: null,
  paddleRightButton: null
};

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

function axisPercent(value) {
  return clamp01((value + 1) / 2) * 100;
}

function buttonPercent(value) {
  return clamp01(value) * 100;
}

function loadSavedMapping() {
  try {
    const raw = window.localStorage?.getItem(storageKey);
    if (!raw) {
      return;
    }
    const saved = JSON.parse(raw);
    if (!saved || typeof saved !== "object") {
      return;
    }
    Object.assign(mapping, saved);
    migrateSavedMapping();
  } catch (error) {
    console.warn("load saved mapping failed", error);
  }
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

function syncOptionsFromMapping() {
  optionInputs.steeringInvert.checked = Boolean(mapping.steeringInvert);
  optionInputs.throttleInvert.checked = Boolean(mapping.throttleInvert);
  optionInputs.brakeInvert.checked = Boolean(mapping.brakeInvert);
  optionInputs.steeringGain.value = String(mapping.steeringGain ?? 1.0);
  optionInputs.steeringDeadzone.value = String(mapping.steeringDeadzone ?? 0.03);
  optionInputs.pedalDeadzone.value = String(mapping.pedalDeadzone ?? 0.05);
}

function syncMappingFromOptions() {
  mapping.steeringInvert = Boolean(optionInputs.steeringInvert.checked);
  mapping.throttleInvert = Boolean(optionInputs.throttleInvert.checked);
  mapping.brakeInvert = Boolean(optionInputs.brakeInvert.checked);
  mapping.steeringGain = numberFromInput(optionInputs.steeringGain, 1.0);
  mapping.steeringDeadzone = numberFromInput(optionInputs.steeringDeadzone, 0.03);
  mapping.pedalDeadzone = numberFromInput(optionInputs.pedalDeadzone, 0.05);
}

function buildViewerUrl() {
  const url = new URL(openViewerEl?.getAttribute("href") || "./viewer.html", location.href);
  const params = new URLSearchParams(url.hash.replace(/^#\??/, ""));
  if (targetDevice) {
    url.searchParams.set("device", targetDevice);
  }
  params.set("gamepad", "1");
  params.set("gamepadIndex", String(mapping.index ?? 0));
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
  params.set("gamepadThrottleInvert", mapping.throttleInvert ? "1" : "0");
  params.set("gamepadThrottleIdle", String(mapping.throttleIdle ?? 1));
  params.set("gamepadThrottlePressed", String(mapping.throttlePressed ?? -1));
  if (mapping.brakeAxis !== null) {
    params.set("gamepadBrakeAxis", String(mapping.brakeAxis));
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
  url.hash = params.toString();
  return url.toString();
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
      ["driveButton", "drive"],
      ["paddleLeftButton", "paddleLeft"],
      ["paddleRightButton", "paddleRight"]
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
  const pad = document.createElement("section");
  pad.className = "pad";

  const title = document.createElement("h2");
  title.textContent = `#${gamepad.index} ${gamepad.id || t("unknownGamepad")}`;
  pad.appendChild(title);

  const meta = document.createElement("div");
  meta.className = "meta";
  meta.textContent = [
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
    pad.appendChild(makeActions("axis", index));
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
    pad.appendChild(makeActions("button", index));
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
      return t("throttle");
    case "brakeAxis":
      return t("brake");
    case "driveButton":
      return t("drive");
    case "paddleLeftButton":
      return t("paddleLeft");
    case "paddleRightButton":
      return t("paddleRight");
    default:
      return field;
  }
}

function assignInput(field, index) {
  mapping[field] = index;
  updateMappingOutput();
  assignStatusEl.textContent = `${fieldLabel(field)} = ${index}`;
}

function saveMapping() {
  syncMappingFromOptions();
  window.localStorage?.setItem(storageKey, JSON.stringify(mapping));
  updateMappingOutput();
  assignStatusEl.textContent = t("savedForViewer");
}

function clearSavedMapping() {
  window.localStorage?.removeItem(storageKey);
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

function setCapturedBoundary(label, gamepad) {
  const steering = getAxisValue(gamepad, mapping.steeringAxis);
  const throttle = getAxisValue(gamepad, mapping.throttleAxis);
  const brake = getAxisValue(gamepad, mapping.brakeAxis);
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
      selectedGamepadIndex = gamepads[0].index;
      mapping.id = gamepads[0].id || "";
      mapping.index = gamepads[0].index;
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
  selectedGamepadIndex = event.gamepad.index;
  mapping.id = event.gamepad.id || "";
  mapping.index = event.gamepad.index;
  console.log("gamepadconnected", event.gamepad);
});

window.addEventListener("gamepaddisconnected", (event) => {
  console.log("gamepaddisconnected", event.gamepad);
});

padsEl.addEventListener("pointerdown", (event) => {
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
