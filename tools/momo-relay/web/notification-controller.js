(function initNotificationControllerModule(root, factory) {
  const api = factory();
  if (typeof module === 'object' && module.exports) {
    module.exports = api;
    return;
  }
  root.MomoNotificationController = api;
}(typeof globalThis !== 'undefined' ? globalThis : this, () => {
  'use strict';

  const PRIORITIES = Object.freeze({
    GAMEPLAY: 100,
    LAP: 200,
    PIT: 300,
    GOAL: 400,
    BLUE_FLAG: 500,
    SAFETY: 600,
    STOP: 700,
  });

  function createNotificationController(options = {}) {
    const now = typeof options.now === 'function' ? options.now : () => Date.now();
    const schedule = typeof options.setTimeout === 'function'
      ? options.setTimeout
      : (callback, delayMs) => setTimeout(callback, delayMs);
    const cancel = typeof options.clearTimeout === 'function'
      ? options.clearTimeout
      : (timer) => clearTimeout(timer);
    const onChange = typeof options.onChange === 'function' ? options.onChange : () => {};
    const notifications = new Map();
    let activeId = '';
    let activeTimer = null;
    let sequence = 0;

    function snapshot(notification) {
      if (!notification) return null;
      return Object.freeze({
        id: notification.id,
        group: notification.group,
        priority: notification.priority,
        persistent: notification.persistent,
        durationMs: notification.durationMs,
        remainingMs: notification.persistent ? null : notification.remainingMs,
        payload: notification.payload,
      });
    }

    function sortedNotifications() {
      return Array.from(notifications.values()).sort((left, right) => (
        right.priority - left.priority || left.order - right.order
      ));
    }

    function stopActiveTimer(pause) {
      if (activeTimer !== null) {
        cancel(activeTimer);
        activeTimer = null;
      }
      const active = notifications.get(activeId);
      if (pause && active && !active.persistent && active.expiresAt !== null) {
        active.remainingMs = Math.max(0, active.expiresAt - now());
      }
      if (active) active.expiresAt = null;
    }

    function scheduleActiveTimer() {
      const active = notifications.get(activeId);
      if (!active || active.persistent) return;
      active.expiresAt = now() + active.remainingMs;
      activeTimer = schedule(() => {
        const expiredId = activeId;
        activeTimer = null;
        activeId = '';
        notifications.delete(expiredId);
        reconcile(snapshot(active));
      }, active.remainingMs);
    }

    function reconcile(previous = null, forceChange = false) {
      const next = sortedNotifications()[0] || null;
      if (next?.id === activeId) {
        if (forceChange) onChange(snapshot(next), previous || snapshot(next));
        return;
      }

      const previousActive = notifications.get(activeId);
      const previousSnapshot = previous || snapshot(previousActive);
      stopActiveTimer(Boolean(previousActive));
      activeId = next?.id || '';
      scheduleActiveTimer();
      onChange(snapshot(next), previousSnapshot);
    }

    function publish(input) {
      const id = typeof input?.id === 'string' ? input.id.trim() : '';
      const priority = Number(input?.priority);
      if (!id || !Number.isFinite(priority)) {
        throw new TypeError('Notification id and finite priority are required.');
      }
      const persistent = input.persistent === true;
      const durationMs = persistent ? null : Number(input.durationMs);
      if (!persistent && (!Number.isFinite(durationMs) || durationMs <= 0)) {
        throw new TypeError('Transient notifications require a positive durationMs.');
      }
      const group = typeof input.group === 'string' && input.group.trim()
        ? input.group.trim()
        : id;
      const previousActive = snapshot(notifications.get(activeId));
      const replacingActive = activeId === id;

      if (input.replaceGroup === true) {
        for (const [candidateId, notification] of notifications) {
          if (candidateId !== id && notification.group === group) {
            if (candidateId === activeId) stopActiveTimer(false);
            notifications.delete(candidateId);
            if (candidateId === activeId) activeId = '';
          }
        }
      }

      const existing = notifications.get(id);
      const notification = {
        id,
        group,
        priority,
        persistent,
        durationMs,
        remainingMs: persistent ? null : durationMs,
        expiresAt: null,
        payload: input.payload,
        order: existing?.order ?? sequence++,
      };
      if (replacingActive) stopActiveTimer(false);
      notifications.set(id, notification);
      reconcile(previousActive, replacingActive);
      if (replacingActive && activeId === id) {
        scheduleActiveTimer();
      }
      return snapshot(notification);
    }

    function clear(id) {
      if (typeof id !== 'string' || !notifications.has(id)) return false;
      const previous = snapshot(notifications.get(activeId));
      if (id === activeId) {
        stopActiveTimer(false);
        activeId = '';
      }
      notifications.delete(id);
      reconcile(previous);
      return true;
    }

    function clearGroup(group) {
      if (typeof group !== 'string' || !group) return 0;
      const ids = Array.from(notifications.values())
        .filter((notification) => notification.group === group)
        .map((notification) => notification.id);
      if (ids.length === 0) return 0;
      const previous = snapshot(notifications.get(activeId));
      if (ids.includes(activeId)) {
        stopActiveTimer(false);
        activeId = '';
      }
      ids.forEach((id) => notifications.delete(id));
      reconcile(previous);
      return ids.length;
    }

    function clearAll() {
      const previous = snapshot(notifications.get(activeId));
      stopActiveTimer(false);
      notifications.clear();
      activeId = '';
      if (previous) onChange(null, previous);
    }

    function getState() {
      const ordered = sortedNotifications();
      return Object.freeze({
        active: snapshot(notifications.get(activeId)),
        queued: Object.freeze(ordered
          .filter((notification) => notification.id !== activeId)
          .map(snapshot)),
      });
    }

    return Object.freeze({
      publish,
      clear,
      clearGroup,
      clearAll,
      getState,
      isActive: (id) => id === activeId,
    });
  }

  return Object.freeze({ PRIORITIES, createNotificationController });
}));
