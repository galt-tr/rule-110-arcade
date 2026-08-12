// Rule 110 space-time diagram.
//
// Generations run down the page, cells across. A live cell is drawn in the
// colour of ITS OWN transaction's progress, so confirmation visibly washes down
// the pattern behind the leading edge — the automaton and the chain are the
// same picture.
//
// The renderer is incremental, and it has to be. A push carries a 48-generation
// tail roughly ten times a second, and the diagram holds up to 2048 generations
// of 128 cells; repainting all of it per push is a quarter of a million fillRect
// calls per frame, about 2.6M a second, for a few dozen cells that actually
// changed. So the canvas is treated as a durable surface: rows are painted once,
// scrolled with the bitmap when the window slides, and repainted only when their
// generation is genuinely replaced.

// Read the palette from CSS so the stylesheet stays the single source of
// truth, but fall back to literals: an empty custom property is silently
// ignored by fillStyle, which would leave cells drawn in whatever colour was
// set last rather than failing visibly.
const CSS = getComputedStyle(document.documentElement);
function color(name, fallback) {
  return CSS.getPropertyValue(name).trim() || fallback;
}
const COLOR = {
  dead:       color('--dead', '#161b26'),
  pending:    color('--pending', '#2d3550'),
  broadcast:  color('--broadcast', '#3f6fa8'),
  seen:       color('--seen', '#62a8e8'),
  mined:      color('--mined', '#b9e0ff'),
  failed:     color('--failed', '#e0575b'),
  failedDead: color('--failed-dead', '#5c2226'),
};

/** How many generations the client keeps. Matches the server's own bound.
 *
 * The client used to keep everything it was ever sent, which is a slow fuse:
 * canvas height is generations x cellPx, browsers refuse a dimension over about
 * 32767px, and past that the diagram does not degrade — it goes blank. */
const MAX_HISTORY = 2048;

/** Trimming reindexes, so trim in chunks rather than once per generation. */
const TRIM_SLACK = 64;

/** Ceiling on either canvas dimension, under the browsers' ~32767px limit.
 *
 * At 16px cells 2048 generations would be 32768px — one pixel over, and blank.
 * Rather than cap the history and lose it, cap what is RENDERED: zoomed all the
 * way in you see fewer rows, and zooming out brings them back. */
const MAX_CANVAS_PX = 32000;

const canvas = document.getElementById('grid');
const ctx = canvas.getContext('2d');
const tip = document.getElementById('tip');
const wrap = document.getElementById('canvas-wrap');

let cellPx = 8;       // driven by the zoom control, not the viewport
let follow = true;    // auto-scroll only while pinned to the newest row

/** Everything the client holds, accumulated across pushes.
 *
 * `snap` is the newest scalars; `history` is every generation we have been sent,
 * ascending; `index` maps a generation number to its position so a tail merges
 * without rebuilding and re-sorting the whole array each time. */
const state = {
  snap: null,
  history: [],
  index: new Map(),
};

/** What is currently ON the canvas bitmap, which is not the same thing as what
 *  we hold.
 *
 * Two checks, cheapest first, because they catch different things. `drawn` maps
 * a generation number to the exact object painted for it: a push leaves every
 * generation older than its tail untouched, so an identity test rules those out
 * in one comparison each. `sig` maps a generation number to what was actually
 * painted, which catches the rest — the tail is re-sent in full ten times a
 * second and almost all of it is unchanged, arriving as fresh objects that carry
 * identical pixels. */
const paint = {
  base: null,   // generation number drawn at row 0
  rows: 0,
  cellPx: null,
  cells: null,
  drawn: new Map(),
  sig: new Map(),
};

/** Everything about a generation that reaches the canvas: the row's bits, and
 *  each cell's transaction state. `err` is deliberately absent — it shows in the
 *  tooltip, never in a pixel. */
function signatureOf(gen, cells) {
  const genCells = gen.cells || [];
  // States are pending/broadcast/seen/mined/failed, distinct in their first
  // character, so one char per cell is a faithful signature.
  let sig = (gen.row || '') + '|';
  for (let i = 0; i < cells; i++) {
    const cell = genCells[i];
    sig += cell && cell.state ? cell.state[0] : 'p';
  }
  return sig;
}

/** The generations actually rendered, newest last. Hit-testing reads this. */
let view = [];

/** Unpack a hex row into a bit array, cell 0 first (matches the contract).
 *
 * Cached on the generation, which is safe precisely because the cache is
 * invalidated the only way it can go stale: a generation whose row changed
 * arrives as a new object. */
