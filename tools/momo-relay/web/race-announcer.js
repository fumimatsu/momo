(function initRaceAnnouncer(root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.MomoRaceAnnouncer = api;
  }
})(typeof globalThis !== 'undefined' ? globalThis : this, () => {
  'use strict';

  const QUALITY_VOICE_PATTERN = /natural|neural|online|enhanced|premium|google|aria|ava|jenny|sonia|ryan/i;

  function normalizeRemoteLanguage(value, enabled = true) {
    if (!enabled) return 'off';
    const normalized = String(value || '').trim().toLowerCase();
    if (normalized === 'ja' || normalized === 'ja-jp') return 'ja-JP';
    if (normalized === 'off' || normalized === 'none' || normalized === 'disabled') return 'off';
    return 'en-US';
  }

  function buildRemotePreference(language) {
    return JSON.stringify({
      type: 'race_audio_preference',
      version: 1,
      language: normalizeRemoteLanguage(language, language !== 'off'),
    });
  }

  function parseRemoteMessage(message) {
    if (typeof message !== 'string' || !message.startsWith('RACE_AUDIO:')) return null;
    try {
      const payload = JSON.parse(message.slice('RACE_AUDIO:'.length));
      if (!payload || typeof payload !== 'object' || payload.version !== 1) return null;
      if (payload.type !== 'race_audio' && payload.type !== 'race_audio_capabilities') return null;
      return payload;
    } catch (_) {
      return null;
    }
  }

  function createRemoteAudioTracker() {
    let playingEventId = '';
    const pendingEventIds = new Set();

    function snapshot(extra = {}) {
      return Object.freeze({
        playingEventId,
        pendingCount: pendingEventIds.size,
        idle: !playingEventId && pendingEventIds.size === 0,
        ...extra,
      });
    }

    return Object.freeze({
      queue(eventId) {
        const normalized = String(eventId || '').trim();
        const wasIdle = !playingEventId && pendingEventIds.size === 0;
        if (normalized) pendingEventIds.add(normalized);
        return snapshot({ wasIdle });
      },
      play(eventId) {
        const normalized = String(eventId || '').trim();
        if (normalized) {
          pendingEventIds.delete(normalized);
          playingEventId = normalized;
        }
        return snapshot();
      },
      finish(eventId) {
        const normalized = String(eventId || '').trim();
        if (normalized) pendingEventIds.delete(normalized);
        if (!normalized || normalized === playingEventId) playingEventId = '';
        return snapshot();
      },
      reset() {
        playingEventId = '';
        pendingEventIds.clear();
        return snapshot();
      },
      snapshot,
    });
  }

  function finiteNumber(value) {
    const number = Number(value);
    return Number.isFinite(number) ? number : null;
  }

  function buildLapAnnouncement(input = {}) {
    const lap = finiteNumber(input.lap);
    const lapTimeMs = finiteNumber(input.lapTimeMs);
    if (lap === null || lap < 1 || lapTimeMs === null || lapTimeMs <= 0) {
      return null;
    }

    const roundedLap = Math.floor(lap);
    const roundedLapTimeMs = Math.round(lapTimeMs);
    const bestLapMs = finiteNumber(input.bestLapMs);
    const overallBestLapMs = finiteNumber(input.overallBestLapMs);
    const position = finiteNumber(input.position);
    const isBestLap = bestLapMs !== null && Math.round(bestLapMs) === roundedLapTimeMs;
    const isOverallBest = overallBestLapMs !== null && Math.round(overallBestLapMs) === roundedLapTimeMs;
    const segments = [
      `Lap ${roundedLap} complete.`,
      `${(roundedLapTimeMs / 1000).toFixed(3)} seconds.`,
    ];

    if (isOverallBest) {
      segments.push('New overall fastest lap.');
    } else if (isBestLap) {
      segments.push('New personal best.');
    } else if (bestLapMs !== null && roundedLapTimeMs > bestLapMs) {
      segments.push(`${((roundedLapTimeMs - bestLapMs) / 1000).toFixed(3)} seconds off your best.`);
    }

    if (position !== null && position >= 1) {
      segments.push(`Position ${Math.floor(position)}.`);
    }

    return Object.freeze({
      lap: roundedLap,
      lapTimeMs: roundedLapTimeMs,
      isBestLap,
      isOverallBest,
      text: segments.join(' '),
    });
  }

  function buildRaceSummary(input = {}) {
    if (String(input.sessionType || '').trim().toLowerCase() !== 'race') {
      return null;
    }
    const lapTimes = Array.isArray(input.laps)
      ? input.laps
        .map((entry) => finiteNumber(entry?.timeMs))
        .filter((value) => value !== null && value > 0)
        .map(Math.round)
      : [];
    if (lapTimes.length === 0) return null;

    const position = finiteNumber(input.position);
    const fieldSize = finiteNumber(input.fieldSize);
    const totalTimeMs = finiteNumber(input.totalTimeMs);
    const suppliedBestLapMs = finiteNumber(input.bestLapMs);
    const bestLapMs = suppliedBestLapMs !== null && suppliedBestLapMs > 0
      ? Math.round(suppliedBestLapMs)
      : Math.min(...lapTimes);
    const averageLapMs = Math.round(
      lapTimes.reduce((total, lapTimeMs) => total + lapTimeMs, 0) / lapTimes.length,
    );
    const lapTimeTotalMs = lapTimes.reduce((total, lapTimeMs) => total + lapTimeMs, 0);

    return Object.freeze({
      position: position !== null && position >= 1 ? Math.floor(position) : null,
      fieldSize: fieldSize !== null && fieldSize >= 1 ? Math.floor(fieldSize) : null,
      totalTimeMs: totalTimeMs !== null && totalTimeMs > 0
        ? Math.round(totalTimeMs)
        : lapTimeTotalMs,
      bestLapMs,
      averageLapMs,
      completedLaps: lapTimes.length,
    });
  }

  function selectPreferredVoice(voices, options = {}) {
    const candidates = Array.isArray(voices)
      ? voices.filter((voice) => voice && typeof voice.name === 'string' && typeof voice.lang === 'string')
      : [];
    if (candidates.length === 0) {
      return null;
    }

    const preferredName = String(options.preferredName || '').trim();
    if (preferredName) {
      const explicit = candidates.find((voice) => voice.name === preferredName);
      if (explicit) return explicit;
    }

    const language = String(options.language || 'en-US').trim().toLowerCase();
    const primaryLanguage = language.split('-')[0];
    const ranked = candidates.map((voice, index) => {
      const voiceLanguage = voice.lang.toLowerCase();
      const voicePrimaryLanguage = voiceLanguage.split('-')[0];
      let score = voiceLanguage === language ? 100 : voicePrimaryLanguage === primaryLanguage ? 55 : -100;
      if (QUALITY_VOICE_PATTERN.test(voice.name)) score += 35;
      if (voice.localService === false) score += 10;
      if (voice.default === true) score += 5;
      return { voice, score, index };
    });
    ranked.sort((left, right) => right.score - left.score || left.index - right.index);
    return ranked[0].score >= 55 ? ranked[0].voice : null;
  }

  function playTone(context, options) {
    const startAt = context.currentTime + Math.max(0, Number(options.delaySeconds) || 0);
    const duration = Math.max(0.04, (Number(options.durationMs) || 0) / 1000);
    const attack = Math.min(0.012, duration * 0.2);
    const release = Math.min(0.09, duration * 0.42);
    const oscillator = context.createOscillator();
    const gain = context.createGain();
    const frequency = Math.max(20, Number(options.frequency) || 440);
    const endFrequency = Math.max(20, Number(options.endFrequency) || frequency);
    const volume = Math.max(0, Math.min(1, Number(options.volume) || 0));

    oscillator.type = options.type || 'sine';
    oscillator.frequency.setValueAtTime(frequency, startAt);
    if (endFrequency !== frequency) {
      oscillator.frequency.exponentialRampToValueAtTime(endFrequency, startAt + duration);
    }
    gain.gain.setValueAtTime(0, startAt);
    gain.gain.linearRampToValueAtTime(volume, startAt + attack);
    gain.gain.setValueAtTime(volume, Math.max(startAt + attack, startAt + duration - release));
    gain.gain.linearRampToValueAtTime(0, startAt + duration);
    oscillator.connect(gain);
    gain.connect(context.destination);
    oscillator.start(startAt);
    oscillator.stop(startAt + duration + 0.02);
  }

  function playSignal(context, signal, volume = 0.35) {
    if (!context || context.state !== 'running' || typeof context.createOscillator !== 'function') {
      return false;
    }
    const level = Math.max(0, Math.min(1, Number(volume) || 0));
    if (level <= 0) return false;

    if (signal === 'green') {
      playTone(context, {
        type: 'triangle',
        frequency: 440,
        endFrequency: 880,
        durationMs: 360,
        volume: level * 0.82,
      });
      playTone(context, {
        type: 'sine',
        frequency: 660,
        endFrequency: 1320,
        delaySeconds: 0.04,
        durationMs: 330,
        volume: level * 0.44,
      });
      playTone(context, {
        type: 'sine',
        frequency: 1320,
        endFrequency: 1760,
        delaySeconds: 0.16,
        durationMs: 260,
        volume: level * 0.28,
      });
      return true;
    }

    playTone(context, {
      type: 'triangle',
      frequency: 330,
      endFrequency: 260,
      durationMs: 180,
      volume: level * 0.88,
    });
    playTone(context, {
      type: 'sine',
      frequency: 660,
      endFrequency: 520,
      durationMs: 125,
      volume: level * 0.36,
    });
    return true;
  }

  return Object.freeze({
    buildRemotePreference,
    buildLapAnnouncement,
    buildRaceSummary,
    createRemoteAudioTracker,
    normalizeRemoteLanguage,
    parseRemoteMessage,
    playSignal,
    selectPreferredVoice,
  });
});
