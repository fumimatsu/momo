// Shared TeamObserver camera/OSD DOM. No network, authority or application state.
function element(tag, className, text) {
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (text !== undefined) node.textContent = text;
  return node;
}

function applyCarAccent(node, car) {
  if (node && car?.color) node.style.setProperty("--car", car.color);
  return node;
}

function svgElement(tag, attributes = {}) {
  const node = document.createElementNS("http://www.w3.org/2000/svg", tag);
  for (const [name, value] of Object.entries(attributes))
    node.setAttribute(name, String(value));
  return node;
}

function createCameraVital(kind, label, unit) {
  const root = element("div", `camera-vital camera-vital-${kind}`);
  root.dataset.state = "waiting";
  const copy = element("span", "camera-vital-copy");
  copy.append(element("small", "", label));
  const reading = element("strong");
  const value = element("output", "", "--");
  reading.append(value, element("em", "", unit));
  copy.append(reading);
  root.append(copy);
  return { root, value };
}

function createCameraResource(kind, label) {
  const root = element("div", `camera-resource camera-resource-${kind}`);
  root.dataset.state = "waiting";
  const name = element("span", "camera-resource-label", label);
  const track = element("span", "camera-resource-track");
  const fill = element("i", "camera-resource-fill");
  track.append(fill);
  const value = element("output", "", "WAIT");
  root.append(name, track, value);
  return { root, fill, value };
}

