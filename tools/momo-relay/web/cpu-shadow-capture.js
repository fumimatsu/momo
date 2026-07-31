(() => {
  'use strict';

  const CAPTURE_SCHEMA = 'momo-fpv-cpu-shadow-capture/v1';
  const UI_FLAG = 'cpuCapture';
  const PANEL_ID = 'fpvCpuShadowCapturePanel';
  const EVENT_TELEMETRY = 'fpv-shadow-telemetry';
  const EVENT_COMMAND = 'fpv-shadow-command';
  const EVENT_DRIVE = 'fpv-shadow-drive';
  const EVENT_STOPPED = 'fpv-shadow-capture-stopped';
  const DIAGNOSTIC_INTERVAL_MS = 1000;
  const MAX_CAPTURE_DURATION_MS = 15 * 60 * 1000;

  function getUrlParams() {
    const params = new URLSearchParams(location.search);
    const hash = location.hash.startsWith('#') ? location.hash.slice(1) : location.hash;
    if (hash) {
      for (const [key, value] of new URLSearchParams(hash)) {
        params.set(key, value);
      }
    }
    return params;
  }

  function isEnabled(value) {
    return ['1', 'true', 'yes', 'on'].includes(String(value || '').toLowerCase());
  }

  function chooseVideoMimeType() {
    if (!window.MediaRecorder) {
      return '';
    }
    const candidates = [
      'video/webm;codecs=vp9',
      'video/webm;codecs=vp8',
      'video/webm',
    ];
    return candidates.find((candidate) => MediaRecorder.isTypeSupported(candidate)) || '';
  }

  function createRunId() {
    const timestamp = new Date().toISOString().replaceAll(/[-:.]/g, '');
    const random = crypto.getRandomValues(new Uint32Array(1))[0]
      .toString(16)
      .padStart(8, '0');
    return `cpu-shadow-${timestamp}-${random}`;
  }

  function finiteOrNull(value) {
    return Number.isFinite(value) ? value : null;
  }

  function errorText(error) {
    return error?.message || String(error || 'unknown error');
  }

  function downloadBlob(blob, filename) {
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement('a');
    anchor.href = url;
    anchor.download = filename;
    anchor.style.display = 'none';
    document.body.appendChild(anchor);
    anchor.click();
    anchor.remove();
    window.setTimeout(() => URL.revokeObjectURL(url), 1000);
  }

  function snapshotTrack(track) {
    if (!track) {
      return null;
    }
    let settings = null;
    try {
      settings = typeof track.getSettings === 'function' ? track.getSettings() : null;
    } catch {
      settings = null;
    }
    return {
      id: track.id || null,
      kind: track.kind || null,
      label: track.label || null,
      enabled: track.enabled === true,
      muted: track.muted === true,
      ready_state: track.readyState || null,
      settings,
    };
  }

  class CpuShadowCapture {
    constructor(video) {
      this.video = video;
      this.running = false;
      this.stopping = false;
      this.runId = '';
      this.startedAtPerformanceMs = 0;
      this.startedAtEpochMs = 0;
      this.records = [];
      this.videoChunks = [];
      this.mediaRecorder = null;
      this.recorderStopSeen = false;
      this.recorderStopPromise = null;
      this.resolveRecorderStop = null;
      this.recordedTrack = null;
      this.videoFrameCallbackId = null;
      this.diagnosticTimer = null;
      this.durationTimer = null;
      this.pendingStop = null;
      this.lastArtifacts = null;
      this.lastArtifactsDownloadRequested = false;
      this.chunkIndex = 0;
      this.counts = {
        frame: 0,
        telemetry: 0,
        command: 0,
        drive: 0,
        diagnostic: 0,
        recorder_chunk: 0,
      };
      this.onTelemetry = (event) => this.append('telemetry', event.detail || {});
      this.onCommand = (event) => this.append('command', event.detail || {});
      this.onDrive = (event) => this.append('drive', event.detail || {});
      this.onTrackEnded = () => this.requestStop('recorded_track_ended');
      this.onTrackMute = () => this.requestStop('recorded_track_muted');
      this.onVisibilityChange = () => {
        this.append('visibility', { state: document.visibilityState });
        if (document.hidden) {
          this.requestStop('document_hidden');
        }
      };
    }

    append(kind, payload = {}) {
      if (
        !this.running
        && !this.stopping
        && kind !== 'run_start'
        && kind !== 'run_stop'
      ) {
        return;
      }
      const performanceMs = performance.now();
      this.records.push({
        ...payload,
        schema: CAPTURE_SCHEMA,
        run_id: this.runId,
        kind,
        epoch_ms: performance.timeOrigin + performanceMs,
        performance_ms: performanceMs,
        run_elapsed_ms: Math.max(0, performanceMs - this.startedAtPerformanceMs),
      });
      if (Object.hasOwn(this.counts, kind)) {
        this.counts[kind] += 1;
      }
    }

    getCurrentVideoTrack() {
      const source = this.video.srcObject;
      if (!source || typeof source.getVideoTracks !== 'function') {
        return null;
      }
      return source.getVideoTracks()
        .find((track) => track.readyState === 'live') || null;
    }

    getVideoStream() {
      const track = this.getCurrentVideoTrack();
      if (!track) {
        throw new Error('remote video track is not live');
      }
      this.recordedTrack = track;
      return new MediaStream([track]);
    }

    trackStillMatches() {
      return (
        this.recordedTrack !== null
        && this.recordedTrack.readyState === 'live'
        && this.getCurrentVideoTrack() === this.recordedTrack
      );
    }

    startFrameMetadata() {
      if (!('requestVideoFrameCallback' in this.video)) {
        throw new Error('requestVideoFrameCallback is not supported');
      }
      const onFrame = (now, metadata) => {
        if (!this.running) {
          return;
        }
        if (!this.trackStillMatches()) {
          this.requestStop('video_track_replaced');
          return;
        }
        this.append('frame', {
          callback_now_ms: finiteOrNull(now),
          presentation_time_ms: finiteOrNull(metadata.presentationTime),
          expected_display_time_ms: finiteOrNull(metadata.expectedDisplayTime),
          media_time_s: finiteOrNull(metadata.mediaTime),
          width: finiteOrNull(metadata.width),
          height: finiteOrNull(metadata.height),
          presented_frames: finiteOrNull(metadata.presentedFrames),
          processing_duration_s: finiteOrNull(metadata.processingDuration),
          capture_time_ms: finiteOrNull(metadata.captureTime),
          receive_time_ms: finiteOrNull(metadata.receiveTime),
          rtp_timestamp: finiteOrNull(metadata.rtpTimestamp),
          recorded_track_id: this.recordedTrack?.id || null,
        });
        this.videoFrameCallbackId = this.video.requestVideoFrameCallback(onFrame);
      };
      this.videoFrameCallbackId = this.video.requestVideoFrameCallback(onFrame);
    }

    startDiagnostics() {
      this.diagnosticTimer = window.setInterval(() => {
        if (!this.trackStillMatches()) {
          this.requestStop('video_track_replaced');
          return;
        }
        const diagnostics = window.fpvViewer?.getDiagnostics?.() || {};
        this.append('diagnostic', {
          reconnect_count: diagnostics.reconnectCount ?? null,
          last_reconnect_at_ms: diagnostics.lastReconnectAt ?? null,
          last_reconnect_reason: diagnostics.lastReconnectReason ?? null,
          last_ws_close: diagnostics.lastWsClose ?? null,
          event_counters: diagnostics.eventCounters || null,
          web_rtc_stats: diagnostics.webRtcStats || null,
        });
      }, DIAGNOSTIC_INTERVAL_MS);
      this.durationTimer = window.setTimeout(() => {
        this.requestStop('maximum_duration');
      }, MAX_CAPTURE_DURATION_MS);
    }

    addEventListeners() {
      window.addEventListener(EVENT_TELEMETRY, this.onTelemetry);
      window.addEventListener(EVENT_COMMAND, this.onCommand);
      window.addEventListener(EVENT_DRIVE, this.onDrive);
      document.addEventListener('visibilitychange', this.onVisibilityChange);
      this.recordedTrack?.addEventListener?.('ended', this.onTrackEnded);
      this.recordedTrack?.addEventListener?.('mute', this.onTrackMute);
    }

    removeEventListeners() {
      window.removeEventListener(EVENT_TELEMETRY, this.onTelemetry);
      window.removeEventListener(EVENT_COMMAND, this.onCommand);
      window.removeEventListener(EVENT_DRIVE, this.onDrive);
      document.removeEventListener('visibilitychange', this.onVisibilityChange);
      this.recordedTrack?.removeEventListener?.('ended', this.onTrackEnded);
      this.recordedTrack?.removeEventListener?.('mute', this.onTrackMute);
    }

    clearTimersAndCallbacks() {
      if (this.diagnosticTimer !== null) {
        window.clearInterval(this.diagnosticTimer);
        this.diagnosticTimer = null;
      }
      if (this.durationTimer !== null) {
        window.clearTimeout(this.durationTimer);
        this.durationTimer = null;
      }
      if (
        this.videoFrameCallbackId !== null
        && 'cancelVideoFrameCallback' in this.video
      ) {
        this.video.cancelVideoFrameCallback(this.videoFrameCallbackId);
      }
      this.videoFrameCallbackId = null;
    }

    installRecorderListeners() {
      this.recorderStopSeen = false;
      this.recorderStopPromise = new Promise((resolve) => {
        this.resolveRecorderStop = resolve;
      });
      this.mediaRecorder.addEventListener('start', () => {
        this.append('recorder_start', {
          recorder_state: this.mediaRecorder?.state || null,
        });
      });
      this.mediaRecorder.addEventListener('dataavailable', (event) => {
        if (event.data && event.data.size > 0) {
          this.videoChunks.push(event.data);
          this.append('recorder_chunk', {
            chunk_index: this.chunkIndex,
            chunk_bytes: event.data.size,
            blob_timecode_ms: finiteOrNull(event.timecode),
          });
          this.chunkIndex += 1;
        }
      });
      this.mediaRecorder.addEventListener('error', (event) => {
        this.append('recorder_error', {
          error: errorText(event.error),
        });
        this.requestStop('media_recorder_error');
      });
      this.mediaRecorder.addEventListener('stop', () => {
        this.recorderStopSeen = true;
        this.resolveRecorderStop?.();
        if (this.running) {
          this.requestStop('media_recorder_stopped');
        }
      });
    }

    async stopRecorder() {
      if (!this.mediaRecorder) {
        return;
      }
      if (this.mediaRecorder.state !== 'inactive') {
        try {
          this.mediaRecorder.stop();
        } catch (error) {
          this.append('recorder_stop_error', {
            error: errorText(error),
          });
        }
      }
      if (this.recorderStopSeen) {
        return;
      }
      await new Promise((resolve) => {
        const timeoutId = window.setTimeout(() => {
          this.append('recorder_stop_timeout', {
            recorder_state: this.mediaRecorder?.state || null,
          });
          resolve();
        }, 3000);
        this.recorderStopPromise?.then(() => {
          window.clearTimeout(timeoutId);
          resolve();
        });
      });
    }

    async start() {
      if (this.running || this.stopping) {
        return this.getStatus();
      }
      if (!window.MediaRecorder) {
        throw new Error('MediaRecorder is not supported');
      }
      const mimeType = chooseVideoMimeType();
      if (!mimeType) {
        throw new Error('no supported WebM video recording format');
      }
      if (this.lastArtifacts && !this.lastArtifactsDownloadRequested) {
        throw new Error('download the previous capture before starting another');
      }
      const videoStream = this.getVideoStream();
      this.runId = createRunId();
      this.startedAtPerformanceMs = performance.now();
      this.startedAtEpochMs = performance.timeOrigin + this.startedAtPerformanceMs;
      this.records = [];
      this.videoChunks = [];
      this.chunkIndex = 0;
      this.pendingStop = null;
      this.lastArtifacts = null;
      this.lastArtifactsDownloadRequested = false;
      for (const key of Object.keys(this.counts)) {
        this.counts[key] = 0;
      }

      this.mediaRecorder = new MediaRecorder(videoStream, { mimeType });
      this.installRecorderListeners();
      this.running = true;
      const viewerSnapshot = window.fpvViewer?.getCaptureSnapshot?.() || null;
      this.append('run_start', {
        mode: 'capture-only',
        transmit_capability: false,
        media_recorder_mime_type: this.mediaRecorder.mimeType,
        video_width: this.video.videoWidth,
        video_height: this.video.videoHeight,
        css_flip: document.body.classList.contains('flip-video'),
        css_mirror: document.body.classList.contains('mirror-video'),
        recorded_track: snapshotTrack(this.recordedTrack),
        viewer: viewerSnapshot,
        maximum_duration_ms: MAX_CAPTURE_DURATION_MS,
        note: 'recorded pixels are the raw video track; orientation is metadata',
      });
      try {
        this.addEventListeners();
        this.startFrameMetadata();
        this.mediaRecorder.start(1000);
        this.append('recorder_start_call', {
          timeslice_ms: 1000,
          recorder_state: this.mediaRecorder.state,
        });
        this.startDiagnostics();
      } catch (error) {
        await this.abortStart();
        throw error;
      }
      return this.getStatus();
    }

    async abortStart() {
      this.running = false;
      this.stopping = true;
      this.removeEventListeners();
      this.clearTimersAndCallbacks();
      try {
        await this.stopRecorder();
      } finally {
        this.mediaRecorder = null;
        this.recorderStopPromise = null;
        this.resolveRecorderStop = null;
        this.recordedTrack = null;
        this.videoChunks = [];
        this.records = [];
        this.stopping = false;
      }
    }

    requestStop(reason) {
      if (this.pendingStop) {
        return this.pendingStop;
      }
      if (!this.running) {
        return Promise.resolve(this.getStatus());
      }
      this.pendingStop = this.stop({ reason })
        .catch((error) => {
          console.error('CPU shadow capture stop failed:', error);
          throw error;
        })
        .finally(() => {
          this.pendingStop = null;
        });
      return this.pendingStop;
    }

    async stop({ reason = 'manual' } = {}) {
      if (!this.running) {
        return this.lastArtifacts || this.getStatus();
      }
      this.running = false;
      this.stopping = true;
      this.removeEventListeners();
      this.clearTimersAndCallbacks();
      try {
        await this.stopRecorder();
        const stoppedAtPerformanceMs = performance.now();
        this.append('run_stop', {
          reason,
          recorder_stop_seen: this.recorderStopSeen,
        });
        const videoBlob = new Blob(this.videoChunks, {
          type: this.mediaRecorder?.mimeType || 'video/webm',
        });
        const jsonl = `${this.records.map((record) => JSON.stringify(record)).join('\n')}\n`;
        const summary = {
          schema: CAPTURE_SCHEMA,
          run_id: this.runId,
          mode: 'capture-only',
          transmit_capability: false,
          stop_reason: reason,
          complete: this.recorderStopSeen,
          recorder_stop_seen: this.recorderStopSeen,
          started_at_epoch_ms: this.startedAtEpochMs,
          duration_ms: Math.max(0, stoppedAtPerformanceMs - this.startedAtPerformanceMs),
          media_recorder_mime_type: videoBlob.type,
          video_bytes: videoBlob.size,
          records: this.records.length,
          counts: { ...this.counts },
          recorded_track: snapshotTrack(this.recordedTrack),
          limitations: [
            'MediaRecorder frames and presentation callbacks are not one-to-one',
            'MediaRecorder container timestamps are not a camera clock',
            'captureTime and receiveTime are browser best-effort fields',
            'command and drive send records prove only local RTCDataChannel.send acceptance, not remote vehicle application',
            'CSS flip and mirror are applied later by replay, not to recorded pixels',
            'artifacts are held in memory and capture is capped at 15 minutes',
            'ESC return telemetry is not present in the current vehicle',
          ],
        };
        const artifacts = {
          runId: this.runId,
          videoBlob,
          logBlob: new Blob([jsonl], { type: 'application/x-ndjson' }),
          summaryBlob: new Blob(
            [`${JSON.stringify(summary, null, 2)}\n`],
            { type: 'application/json' },
          ),
          summary,
        };
        this.lastArtifacts = artifacts;
        this.lastArtifactsDownloadRequested = false;
        window.dispatchEvent(new CustomEvent(EVENT_STOPPED, {
          detail: { summary },
        }));
        return artifacts;
      } finally {
        this.mediaRecorder = null;
        this.recorderStopPromise = null;
        this.resolveRecorderStop = null;
        this.recordedTrack = null;
        this.videoChunks = [];
        this.stopping = false;
      }
    }

    downloadLastArtifacts() {
      if (!this.lastArtifacts) {
        throw new Error('no completed capture is available');
      }
      downloadBlob(
        this.lastArtifacts.videoBlob,
        `${this.lastArtifacts.runId}.webm`,
      );
      downloadBlob(
        this.lastArtifacts.logBlob,
        `${this.lastArtifacts.runId}.jsonl`,
      );
      downloadBlob(
        this.lastArtifacts.summaryBlob,
        `${this.lastArtifacts.runId}-summary.json`,
      );
      this.lastArtifactsDownloadRequested = true;
      return this.lastArtifacts.summary;
    }

    getStatus() {
      return {
        schema: CAPTURE_SCHEMA,
        running: this.running,
        stopping: this.stopping,
        runId: this.runId || null,
        counts: { ...this.counts },
        records: this.records.length,
        hasArtifacts: this.lastArtifacts !== null,
        downloadRequested: this.lastArtifactsDownloadRequested,
      };
    }
  }

  function installOptInUi(capture) {
    if (document.getElementById(PANEL_ID)) {
      return;
    }
    const style = document.createElement('style');
    style.textContent = `
      #${PANEL_ID} {
        display: inline-flex;
        gap: 8px;
        align-items: center;
        padding: 4px 6px;
        border: 1px solid rgba(255, 255, 255, 0.35);
        border-radius: 8px;
        background: rgba(0, 0, 0, 0.82);
        color: #fff;
        font: 12px/1.2 system-ui, sans-serif;
      }
      #${PANEL_ID}.cpu-capture-floating {
        position: fixed;
        top: 56px;
        right: 12px;
        z-index: 10000;
      }
      #${PANEL_ID} button {
        min-height: 30px;
      }
    `;
    document.head.appendChild(style);

    const panel = document.createElement('span');
    panel.id = PANEL_ID;
    const button = document.createElement('button');
    button.type = 'button';
    button.textContent = 'Start CPU Capture';
    const status = document.createElement('span');
    status.textContent = 'capture-only / no command output';
    panel.append(button, status);
    const controlHost = document.querySelector('.top-controls');
    if (controlHost) {
      controlHost.appendChild(panel);
    } else {
      panel.classList.add('cpu-capture-floating');
      document.body.appendChild(panel);
    }

    const renderStopped = (summary) => {
      const completion = summary.complete ? 'ready' : 'INCOMPLETE';
      status.textContent = (
        `${summary.records} records ${completion} (${summary.stop_reason})`
      );
      button.textContent = summary.complete
        ? 'Download Capture'
        : 'Download Incomplete Capture';
      button.disabled = false;
    };
    window.addEventListener(EVENT_STOPPED, (event) => {
      if (event.detail?.summary) {
        renderStopped(event.detail.summary);
      }
    });

    button.addEventListener('click', async () => {
      button.disabled = true;
      try {
        if (capture.running) {
          const artifacts = await capture.stop({ reason: 'manual' });
          renderStopped(artifacts.summary);
        } else if (
          capture.lastArtifacts
          && !capture.lastArtifactsDownloadRequested
        ) {
          capture.downloadLastArtifacts();
          status.textContent = '3 downloads requested; verify all files';
          button.textContent = 'Start CPU Capture';
          button.disabled = false;
        } else {
          await capture.start();
          status.textContent = `${capture.runId} recording`;
          button.textContent = 'Stop Capture';
          button.disabled = false;
        }
      } catch (error) {
        status.textContent = errorText(error);
        button.disabled = false;
      }
    });
  }

  const video = document.getElementById('remote_video');
  if (!video) {
    console.warn('CPU shadow capture: #remote_video is missing');
    return;
  }
  const capture = new CpuShadowCapture(video);
  window.fpvCpuShadowCapture = capture;
  if (isEnabled(getUrlParams().get(UI_FLAG))) {
    installOptInUi(capture);
  }
})();
