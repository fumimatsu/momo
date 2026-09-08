// One frame counter serves the visible FPS and the optional Relay log.
const round = value => value === null ? null : Math.round(value * 1000) / 1000;

export class ObserverScreenStats {
  constructor(video, { now = () => performance.now(), wallNow = () => Date.now(),
    visibility = () => document.visibilityState } = {}) {
    this.video = video;
    this.now = now;
    this.wallNow = wallNow;
    this.visibility = visibility;
    this.startedAt = now();
    this.sequence = 0;
    this.frameCount = 0;
    this.lastFrameAt = null;
    this.visibilityChanged = false;
  }

  noteFrame() {
    this.frameCount++;
    this.lastFrameAt = this.now();
  }

  takeWindow() {
    const now = this.now();
    const windowMs = now - this.startedAt;
    if (!(windowMs > 0)) return null;
    const supported = typeof this.video.requestVideoFrameCallback === 'function';
    const sample = {
      version: 2, sequence: ++this.sequence, clientSampledAtUnixMs: this.wallNow(),
      windowMs: round(windowMs), visibility: this.visibility(), visibilityChanged: this.visibilityChanged,
      videoWidth: this.video.videoWidth, videoHeight: this.video.videoHeight,
      renderCallbackFps: supported ? round(this.frameCount * 1000 / windowMs) : null,
      videoFrameAgeMs: round(this.lastFrameAt === null ? null : now - this.lastFrameAt),
    };
    this.frameCount = 0;
    this.startedAt = now;
    this.visibilityChanged = false;
    return sample;
  }
}
