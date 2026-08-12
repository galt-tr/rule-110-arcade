// Rule 110 space-time diagram.
//
// Generations run down the page, cells across. A live cell is drawn in the
// colour of ITS OWN transaction's progress, so confirmation visibly washes down
// the pattern behind the leading edge — the automaton and the chain are the
// same picture.

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

const canvas = document.getElementById('grid');
const ctx = canvas.getContext('2d');
const tip = document.getElementById('tip');

let snapshot = null;
let cellPx = 8;       // driven by the zoom control, not the viewport
let follow = true;    // auto-scroll only while pinned to the newest row

/** Unpack a hex row into a bit array, cell 0 first (matches the contract). */
function bitsOf(hex, cells) {
  const bits = new Uint8Array(cells);
  for (let i = 0; i < cells; i++) {
    const byte = parseInt(hex.substr((i >> 3) * 2, 2), 16);
    bits[i] = (byte >> (i % 8)) & 1;
  }
  return bits;
}

function sizeCanvas(s) {
  canvas.width = s.cells * cellPx;
  canvas.height = Math.max(1, s.history.length) * cellPx;
  canvas.style.width = canvas.width + 'px';
  canvas.style.height = canvas.height + 'px';
}

/** Generation numbers beside the rows, thinned so they stay legible when zoomed out. */
function drawGutter(s) {
  const g = document.getElementById('gutter');
  const every = Math.max(1, Math.ceil(14 / cellPx));
  const lines = s.history.map((gen, i) =>
    (i % every === 0 ? String(gen.number).padStart(6) : ''));
  g.style.lineHeight = cellPx + 'px';
  g.style.fontSize = Math.min(10, Math.max(6, cellPx)) + 'px';
  g.textContent = lines.join('\n');
}

function draw(s) {
  document.getElementById('empty').hidden = s.history.length > 0;
  const wrap = document.getElementById('canvas-wrap');
  // Decide BEFORE resizing: once the canvas grows, the old scrollTop no longer
  // describes where the viewer was.
  const pinned = follow && atBottom(wrap);

  sizeCanvas(s);
  drawGutter(s);
  ctx.fillStyle = COLOR.dead;
  ctx.fillRect(0, 0, canvas.width, canvas.height);

  for (let g = 0; g < s.history.length; g++) {
    const gen = s.history[g];
    const bits = bitsOf(gen.row, s.cells);
    for (let c = 0; c < s.cells; c++) {
      const state = gen.cells[c] ? gen.cells[c].state : 'pending';
      // A dead cell is background regardless of its transaction — the pattern
      // has to stay readable as a pattern first — with one exception. A failure
      // is a fact about the chain, not about the automaton, and roughly half of
      // all cells are dead, so hiding those failures lets a stalled run look
      // perfectly healthy. Draw them in a dark red instead: visible as a
      // defect, too dim to be mistaken for a live cell.
      if (!bits[c] && state !== 'failed') continue;
      ctx.fillStyle = bits[c] ? (COLOR[state] || COLOR.pending) : COLOR.failedDead;
      // Cell 0 is drawn rightmost so the row reads like the printed form,
      // highest index leftmost.
      const x = (s.cells - 1 - c) * cellPx;
      ctx.fillRect(x, g * cellPx, cellPx, cellPx);
    }
  }

  // Only chase the leading edge when the viewer is already there. Scrolling
  // them back down while they are reading history is worse than losing the
  // live edge, which the follow toggle restores in one click.
  if (pinned) wrap.scrollTop = wrap.scrollHeight;
}

/** Within a row of the bottom — treated as "watching the live edge". */
function atBottom(wrap) {
  return wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight <= cellPx * 2;
}

function renderStats(s) {
  const sat = s.balance.toLocaleString();
  const rows = [
    ['cells', s.cells],
    ['rule', s.rule],
    ['generation', s.generation],
    ['transactions', s.totalTx.toLocaleString()],
    // "spendable", not "balance": the wallet total is a different and
    // misleading number — see chain.Funds.
    ['spendable', sat + ' sat'],
    ['fuel', s.poolCoins.toLocaleString() + ' coins'],
    ['chains', s.consensus ? `${s.cells}/${s.cells} agree` : `${s.failedCells} unproved`],
  ];
  document.getElementById('stats').innerHTML = rows
    .map(([k, v]) => `<span class="stat"><span>${k}</span> <b>${v}</b></span>`)
    .join('');

  document.getElementById('play').setAttribute('aria-pressed', s.mode === 'running');
  document.getElementById('pause').setAttribute('aria-pressed', s.mode === 'paused');

  const err = document.getElementById('err');
  err.hidden = !s.lastError;
  err.textContent = s.lastError || '';
}

/** Merge a streamed update into the history we already hold.
 *
 * Updates carry only a recent tail, so replacing the array wholesale would
 * throw away everything the viewer scrolled back to look at. Generations are
 * keyed by their number and the incoming copy wins. */
function merge(prev, next) {
  if (!prev || !prev.history.length) return next;
  const byNumber = new Map(prev.history.map(g => [g.number, g]));
  for (const g of next.history) byNumber.set(g.number, g);
  next.history = [...byNumber.values()].sort((a, b) => a.number - b.number);
  return next;
}

function apply(s) {
  snapshot = merge(snapshot, s);
  renderStats(snapshot);
  draw(snapshot);
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
  if (snapshot) draw(snapshot);
};

const followBox = document.getElementById('follow');
followBox.onchange = () => {
  follow = followBox.checked;
  if (follow && snapshot) {
    const wrap = document.getElementById('canvas-wrap');
    wrap.scrollTop = wrap.scrollHeight;
  }
};

// Scrolling away from the bottom releases follow, so reading history is not a
// fight with the renderer; scrolling back re-arms it.
document.getElementById('canvas-wrap').addEventListener('scroll', () => {
  if (!followBox.checked) return;
  follow = atBottom(document.getElementById('canvas-wrap'));
});

const rate = document.getElementById('rate');
const rateOut = document.getElementById('rateOut');
rate.oninput = () => { rateOut.textContent = (+rate.value).toFixed(2) + ' gen/s'; };
rate.onchange = () => control('rate', { rate: +rate.value });

/** Which cell is under the pointer, or null. */
function cellAt(ev) {
  if (!snapshot) return null;
  const r = canvas.getBoundingClientRect();
  const g = Math.floor((ev.clientY - r.top) / cellPx);
  const c = snapshot.cells - 1 - Math.floor((ev.clientX - r.left) / cellPx);
  const gen = snapshot.history[g];
  if (!gen || c < 0 || c >= snapshot.cells) return null;
  return { gen, cell: gen.cells[c] || {}, index: c };
}

/** Arcade's status page for a transaction. */
function arcadeTxURL(txid) {
  return snapshot.arcadeUrl.replace(/\/+$/, '') + '/tx/' + txid;
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
  const bits = bitsOf(gen.row, snapshot.cells);
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
window.addEventListener('resize', () => { if (snapshot) draw(snapshot); });

// Live updates. EventSource reconnects on its own, so a server restart or a
// dropped proxy connection recovers without a page reload.
const events = new EventSource('/api/events');
events.onmessage = (ev) => apply(JSON.parse(ev.data));

fetch('/api/state').then(r => r.json()).then(apply).catch(() => {});
