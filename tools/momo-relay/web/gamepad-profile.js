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
  };
})();
