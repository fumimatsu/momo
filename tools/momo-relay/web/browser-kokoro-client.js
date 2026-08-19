(function initBrowserKokoroClient(root, factory) {
  const api = factory(root);
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
  }
  if (root) {
    root.MomoBrowserKokoro = api;
  }
})(typeof globalThis !== 'undefined' ? globalThis : this, (root) => {
  'use strict';

  const MODEL_ID = 'onnx-community/Kokoro-82M-v1.0-ONNX';
  const MODE = 'browser-kokoro';

  function normalizePrompt(input) {
    if (!input || typeof input !== 'object') throw new Error('Kokoro prompt is required');
    if (input.version !== 1 || input.engine !== 'kokoro' || input.modelId !== MODEL_ID) {
      throw new Error('Unsupported Kokoro prompt contract');
    }
    const language = input.language === 'ja-JP' ? 'ja-JP' : input.language === 'en-US' ? 'en-US' : '';
    const voice = String(input.voice || '').trim();
    const phonemes = String(input.phonemes || '');
    const speed = Number(input.speed);
    const modelInputIds = Array.isArray(input.modelInputIds)
      ? input.modelInputIds.map((value) => Number(value))
      : [];
    if (!language || !voice || !phonemes || phonemes.length > 4096 ||
        !Number.isFinite(speed) || speed < 0.5 || speed > 2 ||
        modelInputIds.length < 3 || modelInputIds.length > 1024 ||
        modelInputIds[0] !== 0 || modelInputIds.at(-1) !== 0 ||
        modelInputIds.some((value) => !Number.isSafeInteger(value) || value < 0)) {
      throw new Error('Invalid Kokoro prompt payload');
    }
    return Object.freeze({
      ...input,
      language,
      voice,
      phonemes,
      speed,
      modelInputIds: Object.freeze(modelInputIds),
    });
  }

  function isSupported(environment = root) {
    return typeof environment?.Worker === 'function' && Boolean(environment?.navigator?.gpu);
  }

  function createClient(options = {}) {
    const environment = options.environment || root;
    const WorkerCtor = options.Worker || environment?.Worker;
    if (typeof WorkerCtor !== 'function') throw new Error('Web Worker is unavailable');
    const workerUrl = String(options.workerUrl || './browser-kokoro-worker.mjs');
    const onAudio = typeof options.onAudio === 'function' ? options.onAudio : () => {};
    const onError = typeof options.onError === 'function' ? options.onError : () => {};
    const onState = typeof options.onState === 'function' ? options.onState : () => {};
    const onDropped = typeof options.onDropped === 'function' ? options.onDropped : () => {};
    let worker = null;
    let state = 'idle';
    let loadPromise = null;
    let resolveLoad = null;
    let rejectLoad = null;
    let requestSerial = 0;
    let latestSequence = 0;
    let active = null;
    let pending = null;
    let warmupState = 'idle';
    let warmupPromise = null;
    let resolveWarmup = null;
    let rejectWarmup = null;
    let warmupRequestId = '';

    function snapshot() {
      return Object.freeze({
        state,
        activePromptId: active?.prompt.promptId || null,
        pendingPromptId: pending?.prompt.promptId || null,
        latestSequence,
        warmupState,
      });
    }

    function setState(nextState, detail = {}) {
      state = nextState;
      onState(snapshot(), detail);
    }

    function failLoad(error) {
      const failure = error instanceof Error ? error : new Error(String(error));
      setState('failed', { error: failure });
      rejectLoad?.(failure);
      resolveLoad = null;
      rejectLoad = null;
      if (pending) {
        const failed = pending;
        pending = null;
        onError(failed.prompt, failure);
      }
    }

		function handleWorkerFailure(error) {
			const failure = error instanceof Error ? error : new Error(String(error));
			if (state === 'loading') {
				failLoad(failure);
				return;
			}
			if (warmupState === 'running') {
				warmupState = 'failed';
				warmupRequestId = '';
				const reject = rejectWarmup;
				resolveWarmup = null;
				rejectWarmup = null;
				warmupPromise = null;
				reject?.(failure);
			}
			setState('failed', { error: failure });
			const failed = pending || (active && !active.dropped ? active : null);
			if (active && active !== failed && !active.dropped) {
				onDropped(active.prompt, 'worker-crashed-during-generation');
			}
			active = null;
			pending = null;
			if (failed) onError(failed.prompt, failure);
		}

    function pump() {
      if (state !== 'ready' || warmupState === 'running' || active || !pending || !worker) return;
      active = pending;
      pending = null;
      const requestId = `generate-${++requestSerial}`;
      active.requestId = requestId;
      worker.postMessage({ type: 'generate', requestId, prompt: active.prompt });
    }

    function handleMessage(event) {
      const message = event?.data || {};
      if (message.type === 'loaded' && state === 'loading') {
        setState('ready', { loadMs: Number(message.loadMs) || 0 });
        resolveLoad?.(snapshot());
        resolveLoad = null;
        rejectLoad = null;
        pump();
        return;
      }
      if (message.type === 'progress') {
        onState(snapshot(), { progress: message.progress });
        return;
      }
      if (message.type === 'warmed' && warmupState === 'running' &&
          warmupRequestId === message.requestId) {
        warmupState = 'ready';
        warmupRequestId = '';
        const resolve = resolveWarmup;
        resolveWarmup = null;
        rejectWarmup = null;
        warmupPromise = null;
        onState(snapshot(), { warmup: true, warmupMs: Number(message.warmupMs) || 0 });
        resolve?.(snapshot());
        pump();
        return;
      }
      if (message.type === 'generated' && active?.requestId === message.requestId) {
        const completed = active;
        active = null;
        if (completed.sequence === latestSequence && !completed.dropped) {
          onAudio(completed.prompt, {
            generationMs: Number(message.generationMs) || 0,
            sampleRate: Number(message.sampleRate) || 24000,
            samples: message.samples,
          });
        } else if (!completed.dropped) {
          onDropped(completed.prompt, 'superseded-after-generation');
        }
        pump();
        return;
      }
      if (message.type === 'error') {
        const error = new Error(String(message.message || 'Browser Kokoro worker failed'));
        if (state === 'loading') {
          failLoad(error);
          return;
        }
        if (warmupState === 'running' && warmupRequestId === message.requestId) {
          warmupState = 'failed';
          warmupRequestId = '';
          const reject = rejectWarmup;
          resolveWarmup = null;
          rejectWarmup = null;
          warmupPromise = null;
          onState(snapshot(), { warmup: true, error });
          reject?.(error);
          return;
        }
        if (active?.requestId === message.requestId) {
          const failed = active;
          active = null;
          onError(failed.prompt, error);
          pump();
        }
      }
    }

    function load() {
      if (state === 'ready') return Promise.resolve(snapshot());
      if (loadPromise) return loadPromise;
      if (state === 'failed') return Promise.reject(new Error('Browser Kokoro runtime is unavailable'));
      worker = new WorkerCtor(workerUrl, { type: 'module' });
      worker.addEventListener('message', handleMessage);
      worker.addEventListener('error', (event) => {
			handleWorkerFailure(event?.error || event?.message || 'Worker crashed');
		});
      setState('loading');
      loadPromise = new Promise((resolve, reject) => {
        resolveLoad = resolve;
        rejectLoad = reject;
      });
      worker.postMessage({
        type: 'load',
        requestId: `load-${++requestSerial}`,
        config: { device: 'webgpu', dtype: 'fp32' },
      });
      return loadPromise;
    }

    function warmup(options = {}) {
      if (state !== 'ready' || !worker) {
        return Promise.reject(new Error('Browser Kokoro runtime is not ready'));
      }
      if (warmupState === 'ready') return Promise.resolve(snapshot());
      if (warmupPromise) return warmupPromise;
      if (warmupState === 'failed') {
        return Promise.reject(new Error('Browser Kokoro warm-up is unavailable'));
      }
      if (active || pending) {
        return Promise.reject(new Error('Browser Kokoro is busy'));
      }
      const voice = options.voice === 'jf_alpha' ? 'jf_alpha' : 'am_michael';
      warmupState = 'running';
      warmupRequestId = `warmup-${++requestSerial}`;
      warmupPromise = new Promise((resolve, reject) => {
        resolveWarmup = resolve;
        rejectWarmup = reject;
      });
      onState(snapshot(), { warmupStarted: true });
      worker.postMessage({
        type: 'warmup',
        requestId: warmupRequestId,
        config: { voice },
      });
      return warmupPromise;
    }

    function enqueue(input) {
      const prompt = normalizePrompt(input);
      const item = { prompt, sequence: ++latestSequence, requestId: '', dropped: false };
      if (active && !active.dropped) {
        active.dropped = true;
        onDropped(active.prompt, 'superseded-during-generation');
      }
      if (pending) onDropped(pending.prompt, 'superseded-before-generation');
      pending = item;
      if (state === 'idle') {
        load().catch(() => {});
      } else {
        pump();
      }
      return snapshot();
    }

    function clear() {
      latestSequence++;
		if (active && !active.dropped) {
			active.dropped = true;
			onDropped(active.prompt, 'cleared-during-generation');
		}
      if (pending) onDropped(pending.prompt, 'cleared-before-generation');
      pending = null;
      return snapshot();
    }

    function shutdown() {
		if (active && !active.dropped) onDropped(active.prompt, 'shutdown-during-generation');
		if (pending) onDropped(pending.prompt, 'shutdown-before-generation');
      worker?.terminate?.();
      worker = null;
      active = null;
      pending = null;
      loadPromise = null;
      resolveLoad = null;
      rejectLoad = null;
      warmupState = 'idle';
      warmupPromise = null;
      resolveWarmup = null;
      rejectWarmup = null;
      warmupRequestId = '';
      setState('idle');
    }

    return Object.freeze({ load, warmup, enqueue, clear, shutdown, snapshot });
  }

  return Object.freeze({ MODEL_ID, MODE, createClient, isSupported, normalizePrompt });
});