function bitsOf(gen, cells) {
  if (gen._bits && gen._bits.length === cells) return gen._bits;
  const hex = gen.row || '';
  const bits = new Uint8Array(cells);
  for (let i = 0; i < cells; i++) {
    const byte = parseInt(hex.substr((i >> 3) * 2, 2), 16);
    bits[i] = (byte >> (i % 8)) & 1;
  }
  gen._bits = bits;
  return bits;
}

/** Merge a pushed snapshot into what we already hold.
 *
 * Updates carry only a recent tail, so replacing the array wholesale would throw
 * away everything the viewer scrolled back to look at. Generations are keyed by
 * their number and the incoming copy wins. */
function ingest(s) {
  state.snap = s;
  const incoming = s.history || [];
  let disordered = false;

  for (const g of incoming) {
    const at = state.index.get(g.number);
    if (at !== undefined) {
      state.history[at] = g;
      continue;
    }
    const last = state.history[state.history.length - 1];
    if (last && g.number < last.number) disordered = true;
    state.index.set(g.number, state.history.length);
    state.history.push(g);
  }

  // A tail is contiguous and ascending, so this is the path that never runs —
  // but a reconnect that replays an older window would otherwise leave the
  // array unsorted, and every row would be drawn at the wrong height.
  if (disordered) {
    state.history.sort((a, b) => a.number - b.number);
    reindex();
  }
  if (state.history.length > MAX_HISTORY + TRIM_SLACK) {
    state.history = state.history.slice(state.history.length - MAX_HISTORY);
    reindex();
  }
}

function reindex() {
  state.index.clear();
  for (let i = 0; i < state.history.length; i++) {
    state.index.set(state.history[i].number, i);
  }
}

/** How many rows fit under the canvas dimension ceiling at this zoom. */
function renderableRows() {
  return Math.max(1, Math.floor(MAX_CANVAS_PX / cellPx));
}

/** Bring the bitmap's geometry in line with the window about to be drawn,
 *  preserving as much already-painted content as possible.
 *
 * Setting canvas.width or .height clears the bitmap even when the value is
 * unchanged, so the old content is copied aside and blitted back at its new
 * offset. One image copy per generation beats repainting every row. */
function reflow(base, rows, cells) {
  const w = cells * cellPx;
  const h = Math.max(1, rows * cellPx);
  const rebuild = paint.base === null || paint.cellPx !== cellPx || paint.cells !== cells;
  const shift = rebuild ? 0 : base - paint.base;
  const resized = canvas.width !== w || canvas.height !== h;

  if (rebuild) {
    canvas.width = w;
    canvas.height = h;
    paint.drawn.clear();
    paint.sig.clear();
    fillBackground();
  } else if (shift !== 0 || resized) {
    let carry = null;
    if (canvas.width > 0 && canvas.height > 0) {
      carry = document.createElement('canvas');
      carry.width = canvas.width;
      carry.height = canvas.height;
      carry.getContext('2d').drawImage(canvas, 0, 0);
    }
    if (resized) {
      canvas.width = w;
      canvas.height = h;
    }
    fillBackground();
    if (carry) ctx.drawImage(carry, 0, -shift * cellPx);
  }

  canvas.style.width = w + 'px';
  canvas.style.height = h + 'px';

  // Rows that scrolled out of the window are no longer on the bitmap, whatever
  // the blit left behind; forget them so they repaint if they come back.
  for (const number of paint.drawn.keys()) {
    if (number < base || number >= base + rows) {
      paint.drawn.delete(number);
      paint.sig.delete(number);
    }
  }

  paint.base = base;
  paint.rows = rows;
  paint.cellPx = cellPx;
  paint.cells = cells;
}

function fillBackground() {
  ctx.fillStyle = COLOR.dead;
  ctx.fillRect(0, 0, canvas.width, canvas.height);
}

/** Paint one generation's row, clearing it first so a changed cell that went
 *  back to background does not leave its old colour behind. */
function paintRow(gen, row, cells) {
  const y = row * cellPx;
  const bits = bitsOf(gen, cells);

  ctx.fillStyle = COLOR.dead;
  ctx.fillRect(0, y, cells * cellPx, cellPx);

  const genCells = gen.cells || [];
  for (let c = 0; c < cells; c++) {
    const cell = genCells[c];
    const cellState = cell ? cell.state : 'pending';
    // A dead cell is background regardless of its transaction — the pattern
    // has to stay readable as a pattern first — with one exception. A failure
    // is a fact about the chain, not about the automaton, and roughly half of
    // all cells are dead, so hiding those failures lets a stalled run look
    // perfectly healthy. Draw them in a dark red instead: visible as a
    // defect, too dim to be mistaken for a live cell.
    if (!bits[c] && cellState !== 'failed') continue;
    ctx.fillStyle = bits[c] ? (COLOR[cellState] || COLOR.pending) : COLOR.failedDead;
    // Cell 0 is drawn rightmost so the row reads like the printed form,
    // highest index leftmost.
    ctx.fillRect((cells - 1 - c) * cellPx, y, cellPx, cellPx);
  }
}

