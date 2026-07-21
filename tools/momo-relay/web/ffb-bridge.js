(() => {
  'use strict';

  const DEFAULT_URL = 'ws://127.0.0.1:24725';

  function parseJson(value) {
    try {
      return JSON.parse(value);
    } catch {
      return null;
    }
  }

  class FfbBridgeClient {
    constructor(options = {}) {
      this.url = options.url || DEFAULT_URL;
      this.onState = typeof options.onState === 'function' ? options.onState : () => {};
      this.ws = null;
      this.connected = false;
      this.connecting = false;
      this.acquired = false;
      this.devices = [];
      this.selectedDeviceId = '';
      this.lastError = '';
      this.lastStatus = null;
      this.heartbeatId = 0;
    }

    snapshot() {
      return {
        url: this.url,
        connected: this.connected,
        connecting: this.connecting,
        acquired: this.acquired,
        devices: this.devices,
        selectedDeviceId: this.selectedDeviceId,
        lastError: this.lastError,
        lastStatus: this.lastStatus,
      };
    }

    emitState() {
      this.onState(this.snapshot());
    }

    connect(url = this.url) {
      this.url = url || DEFAULT_URL;
      if (this.connected || this.connecting) return;
      this.connecting = true;
      this.lastError = '';
      this.emitState();
      const ws = new WebSocket(this.url);
      this.ws = ws;
      ws.addEventListener('open', () => {
        this.connected = true;
        this.connecting = false;
        this.send({ type: 'hello', client: 'momo-fpv-viewer', protocol: 1 });
        this.listDevices();
        this.startHeartbeat();
        this.emitState();
      });
      ws.addEventListener('message', (event) => this.handleMessage(parseJson(event.data)));
      ws.addEventListener('close', () => {
        this.stopHeartbeat();
        if (this.ws === ws) this.ws = null;
        this.connected = false;
        this.connecting = false;
        this.acquired = false;
        this.emitState();
      });
      ws.addEventListener('error', () => {
        this.lastError = 'Bridge WebSocket error.';
        this.emitState();
      });
    }

    handleMessage(message) {
      if (!message || typeof message.type !== 'string') return;
      if (message.type === 'deviceList') {
        this.devices = Array.isArray(message.devices) ? message.devices : [];
      } else if (message.type === 'acquired') {
        this.acquired = !!message.ok;
        if (message.ok) this.selectedDeviceId = String(message.deviceId || this.selectedDeviceId);
      } else if (message.type === 'ffbStatus') {
        this.lastStatus = message;
      } else if (message.type === 'error') {
        this.lastError = `${message.code || 'ERROR'}: ${message.message || ''}`.trim();
      }
      this.emitState();
    }

    send(message) {
      if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return false;
      this.ws.send(JSON.stringify(message));
      return true;
    }

    listDevices() { return this.send({ type: 'listDevices' }); }

    acquire(deviceId) {
      this.selectedDeviceId = String(deviceId || '');
      return this.send({ type: 'acquireDevice', deviceId: this.selectedDeviceId, preferExclusive: true });
    }

    sendFfb(command) {
      if (!this.connected || !this.acquired) return false;
      return this.send({ type: 'setFfb', ...command });
    }

    stopAll() { return this.send({ type: 'stopAll' }); }

    disconnect() {
      this.stopAll();
      this.stopHeartbeat();
      try { this.ws?.close(); } catch {}
      this.ws = null;
      this.connected = false;
      this.connecting = false;
      this.acquired = false;
      this.emitState();
    }

    startHeartbeat() {
      this.stopHeartbeat();
      this.heartbeatId = window.setInterval(() => this.send({ type: 'ping', timeMs: Date.now() }), 1000);
    }

    stopHeartbeat() {
      if (!this.heartbeatId) return;
      window.clearInterval(this.heartbeatId);
      this.heartbeatId = 0;
    }
  }

  window.FpvFfbBridge = { FfbBridgeClient, DEFAULT_URL };
})();