export function createObserverCameraTile(car, { onZoom = null } = {}) {
  const tile = applyCarAccent(element("article", "camera-tile"), car);
  tile.dataset.carId = car.carId;
  tile.dataset.vehicleId = car.vehicleId;
  const head = element("div", "camera-head");
  const title = element("strong", "", `CAR ${car.displayNumber} `);
  const driver = element(
    "span",
    "",
    car.driver || car.vehicleName || car.device || "SOURCE UNBOUND",
  );
  title.append(driver);
  const titleNodes = { root: title, driver };
  const status = element("span", "", "");
  status.id = `camera-status-${car.carId}`;
  status.append(element("i"), document.createTextNode("WAITING"));
  const fps = element("em", "", "-- FPS");
  fps.id = `camera-fps-${car.carId}`;
  const zoomButton = element("button", "camera-zoom-toggle");
  zoomButton.type = "button";
  zoomButton.setAttribute("aria-pressed", "false");
  zoomButton.setAttribute(
    "aria-label",
    `Enlarge CAR ${car.displayNumber} onboard video`,
  );
  zoomButton.title = "Enlarge camera";
  zoomButton.append(element("span", "camera-zoom-icon"));
  zoomButton.hidden = !onZoom;
  if (onZoom) zoomButton.addEventListener("click", () => onZoom(car));
  head.append(title, status, fps, zoomButton);
  const feed = element("div", "camera-feed");
  if (onZoom) feed.addEventListener("dblclick", () => onZoom(car));
  const video = document.createElement("video");
  video.id = `video-${car.carId}`;
  video.autoplay = true;
  video.muted = true;
  video.playsInline = true;
  video.classList.toggle("video-flipped", Boolean(car.flip));
  video.setAttribute("aria-label", `CAR ${car.displayNumber} onboard video`);
  const videoState = element("span", "video-state", "WAITING FOR RELAY");
  videoState.id = `video-state-${car.carId}`;
  const eventFlash = element("strong", "camera-event-flash");
  eventFlash.hidden = true;
  const dashboard = element("div", "camera-dashboard");
  dashboard.id = `camera-dashboard-${car.carId}`;
  dashboard.dataset.active = "false";

  const motionCard = element("section", "camera-instrument camera-motion-card");
  motionCard.setAttribute("aria-label", "Vehicle acceleration and yaw");
  const motionStatus = element("div", "camera-motion-status");
  const rate = element("strong", "telemetry-rate", "--Hz");
  const loss = element("span", "telemetry-loss", "L--");
  motionStatus.append(rate, loss);
  const motionValues = element("div", "camera-motion-values");
  const lateral = element("output", "telemetry-lateral", "--");
  const forward = element("output", "telemetry-forward", "--");
  const yaw = element("output", "telemetry-yaw", "--");
  for (const [label, value] of [
    ["LAT G", lateral],
    ["FWD G", forward],
    ["Y rad/s", yaw],
  ]) {
    const reading = element("span");
    reading.append(element("small", "", label), value);
    motionValues.append(reading);
  }
  const motionGauge = element("div", "camera-motion-gauge");
  const motionScope = svgElement("svg", {
    viewBox: "0 0 64 64",
    class: "camera-motion-scope",
    "aria-hidden": "true",
  });
  motionScope.dataset.state = "waiting";
  motionScope.append(
    svgElement("circle", {
      cx: 32,
      cy: 32,
      r: 24,
      class: "camera-motion-ring",
    }),
    svgElement("circle", {
      cx: 32,
      cy: 32,
      r: 12,
      class: "camera-motion-ring inner",
    }),
    svgElement("path", { d: "M6,32H58 M32,6V58", class: "camera-motion-axis" }),
  );
  const motionDot = svgElement("circle", {
    cx: 32,
    cy: 32,
    r: 3.5,
    class: "camera-motion-dot",
  });
  motionScope.append(motionDot);
  motionGauge.append(
    motionScope,
    element("span", "camera-motion-scale", "±1.5 G"),
  );
  motionCard.append(motionGauge, motionValues, motionStatus);

  const powerCard = element("section", "camera-instrument camera-power-card");
  powerCard.setAttribute("aria-label", "ESC powertrain telemetry");
  const rpmRow = element("div", "camera-rpm camera-speed");
  const rpmLabel = element("span", "", car.speedProfile ? "EST KM/H" : "RPM");
  const rpm = element("output", "", "--");
  const rpmTrack = element("span", "camera-rpm-track");
  const rpmFill = element("i", "camera-rpm-fill");
  rpmTrack.append(rpmFill);
  rpmRow.append(rpmLabel, rpm, rpmTrack);
  const vitalRow = element("div", "camera-vitals");
  const voltage = createCameraVital("battery", "BAT", "V");
  const escTemp = createCameraVital("esc", "ESC", "°C");
  const motorTemp = createCameraVital("motor", "MTR", "°C");
  vitalRow.append(voltage.root, escTemp.root, motorTemp.root);
  powerCard.append(rpmRow, vitalRow);

  const resourceCard = element(
    "section",
    "camera-instrument camera-resource-card",
  );
  resourceCard.setAttribute("aria-label", "Damage, fuel and boost status");
  const resourceHead = element("div", "camera-resource-head");
  resourceHead.append(
    element("span", "", "VEHICLE"),
    element("strong", "camera-gear", "G--"),
  );
  const damage = createCameraResource("damage", "DMG");
  const fuel = createCameraResource("fuel", "FUEL");
  const boost = createCameraResource("boost", "BOOST");
  resourceCard.append(resourceHead, damage.root, fuel.root, boost.root);

  const telemetry = {
    root: dashboard,
    powerRoot: powerCard,
    motionRoot: motionCard,
    motionScope,
    motionDot,
    rate,
    loss,
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
  };
  const health = {
    gear: resourceHead.lastChild,
    damage,
    fuel,
    boost,
  };
  const controls = element("section", "camera-controls");
  controls.dataset.state = "waiting";
  const controlHead = element("div", "camera-control-head");
  const controlStatus = element("span", "camera-control-status", "WAITING");
  const throttleValue = element("output", "", "--");
  const brakeValue = element("output", "", "--");
  const throttleLabel = element("span", "camera-control-throttle", "THR ");
  const brakeLabel = element("span", "camera-control-brake", "BRK ");
  throttleLabel.append(throttleValue);
  brakeLabel.append(brakeValue);
  controlHead.append(controlStatus, throttleLabel, brakeLabel);
  const chart = svgElement("svg", {
    viewBox: "0 0 240 44",
    preserveAspectRatio: "none",
    "aria-hidden": "true",
    class: "camera-control-chart",
  });
  chart.append(
    svgElement("path", {
      d: "M0,2H240 M0,22H240 M0,42H240",
      class: "camera-control-grid",
    }),
  );
  const throttle = svgElement("path", {
    class: "camera-control-line camera-control-throttle",
  });
  const brake = svgElement("path", {
    class: "camera-control-line camera-control-brake",
  });
  chart.append(throttle, brake);
  const chartScale = element("div", "camera-control-scale");
  chartScale.append(
    element("span", "", "0–100%"),
    element("span", "", "−8s → NOW"),
  );
  controls.append(controlHead, chart, chartScale);
  const control = {
    root: controls,
    throttle,
    brake,
    throttleValue,
    brakeValue,
    status: controlStatus,
  };
  const readings = element("div", "camera-readings");
  readings.append(motionCard, powerCard);
  dashboard.append(controls, resourceCard, readings);
  feed.append(video, videoState, eventFlash);
  tile.append(head, feed, dashboard);
  return {
    tile,
    title: titleNodes,
    status,
    fps,
    zoomButton,
    video,
    videoState,
    telemetry,
    health,
    control,
    effect: { root: tile, badge: eventFlash },
  };
}