/** Generation numbers beside the rows, thinned so they stay legible when zoomed
 *  out. Rebuilt only when the window or the zoom moves — the labels do not
 *  depend on transaction state, so a push that only changes colours leaves them
 *  alone. */
const gutterState = { base: null, rows: null, cellPx: null };
function drawGutter(rows) {
  const base = rows.length ? rows[0].number : 0;
  if (gutterState.base === base && gutterState.rows === rows.length &&
      gutterState.cellPx === cellPx) {
    return;
  }
  const g = document.getElementById('gutter');
  const every = Math.max(1, Math.ceil(14 / cellPx));
  const lines = rows.map((gen, i) =>
    (i % every === 0 ? String(gen.number).padStart(6) : ''));
  g.style.lineHeight = cellPx + 'px';
  g.style.fontSize = Math.min(10, Math.max(6, cellPx)) + 'px';
  g.textContent = lines.join('\n');
  gutterState.base = base;
  gutterState.rows = rows.length;
  gutterState.cellPx = cellPx;
}

function draw() {
  const s = state.snap;
  if (!s) return;
  document.getElementById('empty').hidden = state.history.length > 0;

  const limit = renderableRows();
  view = state.history.length > limit
    ? state.history.slice(state.history.length - limit)
    : state.history;

  // Decide BEFORE resizing: once the canvas grows, the old scrollTop no longer
  // describes where the viewer was.
  const pinned = follow && atBottom();

  const base = view.length ? view[0].number : 0;
  reflow(base, view.length, s.cells);
  drawGutter(view);

  for (let i = 0; i < view.length; i++) {
    const gen = view[i];
    if (paint.drawn.get(gen.number) === gen) continue;
    const sig = signatureOf(gen, s.cells);
    paint.drawn.set(gen.number, gen);
    if (paint.sig.get(gen.number) === sig) continue; // re-sent, identical pixels
    paintRow(gen, i, s.cells);
    paint.sig.set(gen.number, sig);
  }

  // Only chase the leading edge when the viewer is already there. Scrolling
  // them back down while they are reading history is worse than losing the
  // live edge, which the follow toggle restores in one click.
  if (pinned) wrap.scrollTop = wrap.scrollHeight;
}

/** Within a row of the bottom — treated as "watching the live edge". */
function atBottom() {
  return wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight <= cellPx * 2;
}

/** The stat row, built once and updated in place. Replacing innerHTML ten times
 *  a second reparses markup to change a handful of numbers. */
const statFields = new Map();
function renderStats(s) {
  const rows = [
    ['cells', s.cells],
    ['rule', s.rule],
    ['generation', s.generation],
    ['transactions', s.totalTx.toLocaleString()],
    // "spendable", not "balance": the wallet total is a different and
    // misleading number — see chain.Funds.
    ['spendable', s.balance.toLocaleString() + ' sat'],
    ['fuel', s.poolCoins.toLocaleString() + ' coins'],
    // "proved", never "agree". Each cell's script checks only its own bit of
    // the next row, so this counts transitions the network accepted in the
    // newest generation — it says nothing about the cells agreeing with each
    // other, which no script enforces. See Snapshot.ProvedCells.
    ['latest row', `${s.provedCells}/${s.cells} proved` +
      (s.failedCells ? ` · ${s.failedCells} failed` : '')],
  ];

  const host = document.getElementById('stats');
  if (!statFields.size) {
    for (const [key] of rows) {
      const span = document.createElement('span');
      span.className = 'stat';
      const label = document.createElement('span');
      label.textContent = key;
      const value = document.createElement('b');
      span.append(label, value);
      host.append(span);
      statFields.set(key, value);
    }
  }
  for (const [key, val] of rows) {
    const el = statFields.get(key);
    const text = String(val);
    if (el.textContent !== text) el.textContent = text;
  }

  document.getElementById('play').setAttribute('aria-pressed', s.mode === 'running');
  document.getElementById('pause').setAttribute('aria-pressed', s.mode === 'paused');

  const err = document.getElementById('err');
  err.hidden = !s.lastError;
  if (err.textContent !== (s.lastError || '')) err.textContent = s.lastError || '';
}

/** Render at most once per animation frame.
 *
 * Pushes arrive faster than the display refreshes, and several can land in one
 * frame. Rendering per message does work the viewer cannot see; rendering per
 * frame is the most that can ever be shown. */
