(() => {
  'use strict';

  const axisFields = ['steeringAxis', 'throttleAxis', 'brakeAxis'];
  const buttonFields = ['throttleButton', 'brakeButton', 'driveButton', 'paddleLeftButton', 'paddleRightButton', 'ffbPresetButton', 'menuButton'];
  const unitFields = ['steeringCenter', 'steeringLeft', 'steeringRight', 'throttleIdle', 'throttlePressed', 'brakeIdle', 'brakePressed'];
  const boolFields = ['steeringInvert', 'throttleInvert', 'brakeInvert'];
  const defaults = { steeringInvert: false, throttleInvert: false, brakeInvert: false, steeringGain: 1,
    steeringDeadzone: 0.03, pedalDeadzone: 0.05 };
  const fields = [...axisFields, ...buttonFields, ...unitFields, ...boolFields, 'steeringGain', 'steeringDeadzone', 'pedalDeadzone'];
  const normalizedId = id => String(id || '').trim().toLowerCase().replace(/\s+/g, ' ');
  const keyFor = pad => `${normalizedId(pad.id)}|axes:${pad.axes.length}|buttons:${pad.buttons.length}|mapping:${pad.mapping || ''}`;

  function validate(profile) {
    const require = valid => { if (!valid) throw new Error('ハンコンの識別情報またはキャリブレーションが不正です。'); };
    require(profile && typeof profile.id === 'string' && profile.id.trim() && profile.id.length <= 256 && !/[\x00-\x1f\x7f]/.test(profile.id));
    require(Number.isInteger(profile.axes) && profile.axes >= 1 && profile.axes <= 64);
    require(Number.isInteger(profile.buttons) && profile.buttons >= 0 && profile.buttons <= 128);
    require(['', 'standard'].includes(profile.gamepadMapping) && Number.isInteger(profile.revision) && profile.revision >= 0);
    require(profile.key === `${normalizedId(profile.id)}|axes:${profile.axes}|buttons:${profile.buttons}|mapping:${profile.gamepadMapping}`);
    const c = profile.calibration;
    require(c && Object.keys(c).length === fields.length && Object.keys(c).every(f => fields.includes(f)));
    const number = (name, min, max) => require(Number.isFinite(c[name]) && c[name] >= min && c[name] <= max);
    for (const f of axisFields) { number(f, f === 'steeringAxis' ? 0 : -1, profile.axes - 1); require(Number.isInteger(c[f])); }
    for (const f of buttonFields) { number(f, -1, profile.buttons - 1); require(Number.isInteger(c[f])); }
    for (const f of unitFields) number(f, -1, 1);
    for (const f of boolFields) require(typeof c[f] === 'boolean');
    number('steeringGain', 0.1, 10);
    number('steeringDeadzone', 0, 0.5);
    number('pedalDeadzone', 0, 0.5);
    const left = c.steeringLeft - c.steeringCenter, right = c.steeringRight - c.steeringCenter;
    require(Math.abs(left) >= 0.15 && Math.abs(right) >= 0.15 && left * right < 0);
    for (const pedal of ['throttle', 'brake']) {
      require(c[`${pedal}Axis`] >= 0 || c[`${pedal}Button`] >= 0);
      require(Math.abs(c[`${pedal}Pressed`] - c[`${pedal}Idle`]) >= 0.15);
    }
    if (c.throttleButton < 0 && c.brakeButton < 0 && c.throttleAxis === c.brakeAxis)
      require((c.throttlePressed - c.throttleIdle) * (c.brakePressed - c.brakeIdle) < 0);
    return profile;
  }

  function fromMapping(mapping, pad, revision = 0) {
    if (!pad?.connected || normalizedId(mapping.id) !== normalizedId(pad.id))
      throw new Error('保存する設定と接続中のハンコンが一致しません。');
    const calibration = {};
    for (const f of fields) calibration[f] = mapping[f] ?? (axisFields.includes(f) || buttonFields.includes(f) ? -1 : defaults[f]);
    return validate({ key: keyFor(pad), id: pad.id, axes: pad.axes.length, buttons: pad.buttons.length,
      gamepadMapping: pad.mapping || '', revision, calibration });
  }

  function apply(profile, mapping, pad) {
    validate(profile);
    if (profile.key !== keyFor(pad)) throw new Error('接続中のハンコンに合う設定ではありません。');
    // No PWM limits, car settings, FFB gains, connection URLs or browser indices cross this boundary.
    const identity = window.FpvGamepadProfiles.parseGamepadIdentity(pad.id);
    return { ...mapping, ...profile.calibration, id: pad.id, index: pad.index,
      profileKey: identity.key, vendorId: identity.vendorId, productId: identity.productId,
      inputProfileKey: profile.key };
  }

  function rpc(url, request, responseType, timeoutMs = 1800) {
    return new Promise((resolve, reject) => {
      let socket, timer, done = false;
      const finish = (error, value) => {
        if (done) return;
        done = true;
        clearTimeout(timer);
        if (socket) {
          socket.onmessage = socket.onerror = socket.onclose = socket.onopen = null;
          socket.close(); // Deliberately no stopAll, heartbeat, acquireDevice or releaseDevice.
        }
        error ? reject(error) : resolve(value);
      };
      try {
        socket = new WebSocket(url);
        timer = setTimeout(() => finish(new Error('FFB Bridgeに接続できません。')), timeoutMs);
        socket.onopen = () => socket.send(JSON.stringify({ type: 'hello', protocol: 2 }));
        socket.onmessage = event => {
          try {
            if (typeof event.data !== 'string' || event.data.length > 256 * 1024) throw new Error('Bridge応答が不正です。');
            const message = JSON.parse(event.data);
            if (message.type === 'helloAck') {
              if (!message.features?.includes('inputProfilesV1')) {
                finish(new Error('PC共通保存にはFFB Bridgeの更新が必要です。'));
              } else socket.send(JSON.stringify(request));
            } else if (message.type === 'error') {
              const error = new Error(message.message || 'PC共通設定を読み書きできません。');
              error.profileError = true;
              finish(error);
            } else if (message.type === responseType) finish(null, message);
          } catch (error) { error.profileError = true; finish(error); }
        };
        socket.onerror = socket.onclose = () => finish(new Error('FFB Bridgeに接続できません。'));
      } catch (error) { finish(error); }
    });
  }

  // One read per page/settings open, one write per save. Nothing is polled or persisted per input frame.
  function createClient(url) {
    let state = 'idle', detail = '', profiles = new Map(), generation = 0;
    return {
      get state() { return state; }, get detail() { return detail; }, get generation() { return generation; },
      profileFor: pad => pad ? profiles.get(keyFor(pad)) || null : null,
      async load() {
        state = 'loading'; generation++;
        try {
          const { store } = await rpc(url, { type: 'getInputProfiles' }, 'inputProfiles');
          try {
            if (store?.version !== 1 || !Array.isArray(store.profiles) || store.profiles.length > 32) throw new Error('PC共通設定の形式が不正です。');
            const next = new Map();
            for (const p of store.profiles) {
              validate(p);
              if (p.revision < 1 || next.has(p.key)) throw new Error('PC共通設定の識別情報が重複しています。');
              next.set(p.key, p);
            }
            profiles = next;
          } catch (error) { error.profileError = true; throw error; }
          state = 'ready'; detail = '';
        } catch (error) {
          state = error.profileError ? 'error' : 'unavailable'; detail = error.message;
          profiles = new Map();
        }
        generation++;
        return state;
      },
      async save(mapping, pad) {
        if (state !== 'ready') throw new Error(detail || 'PC共通設定の読み込み完了を待ってください。');
        const profile = fromMapping(mapping, pad, profiles.get(keyFor(pad))?.revision || 0);
        const result = await rpc(url, { type: 'saveInputProfile', profile }, 'inputProfileSaved');
        validate(result.profile);
        if (result.profile.key !== profile.key || result.profile.revision !== profile.revision + 1
            || fields.some(f => result.profile.calibration[f] !== profile.calibration[f]))
          throw new Error('PC共通設定の保存確認に失敗しました。設定画面を開き直してください。');
        profiles.set(profile.key, result.profile); generation++;
        return result.profile;
      },
    };
  }

  window.MomoInputProfileBridge = { fields, normalizedId, keyFor, validate, fromMapping, apply, createClient };
})();
