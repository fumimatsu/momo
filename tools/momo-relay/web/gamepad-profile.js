(() => {
  'use strict';

  const STORAGE_KEY = 'fpvGamepadProfiles';
  const STORE_VERSION = 1;

  function normalizeHex(value) {
    return String(value || '').toLowerCase().padStart(4, '0');
  }

  function parseGamepadIdentity(id) {
    const source = String(id || '').trim();
    const patterns = [
      /vendor:\s*(?:0x)?([0-9a-f]{4})\s+product:\s*(?:0x)?([0-9a-f]{4})/i,
      /vid[_:\s-]*(?:0x)?([0-9a-f]{4}).*pid[_:\s-]*(?:0x)?([0-9a-f]{4})/i,
      /vendor(?:id)?[_:\s-]*(?:0x)?([0-9a-f]{4}).*product(?:id)?[_:\s-]*(?:0x)?([0-9a-f]{4})/i,
      /^([0-9a-f]{4})-([0-9a-f]{4})-/i,
    ];

    for (const pattern of patterns) {
      const match = source.match(pattern);
      if (match) {
        const vendorId = normalizeHex(match[1]);
        const productId = normalizeHex(match[2]);
        return {
          id: source,
          vendorId,
          productId,
          key: `vid:${vendorId}:pid:${productId}`,
          label: `VID ${vendorId.toUpperCase()} / PID ${productId.toUpperCase()}`,
        };
      }
    }

    const normalizedId = source.toLowerCase().replace(/\s+/g, ' ').trim() || 'unknown';
    return {
      id: source,
      vendorId: '',
      productId: '',
      key: `id:${normalizedId}`,
      label: source || 'Unknown gamepad',
    };
  }

  function emptyStore() {
    return {
      version: STORE_VERSION,
      activeProfileKey: '',
      profiles: {},
    };
  }

  function normalizeStore(value) {
    if (!value || typeof value !== 'object') {
      return emptyStore();
    }
    const profiles = value.profiles && typeof value.profiles === 'object'
      ? value.profiles
      : {};
    return {
      version: STORE_VERSION,
      activeProfileKey: typeof value.activeProfileKey === 'string' ? value.activeProfileKey : '',
      profiles: { ...profiles },
    };
  }

  function storageKey(scope) {
    const normalizedScope = String(scope || '').trim();
    return normalizedScope ? `${STORAGE_KEY}:${encodeURIComponent(normalizedScope)}` : STORAGE_KEY;
  }

  function load(storage, scope) {
    try {
      const raw = storage?.getItem(storageKey(scope));
      return raw ? normalizeStore(JSON.parse(raw)) : emptyStore();
    } catch (_) {
      return emptyStore();
    }
  }

  function persist(storage, store, scope) {
    const normalized = normalizeStore(store);
    storage?.setItem(storageKey(scope), JSON.stringify(normalized));
    return normalized;
  }

  function saveProfile(storage, store, profileKey, profile, scope) {
    const next = normalizeStore(store);
    next.activeProfileKey = profileKey;
    next.profiles[profileKey] = { ...profile, profileKey };
    return persist(storage, next, scope);
  }

  function removeProfile(storage, store, profileKey, scope) {
    const next = normalizeStore(store);
    delete next.profiles[profileKey];
    if (next.activeProfileKey === profileKey) {
      next.activeProfileKey = '';
    }
    return persist(storage, next, scope);
  }

  function profileForGamepad(store, gamepad) {
    if (!gamepad) {
      return null;
    }
    const identity = parseGamepadIdentity(gamepad.id);
    const profile = normalizeStore(store).profiles[identity.key];
    return profile ? { profile: { ...profile }, identity, gamepad } : null;
  }

  function findCalibrationChange(base, current, {
    excludedAxes = [],
    excludedButtons = [],
    allowAxes = true,
    allowButtons = true,
    minDelta = 0.15,
  } = {}) {
    if (!current || typeof current !== 'object') {
      return null;
    }
    const excludedAxisSet = new Set(excludedAxes);
    const excludedButtonSet = new Set(excludedButtons);
    let candidate = null;

    if (allowAxes) {
      for (let index = 0; index < (current.axes?.length || 0); index += 1) {
        if (excludedAxisSet.has(index)) continue;
        const value = Number(current.axes[index]);
        const baseValue = Number(base?.axes?.[index]);
        const delta = Math.abs(value - (Number.isFinite(baseValue) ? baseValue : value));
        if (!candidate || delta > candidate.delta) {
          candidate = { type: 'axis', index, delta };
        }
      }
    }

    if (allowButtons) {
      for (let index = 0; index < (current.buttons?.length || 0); index += 1) {
        if (excludedButtonSet.has(index)) continue;
        const value = Number(current.buttons[index]);
        const baseValue = Number(base?.buttons?.[index]);
        const delta = Math.abs(value - (Number.isFinite(baseValue) ? baseValue : value));
        if (!candidate || delta > candidate.delta) {
          candidate = { type: 'button', index, delta };
        }
      }
    }

    const threshold = Number.isFinite(Number(minDelta)) ? Math.max(0, Number(minDelta)) : 0.15;
    return candidate && candidate.delta >= threshold ? candidate : null;
  }

  function haveOppositeCalibrationDirections(firstIdle, firstPressed, secondIdle, secondPressed) {
    const firstDelta = Number(firstPressed) - Number(firstIdle);
    const secondDelta = Number(secondPressed) - Number(secondIdle);
    if (!Number.isFinite(firstDelta) || !Number.isFinite(secondDelta)) return false;
    const firstDirection = Math.sign(firstDelta);
    const secondDirection = Math.sign(secondDelta);
    return firstDirection !== 0 && secondDirection !== 0 && firstDirection !== secondDirection;
  }

  window.FpvGamepadProfiles = {
    STORAGE_KEY,
    STORE_VERSION,
    storageKey,
    emptyStore,
    load,
    persist,
    saveProfile,
    removeProfile,
    parseGamepadIdentity,
    profileForGamepad,
    findCalibrationChange,
    haveOppositeCalibrationDirections,
  };
})();
