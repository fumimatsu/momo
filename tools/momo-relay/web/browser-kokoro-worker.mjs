const KOKORO_MODULE_URL = 'https://cdn.jsdelivr.net/npm/kokoro-js@1.2.1/dist/kokoro.web.js';
const MODEL_ID = 'onnx-community/Kokoro-82M-v1.0-ONNX';

let runtime = null;

function postFailure(requestId, error) {
  self.postMessage({
    type: 'error',
    requestId,
    message: error instanceof Error ? error.message : String(error),
  });
}

async function loadRuntime(requestId, config) {
  if (config?.device !== 'webgpu' || config?.dtype !== 'fp32') {
    throw new Error('Pilot Browser Kokoro requires WebGPU with FP32');
  }
  if (runtime) {
    self.postMessage({ type: 'loaded', requestId, loadMs: 0, cachedRuntime: true });
    return;
  }
  const started = performance.now();
  const { KokoroTTS } = await import(KOKORO_MODULE_URL);
  runtime = await KokoroTTS.from_pretrained(MODEL_ID, {
    device: 'webgpu',
    dtype: 'fp32',
    progress_callback: (progress) => self.postMessage({ type: 'progress', requestId, progress }),
  });
  self.postMessage({
    type: 'loaded',
    requestId,
    loadMs: Math.round(performance.now() - started),
    cachedRuntime: false,
  });
}

async function generate(requestId, prompt) {
  if (!runtime) throw new Error('Kokoro runtime is not loaded');
  if (!prompt?.phonemes || !Array.isArray(prompt.modelInputIds) || prompt.modelInputIds.length < 3) {
    throw new Error('Precomputed Kokoro prompt is required');
  }
  const tokenized = runtime.tokenizer(prompt.phonemes, { truncation: true });
  const template = tokenized.input_ids;
  const useBigInt = template.data instanceof BigInt64Array || template.data instanceof BigUint64Array;
  const InputArray = template.data.constructor;
  const inputData = InputArray.from(
    prompt.modelInputIds,
    (value) => useBigInt ? BigInt(value) : Number(value),
  );
  const inputIds = new template.constructor(template.type, inputData, [1, inputData.length]);
  const started = performance.now();
  const audio = await runtime.generate_from_ids(inputIds, {
    voice: prompt.voice,
    speed: prompt.speed,
  });
  const source = audio.audio || audio.data;
  if (!source || source.length === 0) throw new Error('Kokoro returned empty audio');
  const samples = Float32Array.from(source);
  self.postMessage({
    type: 'generated',
    requestId,
    promptId: prompt.promptId,
    generationMs: Math.round(performance.now() - started),
    sampleRate: Number(audio.sampling_rate || audio.sample_rate || 24000),
    samples: samples.buffer,
  }, [samples.buffer]);
}

async function warmRuntime(requestId, config) {
  if (!runtime) throw new Error('Kokoro runtime is not loaded');
  const voice = config?.voice === 'jf_alpha' ? 'jf_alpha' : 'am_michael';
  const inputIds = runtime.tokenizer('a', { truncation: true }).input_ids;
  const started = performance.now();
  const audio = await runtime.generate_from_ids(inputIds, { voice, speed: 1 });
  const source = audio.audio || audio.data;
  if (!source || source.length === 0) throw new Error('Kokoro warm-up returned empty audio');
  self.postMessage({
    type: 'warmed',
    requestId,
    warmupMs: Math.round(performance.now() - started),
  });
}

self.addEventListener('message', async (event) => {
  const { type, requestId, config, prompt } = event.data || {};
  try {
    if (type === 'load') {
      await loadRuntime(requestId, config);
    } else if (type === 'warmup') {
      await warmRuntime(requestId, config);
    } else if (type === 'generate') {
      await generate(requestId, prompt);
    }
  } catch (error) {
    postFailure(requestId, error);
  }
});