let frameQueued = false;
function scheduleRender() {
  if (frameQueued) return;
  frameQueued = true;
  requestAnimationFrame(() => {
    frameQueued = false;
    render();
  });
}

function render() {
  if (!state.snap) return;
  renderStats(state.snap);
  draw();
}

function apply(s) {
  ingest(s);
  scheduleRender();
}

/** Force every row to repaint on the next frame — for changes that invalidate
 *  the whole bitmap rather than any particular row. */
function invalidate() {
  paint.base = null;
  paint.drawn.clear();
  paint.sig.clear();
  gutterState.base = null;
  scheduleRender();
}

async function control(action, extra = {}) {
  const res = await fetch('/api/control', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ action, ...extra }),
  });
  if (res.ok) apply(await res.json());
}

document.getElementById('play').onclick = () => control('play');
document.getElementById('pause').onclick = () => control('pause');
document.getElementById('step').onclick = () => control('step');

const zoom = document.getElementById('zoom');
const zoomOut = document.getElementById('zoomOut');
zoom.value = String(cellPx);
zoomOut.textContent = cellPx + 'px';
zoom.oninput = () => {
  cellPx = +zoom.value;
  zoomOut.textContent = cellPx + 'px';
  // A zoom changes every row's height and position, so nothing on the bitmap
  // survives — but it still goes through the frame queue, because the slider
  // fires far faster than the display refreshes.
  invalidate();
};

const followBox = document.getElementById('follow');
followBox.onchange = () => {
  follow = followBox.checked;
  if (follow) wrap.scrollTop = wrap.scrollHeight;
};

// Scrolling away from the bottom releases follow, so reading history is not a
// fight with the renderer; scrolling back re-arms it.
wrap.addEventListener('scroll', () => {
  if (!followBox.checked) return;
  follow = atBottom();
});

const rate = document.getElementById('rate');
const rateOut = document.getElementById('rateOut');
rate.oninput = () => { rateOut.textContent = (+rate.value).toFixed(2) + ' gen/s'; };
rate.onchange = () => control('rate', { rate: +rate.value });

/** Which cell is under the pointer, or null. */
function cellAt(ev) {
  if (!state.snap || !view.length) return null;
  const r = canvas.getBoundingClientRect();
  const g = Math.floor((ev.clientY - r.top) / cellPx);
  const c = state.snap.cells - 1 - Math.floor((ev.clientX - r.left) / cellPx);
  const gen = view[g];
  if (!gen || c < 0 || c >= state.snap.cells) return null;
  return { gen, cell: (gen.cells && gen.cells[c]) || {}, index: c };
}

/** Arcade's status page for a transaction. */
function arcadeTxURL(txid) {
  return state.snap.arcadeUrl.replace(/\/+$/, '') + '/tx/' + txid;
}

// Click a cell to open its transaction in arcade.
canvas.onclick = (ev) => {
  const hit = cellAt(ev);
  if (!hit || !hit.cell.txid) return;
  window.open(arcadeTxURL(hit.cell.txid), '_blank', 'noopener');
};

// Hover a cell to see which transaction proved it.
canvas.onmousemove = (ev) => {
  const hit = cellAt(ev);
  if (!hit) { tip.hidden = true; canvas.style.cursor = 'default'; return; }
  const { gen, cell } = hit;
  const c = hit.index;
  canvas.style.cursor = cell.txid ? 'pointer' : 'default';
  const bits = bitsOf(gen, state.snap.cells);
  tip.innerHTML =
    `<b>cell ${c}</b> · generation ${gen.number}<br>` +
    `state: ${bits[c] ? 'alive' : 'dead'} · tx: ${cell.state || 'pending'}<br>` +
    (cell.txid ? `${cell.txid}<br><span class="hint">click to open in arcade</span>` : '') +
    (cell.err ? `<br><span style="color:var(--failed)">${cell.err}</span>` : '');
  tip.hidden = false;
  tip.style.left = Math.min(ev.clientX + 14, window.innerWidth - 440) + 'px';
  tip.style.top = (ev.clientY + 14) + 'px';
};
canvas.onmouseleave = () => { tip.hidden = true; };
window.addEventListener('resize', scheduleRender);

// Live updates. EventSource reconnects on its own, so a server restart or a
// dropped proxy connection recovers without a page reload.
const events = new EventSource('/api/events');
events.onmessage = (ev) => apply(JSON.parse(ev.data));

fetch('/api/state').then(r => r.json()).then(apply).catch(() => {});

// Exposed for the renderer test, which drives this file under a DOM stub. Not
// used by the page itself.
if (typeof module !== 'undefined' && module.exports) {
  module.exports = { apply, state, paint, MAX_HISTORY, MAX_CANVAS_PX };
}
