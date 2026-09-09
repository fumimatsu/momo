// Shared by LAN TeamObserver and Cloud spectator. Transports own subscriptions;
// this component owns only the selection interaction and accessible controls.
export const TEAM_VIDEO_LIMIT = 4;

export function validateVideoSelection(ids, availableIds, limit = TEAM_VIDEO_LIMIT) {
  if (!Array.isArray(ids) || ids.length > limit || new Set(ids).size !== ids.length
      || ids.some(id => !availableIds.includes(id))) throw Error('invalid_video_selection');
  return [...ids];
}

const node = (tag, className = '', text = '') => {
  const n = document.createElement(tag); n.className = className; n.textContent = text; return n;
};
const accent = (n, car) => { n.style.setProperty('--car', car.color); return n; };
const button = (className, text, action, label) => {
  const n = node('button', className, text); n.type = 'button';
  if (label) n.setAttribute('aria-label', label);
  n.addEventListener('click', action); return n;
};

export class ObserverSelection {
  constructor({ onChange, onFocus, root = document }) {
    this.root = root; this.onChange = onChange; this.onFocus = onFocus;
    this.cars = []; this.ids = []; this.pending = ''; this.signature = '';
    const host = root.getElementById('observerSelectionDrawer');
    host.innerHTML = `<div id="teamSelectorBackdrop" class="team-selector-backdrop" hidden>
      <aside id="teamSelectorDrawer" class="team-selector-drawer" role="dialog" aria-modal="true" aria-labelledby="teamSelectorTitle">
        <header class="team-selector-head"><div><span class="eyebrow">MAXIMUM 4 ONBOARD FEEDS</span>
          <h2 id="teamSelectorTitle">SELECT TEAM CARS</h2></div>
          <button id="teamSelectorClose" class="team-selector-close" type="button" aria-label="Close team car selector">×</button></header>
        <section id="teamReplacementPanel" class="team-replacement-panel" hidden></section>
        <div id="teamSelectorList" class="team-selector-list"></div>
        <footer class="team-selector-footer"><span id="teamSelectionCount"></span>
          <button id="teamSelectionClear" type="button">CLEAR ALL</button></footer>
      </aside></div>`;
    this.get('teamSelectorOpen').addEventListener('click', () => this.open());
    this.get('teamSelectorClose').addEventListener('click', () => this.close());
    this.get('teamSelectionClear').addEventListener('click', () => this.change([]));
    this.get('teamSelectorBackdrop').addEventListener('click', e => { if (e.target === e.currentTarget) this.close(); });
    root.addEventListener('keydown', e => {
      if (this.get('teamSelectorBackdrop').hidden) return;
      if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); this.close(); }
      if (e.key === 'Tab') {
        const controls = [...this.get('teamSelectorDrawer').querySelectorAll('button')].filter(n => n.getClientRects().length && !n.disabled);
        const first = controls[0], last = controls.at(-1);
        if (e.shiftKey && (root.activeElement === first || !controls.includes(root.activeElement))) { e.preventDefault(); last.focus(); }
        else if (!e.shiftKey && (root.activeElement === last || !controls.includes(root.activeElement))) { e.preventDefault(); first.focus(); }
      }
    });
  }
  get(id) { return this.root.getElementById(id); }
  open() { this.get('teamSelectorBackdrop').hidden = false; this.get('teamSelectorClose').focus(); }
  close() { this.pending = ''; this.render(); this.get('teamSelectorBackdrop').hidden = true; this.get('teamSelectorOpen').focus(); }
  update(cars, ids, note = '') {
    this.ids = validateVideoSelection(ids, cars.map(c => c.id));
    this.cars = cars; this.note = note;
    if (!cars.some(c => c.id === this.pending) || ids.includes(this.pending)) this.pending = '';
    this.render();
  }
  change(ids) {
    this.pending = '';
    this.onChange(validateVideoSelection(ids, this.cars.map(c => c.id)));
  }
  request(id) {
    if (!this.cars.some(c => c.id === id)) return;
    if (this.ids.includes(id)) { this.onFocus(id); return; }
    if (this.ids.length < TEAM_VIDEO_LIMIT) this.change([...this.ids, id]);
    else { this.pending = id; this.render(); this.open(); }
  }
  render() {
    const signature = JSON.stringify([this.cars, this.ids, this.pending, this.note]);
    if (this.signature === signature) return;
    this.signature = signature;
    const active = this.root.activeElement;
    const focusKey = active?.dataset.selectionKey;
    const selectedCars = this.ids.map(id => this.cars.find(c => c.id === id));
    this.get('teamSelectionSlots').replaceChildren(...Array.from({ length: TEAM_VIDEO_LIMIT }, (_, i) => {
      const car = selectedCars[i], slot = node('div', 'team-selection-slot' + (car ? ' is-filled' : ''));
      if (!car) { slot.append(button('team-slot-main', `SLOT ${i + 1}`, () => this.open(), `Select a car for slot ${i + 1}`)); return slot; }
      accent(slot, car);
      const main = button('team-slot-main', '', () => this.onFocus(car.id));
      main.append(node('strong', '', car.number), node('span', '', car.name));
      const remove = button('team-slot-remove', '×', () => this.change(this.ids.filter(id => id !== car.id)), `Remove ${car.number} from team monitor`);
      remove.dataset.selectionKey = 'remove-' + car.id;
      slot.append(main, remove); return slot;
    }));
    this.get('teamSelectorList').replaceChildren(...this.cars.map(car => {
      const selected = this.ids.includes(car.id);
      const row = accent(button('team-selector-row' + (selected ? ' is-selected' : ''), '', () => {
        if (this.ids.includes(car.id)) this.change(this.ids.filter(id => id !== car.id)); else this.request(car.id);
      }), car);
      row.dataset.vehicleId = car.id; row.dataset.selectionKey = car.id; row.setAttribute('aria-pressed', String(selected));
      const identity = node('span', 'team-selector-identity');
      identity.append(node('strong', '', car.number), node('span', '', car.name));
      row.append(identity, node('span', 'team-selector-device', car.detail || ''), node('em', '', car.status || (selected ? 'SELECTED' : 'AVAILABLE')));
      return row;
    }));
    this.get('teamSelectionCount').textContent = `${this.ids.length} / ${TEAM_VIDEO_LIMIT} SELECTED · ${this.cars.length} CARS${this.note ? ' · ' + this.note : ''}`;
    const panel = this.get('teamReplacementPanel'), candidate = this.cars.find(c => c.id === this.pending);
    panel.hidden = !candidate; panel.replaceChildren();
    if (candidate) {
      panel.append(node('strong', '', `REPLACE WITH ${candidate.number}`));
      const choices = node('div', 'team-replacement-choices');
      selectedCars.forEach((car, i) => choices.append(accent(button('car-accent', `SLOT ${i + 1} / ${car.number}`, () => {
        const next = [...this.ids]; next[i] = candidate.id; this.change(next);
      }), car)));
      choices.append(button('team-replacement-cancel', 'CANCEL', () => { this.pending = ''; this.render(); }));
      panel.append(choices);
    }
    // Updating connection labels must not steal keyboard focus from a changing list.
    if (focusKey) {
      const next = [...this.root.querySelectorAll('[data-selection-key]')].find(n => n.dataset.selectionKey === focusKey);
      (next || this.get('teamSelectorOpen')).focus({ preventScroll: true });
    }
  }
}
