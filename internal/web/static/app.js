// Rule 110 space-time diagram.
//
// Generations run down the page, cells across. A live cell is drawn in the
// colour of ITS OWN transaction's progress, so confirmation visibly washes down
// the pattern behind the leading edge — the automaton and the chain are the
// same picture.
//
// The renderer is incremental, and it has to be. A push carries a short tail
// several times a second, and the diagram holds a thousand-odd generations of a
// 256-cell ring; repainting all of it per push is a quarter of a million
// fillRect calls per frame for a few dozen cells that actually changed. So the
// canvas is treated as a durable surface: rows are painted once, scrolled with
// the bitmap when the window slides, and repainted only when their generation is
// genuinely replaced.

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

/** How many generations the client caches, across the WHOLE archive.
 *
 * Not a window on the newest rows any more — an LRU over wherever the viewer
 * has been. The diagram is scrolled, not tailed, so the useful thing to keep is
 * what is near the scroll position, and what falls out is re-fetched in a
 * request that costs about ten kilobytes.
 *
 * Each cached generation is a row of hex and one status character per cell —
 * ~350 bytes — rather than 256 objects carrying transaction ids. That is what
 * makes holding thousands of them affordable at all. */
const MAX_CACHED = 8192;

/** How long the scroll must be still before the archive is asked for anything.
 *
 * Dragging the scrollbar across the run redraws every frame, and every frame
 * lands on a window nobody has cached. Fetching per frame measured at 59
 * requests and ~8.3 million cell_txs rows scanned for ONE second of one user
 * flinging the bar — far more load than ten people reading. Waiting for the
 * motion to stop turns that into a single request for the window they actually
 * landed on. Painting is NOT deferred: whatever is already cached draws
 * immediately, so the diagram still moves under the cursor. */
const FETCH_SETTLE_MS = 120;

/** How many rows to render beyond the viewport, above and below.
 *
 * Scrolling within the overscan repaints nothing. It is the difference between
 * a drag that feels continuous and one that fetches on every frame. */
const OVERSCAN = 200;

/** Ceiling on either canvas dimension, under the browsers' ~32767px limit.
 *
 * It no longer bounds what is VIEWABLE — the canvas is the size of the viewport
 * plus overscan, and the scrollbar comes from an empty spacer as tall as the
 * whole run. This survives as a floor-of-last-resort so a very tall window or a
 * very large zoom cannot produce a canvas the browser silently refuses to
 * allocate, which does not degrade — it goes blank. */
const MAX_CANVAS_PX = 32000;

/** Ceiling on canvas AREA, in pixels.
 *
 * The dimension cap above is necessary and not sufficient. Cost is width times
 * height, and width is now cells x cellPx: a 256-cell ring at 8px is 2048px
 * wide, so a legal 32000px height is 65 megapixels — a quarter of a gigabyte of
 * bitmap, doubled while reflow holds the carry copy. Desktop browsers swap and
 * stutter; iOS Safari refuses outright somewhere near 16M and the diagram goes
 * blank, which is the same failure the dimension cap was added to prevent and
 * was reached a different way. */
const MAX_CANVAS_AREA = 16e6;

const canvas = document.getElementById('grid');
const ctx = canvas.getContext('2d');
const tip = document.getElementById('tip');
const wrap = document.getElementById('canvas-wrap');
const surface = document.getElementById('surface');
// pinnedBox, not `pinned`: draw() already has a local `pinned` boolean for
// "the viewer is watching the live edge", and the shadowing was silent.
const pinnedBox = document.getElementById('pinned');

// 4px, not 8. Width is cells x cellPx and the ring is 256: at 8px the diagram
// opens 2048px wide, which is wider than most viewports, so the live edge starts
// off-screen behind a horizontal scrollbar. Zooming in is a deliberate act;
// having to zoom out to see anything is not.
let cellPx = 4;       // driven by the zoom control, not the viewport
let follow = true;    // auto-scroll only while pinned to the newest row

/** Everything the client holds.
 *
 * `snap` is the newest scalars. `rows` is an LRU cache of COMPACT generations
 * keyed by generation number — `{number, row, s}` where `s` is one status
 * character per cell — filled from /api/history as the viewer scrolls and from
 * the live stream at the frontier. `extent` is what the archive says it holds,
 * and is what the scrollbar measures.
 *
 * There is deliberately no array and no index: position is arithmetic on the
 * generation number now, so nothing has to be kept sorted. */
const state = {
  snap: null,
  rows: new Map(),
  extent: { oldest: 0, newest: 0, count: 0, empty: true },
  /** Ranges already requested, so a drag does not ask twice. */
  inflight: new Set(),
};

/** Status character -> palette key. The compact archive uses the first letter
 *  of the stored status and '-' for a cell with no record; the live stream uses
 *  the first letter of the display state. Both land here. */
const STATE_OF = {
  m: 'mined', s: 'seen', b: 'broadcast', f: 'failed',
  a: 'pending', p: 'pending', '-': 'pending',
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

/** Everything about a generation that reaches the canvas.
 *
 * Now literally the stored form: the row's bits and one status character per
 * cell. Building a signature used to mean walking 256 objects; it is now a
 * string concatenation, because that IS the representation. */
function signatureOf(gen) {
  return (gen.row || '') + '|' + (gen.s || '');
}

/** The window currently on the canvas: first generation number and count.
 *  Hit-testing and the gutter read this. */
let view = { base: 0, rows: 0 };

/** Unpack a hex row into a bit array, cell 0 first (matches the contract).
 *
 * Cached on the generation, which is safe precisely because the cache is
 * invalidated the only way it can go stale: a generation whose row changed
 * arrives as a new object. */
function bitsOf(gen, cells) {
  if (gen._bits && gen._bits.length === cells && gen._bitsRow === gen.row) return gen._bits;
  const hex = gen.row || '';
  const bits = new Uint8Array(cells);
  for (let i = 0; i < cells; i++) {
    const byte = parseInt(hex.substr((i >> 3) * 2, 2), 16);
    bits[i] = (byte >> (i % 8)) & 1;
  }
  gen._bits = bits;
  gen._bitsRow = gen.row;
  return bits;
}

/** Fold a pushed snapshot into the cache.
 *
 * The stream carries the newest few generations in full, with a record per
 * cell; the archive carries everything in the compact form. Both are reduced to
 * the compact form HERE so that exactly one representation reaches the
 * renderer — the alternative is two code paths for drawing the same pixels,
 * differing only near the frontier, which is precisely where a bug would be
 * least visible.
 *
 * Live data WINS over cached: near the frontier the stream's status is fresher
 * than whatever the archive returned when the window was fetched.
 */
function ingest(s) {
  state.snap = s;
  const cells = s.cells || 0;
  for (const g of s.history || []) {
    const genCells = g.cells || [];
    let chars = '';
    for (let i = 0; i < cells; i++) {
      const c = genCells[i];
      chars += c && c.state ? c.state[0] : 'p';
    }
    state.rows.set(g.number, { number: g.number, row: g.row, s: chars });
  }
  // The stream is the authority on how far the run has got.
  if (typeof s.generation === 'number' && s.generation > state.extent.newest) {
    state.extent.newest = s.generation;
    state.extent.count = state.extent.newest - state.extent.oldest + 1;
    state.extent.empty = false;
  }
  trimCache();
}

/** Bound the cache. Evicts whatever is furthest from the current window, since
 *  that is the least likely to be scrolled back to. */
function trimCache() {
  if (state.rows.size <= MAX_CACHED) return;
  const centre = view.base + view.rows / 2;
  const keys = [...state.rows.keys()].sort(
    (a, b) => Math.abs(b - centre) - Math.abs(a - centre));
  for (let i = 0; i < keys.length && state.rows.size > MAX_CACHED; i++) {
    state.rows.delete(keys[i]);
  }
}

/** How many rows the canvas should carry: the viewport plus overscan either
 *  side, clamped to what a canvas can actually be.
 *
 * This is the change that makes the whole archive viewable. The canvas used to
 * be as tall as the history, which put a hard ceiling on the history at ~32,000
 * rows (3,906 at the default zoom, area-limited) and 100,000 flatly out of
 * reach. It is now the size of what you can see, and the scrollbar comes from an
 * empty spacer instead — so the number of generations stops being a rendering
 * problem at all. */
function windowRows(cells) {
  // EXACTLY the scrollport, plus one row for a partial at each edge — and NOT a
  // row more.
  //
  // The canvas rides in a position:sticky box, and sticky cannot pin a box
  // taller than the scrollport: past that the browser just lets it scroll away,
  // which is precisely what it did. The first version added OVERSCAN rows to
  // the canvas itself, making it 2,300px in a 700px viewport, and the diagram
  // drifted off screen leaving a sliver behind.
  //
  // Overscan still exists — it is what ensureRows PREFETCHES — but prefetching
  // is about which rows are in memory, and this is about how big a bitmap the
  // browser has to keep pinned. Conflating the two was the bug.
  const visible = Math.ceil((wrap.clientHeight || 600) / cellPx) + 2;
  const byHeight = Math.floor(MAX_CANVAS_PX / cellPx);
  const width = Math.max(1, (cells || 1) * cellPx);
  const byArea = Math.floor(MAX_CANVAS_AREA / (width * cellPx));
  return Math.max(1, Math.min(visible, byHeight, byArea));
}

/** The window the viewer most recently landed on, and the timer that asks for
 *  it. See FETCH_SETTLE_MS. */
let wanted = null;
let fetchTimer = null;
function scheduleFetch() {
  if (fetchTimer) clearTimeout(fetchTimer);
  fetchTimer = setTimeout(() => {
    fetchTimer = null;
    if (wanted) ensureRows(wanted.base, wanted.rows);
  }, FETCH_SETTLE_MS);
}

/** Ask the archive for any generation in [from, from+count) we do not hold.
 *
 * Requests are issued per missing RUN rather than per generation, and a run
 * already in flight is not asked for twice — a drag crosses hundreds of
 * generations and would otherwise issue hundreds of requests. */
async function ensureRows(from, count) {
  if (state.extent.empty) return;
  const lo = Math.max(from, state.extent.oldest);
  const hi = Math.min(from + count - 1, state.extent.newest);
  if (hi < lo) return;

  const runs = [];
  let start = null;
  for (let n = lo; n <= hi; n++) {
    const have = state.rows.has(n);
    if (!have && start === null) start = n;
    if ((have || n === hi) && start !== null) {
      runs.push([start, (have ? n - 1 : n)]);
      start = null;
    }
  }

  for (const [a, b] of runs) {
    const key = a + ':' + b;
    if (state.inflight.has(key)) continue;
    state.inflight.add(key);
    try {
      const res = await fetch(`/api/history?from=${a}&count=${b - a + 1}`);
      if (!res.ok) continue;
      const body = await res.json();
      for (const g of body.generations || []) {
        // Never overwrite a live row: the stream is fresher at the frontier.
        if (!state.rows.has(g.n)) state.rows.set(g.n, { number: g.n, row: g.row, s: g.s });
      }
      trimCache();
      scheduleRender();
    } catch {
      /* a failed window simply stays blank and is retried on the next scroll */
    } finally {
      state.inflight.delete(key);
    }
  }
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
  // The spacer is what the scrollbar measures: as tall as the WHOLE run, while
  // the canvas above stays the size of a viewport. This is the trick that makes
  // 100,000 generations scrollable at all.
  surface.style.height = Math.max(1, state.extent.count * cellPx) + 'px';

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

  const chars = gen.s || '';
  for (let c = 0; c < cells; c++) {
    const cellState = STATE_OF[chars[c]] || 'pending';
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
function drawGutter(base, rows) {
  if (gutterState.base === base && gutterState.rows === rows &&
      gutterState.cellPx === cellPx) {
    return;
  }
  const g = document.getElementById('gutter');
  const every = Math.max(1, Math.ceil(14 / cellPx));
  // padStart(8), not 6: six digits stops aligning at a million generations,
  // which at half a generation per second is under a month away.
  const lines = [];
  for (let i = 0; i < rows; i++) {
    lines.push(i % every === 0 ? String(base + i).padStart(8) : '');
  }
  g.style.lineHeight = cellPx + 'px';
  g.style.fontSize = Math.min(10, Math.max(6, cellPx)) + 'px';
  g.textContent = lines.join('\n');
  gutterState.base = base;
  gutterState.rows = rows;
  gutterState.cellPx = cellPx;
}

function draw() {
  const s = state.snap;
  if (!s) return;
  const cells = s.cells || 0;

  // Where the viewer is, in generations. This is the whole inversion: the
  // window used to be "the newest N we hold", so there was no way to look at
  // generation 40,000 of 100,000. It is now wherever the scrollbar is.
  const rows = windowRows(cells);
  const firstVisible = Math.floor(wrap.scrollTop / cellPx);
  let base = state.extent.oldest + firstVisible;
  base = Math.max(state.extent.oldest, base);
  if (base + rows > state.extent.newest + 1) {
    base = Math.max(state.extent.oldest, state.extent.newest + 1 - rows);
  }

  document.getElementById('empty').hidden = !state.extent.empty;

  // Decide BEFORE resizing: once the geometry changes, the old scrollTop no
  // longer describes where the viewer was.
  const pinned = follow && atBottom();

  reflow(base, rows, cells);
  view = { base, rows };
  drawGutter(base, rows);

  for (let i = 0; i < rows; i++) {
    const number = base + i;
    const gen = state.rows.get(number);
    if (!gen) continue;
    if (paint.drawn.get(number) === gen) continue;
    const sig = signatureOf(gen);
    paint.drawn.set(number, gen);
    if (paint.sig.get(number) === sig) continue; // re-sent, identical pixels
    paintRow(gen, i, cells);
    paint.sig.set(number, sig);
  }

  // Rows are whole generations but scrolling is per pixel, so shift the pinned
  // box up by the remainder. Without this every row is drawn up to cellPx-1
  // pixels away from where the scrollbar says it is, and the diagram shivers as
  // you drag.
  pinnedBox.style.transform = `translateY(${-(wrap.scrollTop % cellPx)}px)`;

  // Ask for what the window wants once the scroll settles, with overscan either
  // side — prefetching is where overscan belongs, not in the canvas size.
  wanted = { base: Math.max(state.extent.oldest, base - OVERSCAN), rows: rows + 2 * OVERSCAN };
  scheduleFetch();

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
  renderControls(state.snap);
  renderFund(state.snap);
  draw();
}

/** Hide the clock controls when the deployment refuses them.
 *
 * Presentation only — the server returns 403 for every one of these whether or
 * not the buttons exist. Zoom and follow stay: they are client-side and affect
 * nobody else. */
function renderControls(s) {
  for (const id of ['play', 'pause', 'step']) {
    const el = document.getElementById(id);
    if (el) el.hidden = !!s.locked;
  }
  const rateLabel = document.getElementById('rateLabel');
  if (rateLabel) rateLabel.hidden = !!s.locked;
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
  if (followBox.checked) follow = atBottom();
  // Scrolling now CHANGES WHAT IS DRAWN, not just which part of a painted
  // bitmap is visible. This is the listener that turns the spacer into a view
  // of the archive.
  scheduleRender();
});

const rate = document.getElementById('rate');
const rateOut = document.getElementById('rateOut');
rate.oninput = () => { rateOut.textContent = (+rate.value).toFixed(2) + ' gen/s'; };
rate.onchange = () => control('rate', { rate: +rate.value });

// ---------------------------------------------------------------------------
// Funding
// ---------------------------------------------------------------------------

/** The payment instruction from GET /api/funding, or null if this deployment
 *  does not take public funding (the endpoint 404s). */
let fundTarget = null;

/** The funding panel, which is ALWAYS shown once we know funding is possible.
 *
 * It used to appear only when the automaton was starved or bootstrapping, and
 * that was wrong twice over. It made "you can pay for this" look like an error
 * state, when on a public exhibit it is the whole social contract — the thing
 * runs on real fees and strangers keep it alive. And it hid the one control
 * that matters at exactly the moment it mattered, because the panel sat below a
 * canvas thousands of pixels tall: the automaton stopped, the page said so
 * somewhere nobody was looking, and it simply appeared broken.
 *
 * So the panel is permanent and its URGENCY is what changes. Three states, and
 * they escalate on evidence the engine already publishes:
 *
 *   stopped  starved — every cell is out of coin and nothing is advancing.
 *   low      the pool cannot fund a full generation, or cells are already
 *            retrying a shortfall. This is the LEADING indicator: starvation
 *            is only declared after a 20-second grace, so without this the
 *            page looks healthy right up until it halts.
 *   (none)   running, funded, quietly fundable anyway.
 */
function renderFund(s) {
  const panel = document.getElementById('fund');
  if (!panel) return;

  // No target means /api/funding 404'd: this deployment does not take public
  // funding, so the panel can never be useful.
  if (!fundTarget) {
    panel.hidden = true;
    return;
  }
  panel.hidden = false;

  const title = document.getElementById('fundTitle');
  const why = document.getElementById('fundWhy');
  const btn = document.getElementById('fundBtn');
  const boot = s.bootstrap;
  const phase = boot?.phase;

  // Mid-bootstrap and already paid: money has arrived and is being spent.
  // Keep the panel up so the sequence is visible, but stop asking.
  if (phase && phase !== 'funding' && phase !== 'waiting') {
    panel.className = 'working';
    title.textContent = phase === 'fuel' ? 'Minting fuel…' : 'Creating generation 0…';
    why.textContent = 'Funded — this takes a few seconds.';
    btn.disabled = true;
    return;
  }
  btn.disabled = false;

  if (boot) {
    panel.className = 'stopped';
    const need = boot.minSatoshis || 0;
    const have = boot.have || 0;
    title.textContent = 'Nothing has run yet — fund it to start it';
    why.textContent =
      `It needs about ${need.toLocaleString()} sat to mint its first fuel and create ` +
      `generation 0. It has ${have.toLocaleString()}.`;
    return;
  }

  if (s.starved) {
    panel.className = 'stopped';
    title.textContent = 'STOPPED — out of funds';
    why.textContent =
      'Every cell transition costs a fee and the pool is empty, so the automaton has ' +
      'halted. Any amount restarts it, and it resumes on its own.';
    return;
  }

  if (isLowOnFuel(s)) {
    panel.className = 'low';
    title.textContent = 'Running low on fuel';
    why.textContent =
      `${s.poolCoins.toLocaleString()} coins left and one funds one cell transition, so ` +
      `there is under a generation in hand. It stops when they run out.`;
    return;
  }

  panel.className = '';
  title.textContent = 'Keep it running';
  why.textContent =
    `Every one of the ${s.cells} cells pays a fee to advance. ${s.poolCoins.toLocaleString()} ` +
    `coins in hand. Anyone can top it up, any time.`;
}

/** Whether the pool can still fund a whole generation.
 *
 * One coin funds one cell transition, so fewer coins than cells means the next
 * generation cannot complete — which is the honest definition of "low" here and
 * is visible well before the engine declares starvation. waitingOnCoin catches
 * the same thing from the other side: cells already retrying a shortfall. */
function isLowOnFuel(s) {
  return s.waitingOnCoin > 0 || (s.poolCoins || 0) < s.cells;
}

/** Say something to the payer, and leave it said. */
function fundSay(msg, kind) {
  const el = document.getElementById('fundStatus');
  el.textContent = msg;
  el.className = 'fund-status' + (kind ? ' ' + kind : '');
  el.hidden = !msg;
}

async function loadFundTarget() {
  const res = await fetch('/api/funding');
  if (!res.ok) return; // public funding is off; the panel stays hidden forever
  fundTarget = await res.json();

  document.getElementById('fundAddress').textContent = fundTarget.address;
  document.getElementById('fundNetwork').textContent = fundTarget.network;
  const amount = document.getElementById('fundAmount');
  amount.value = String(fundTarget.suggestedSatoshis || fundTarget.minSatoshis || 0);
  amount.min = String(fundTarget.minSatoshis || 0);
  scheduleRender();
}

document.getElementById('fundBtn').onclick = async () => {
  const btn = document.getElementById('fundBtn');
  const satoshis = Number(document.getElementById('fundAmount').value);

  if (!Number.isFinite(satoshis) || satoshis < (fundTarget.minSatoshis || 0)) {
    fundSay(`The minimum is ${(fundTarget.minSatoshis || 0).toLocaleString()} satoshis.`, 'bad');
    return;
  }

  btn.disabled = true;
  try {
    fundSay('Looking for a wallet…');
    if (!await Wallet.probe()) {
      fundSay(Wallet.explain(new Error('none')), 'bad');
      document.getElementById('fundManual').open = true;
      return;
    }

    // Coarse guard only. BRC-100 cannot say WHICH test network a wallet is on,
    // so this catches a mainnet wallet and nothing subtler; the authoritative
    // check is our own arcade refusing the broadcast, which is safe to rely on
    // because the wallet was asked not to send.
    const net = await Wallet.network();
    const wantMainnet = fundTarget.network === 'main';
    if (net && (net === 'mainnet') !== wantMainnet) {
      fundSay(`This deployment runs on ${fundTarget.network}, but your wallet is on ${net}. ` +
        `Nothing has been spent.`, 'bad');
      return;
    }

    fundSay('Waiting for you to approve the payment…');
    const { beef, txid } = await Wallet.fundWith(fundTarget.lockingScript, satoshis);

    fundSay('Broadcasting and crediting…');
    const res = await fetch('/api/fund', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ beef }),
    });
    if (!res.ok) {
      fundSay((await res.text()).trim() || 'The payment was refused.', 'bad');
      return;
    }

    const out = await res.json();
    fundSay(`Thank you — ${out.satoshis.toLocaleString()} satoshis credited.`, 'good');
    // Best effort: let the wallet mark its own noSend action as sent.
    Wallet.settle(txid || out.txid);
  } catch (err) {
    fundSay(Wallet.explain(err), 'bad');
  } finally {
    btn.disabled = false;
  }
};

document.getElementById('fundCopy').onclick = async () => {
  try {
    await navigator.clipboard.writeText(fundTarget.address);
    fundSay('Address copied.', 'good');
  } catch {
    fundSay('Could not copy; select the address by hand.', 'bad');
  }
};

document.getElementById('fundTxidBtn').onclick = async () => {
  const txid = document.getElementById('fundTxid').value.trim();
  if (!txid) return;
  fundSay('Looking that transaction up…');
  const res = await fetch('/api/fund', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ txid }),
  });
  if (!res.ok) {
    fundSay((await res.text()).trim() || 'That transaction could not be credited.', 'bad');
    return;
  }
  const out = await res.json();
  fundSay(`Thank you — ${out.satoshis.toLocaleString()} satoshis credited.`, 'good');
};

/** Which cell is under the pointer, or null. */
function cellAt(ev) {
  if (!state.snap) return null;
  const cells = state.snap.cells;
  const r = canvas.getBoundingClientRect();
  const row = Math.floor((ev.clientY - r.top) / cellPx);
  const c = cells - 1 - Math.floor((ev.clientX - r.left) / cellPx);
  if (row < 0 || row >= view.rows || c < 0 || c >= cells) return null;
  const number = view.base + row;
  const gen = state.rows.get(number);
  if (!gen) return null;
  return { gen, number, index: c, stateChar: (gen.s || '')[c] || '-' };
}

/** Fetch one cell's transaction id.
 *
 * On demand, and that is the point rather than an optimisation. Transaction ids
 * were 62% of the payload and the UI wants exactly one of them at a time — the
 * one under the pointer. Dropping them from the bulk payload is what took a
 * page load from 25.5 MB to kilobytes, and this is the other half of that
 * trade. `cell_txs` is keyed on (generation, cell), so it is a point lookup. */
const txCache = new Map();
/** Guards against a slow lookup overwriting a newer hover. */
let hoverToken = 0;
async function detailFor(number, cell) {
  const key = number + ':' + cell;
  if (txCache.has(key)) return txCache.get(key);
  try {
    const res = await fetch(`/api/tx?generation=${number}&cell=${cell}`);
    const d = res.ok ? await res.json() : null;
    txCache.set(key, d);
    return d;
  } catch {
    return null;
  }
}

canvas.onclick = async (ev) => {
  const hit = cellAt(ev);
  if (!hit || hit.stateChar === '-') return;
  const d = await detailFor(hit.number, hit.index);
  if (d && d.txid) window.open(arcadeTxURL(d.txid), '_blank', 'noopener');
};

canvas.onmousemove = (ev) => {
  const hit = cellAt(ev);
  if (!hit) { tip.hidden = true; return; }
  const { gen, number, index: c } = hit;
  canvas.style.cursor = hit.stateChar === '-' ? 'default' : 'pointer';

  const bits = bitsOf(gen, state.snap.cells);
  const cellState = STATE_OF[hit.stateChar] || 'pending';
  const head =
    `<b>cell ${c}</b> · generation ${number}<br>` +
    `state: ${bits[c] ? 'alive' : 'dead'} · tx: ${cellState}<br>`;

  // The id and the refusal reason are not in the bulk payload any more — that
  // is the trade that made the archive viewable — so they are fetched for the
  // cell under the pointer and cached. Render immediately with what is known,
  // then fill them in: a tooltip that waits on a request is a tooltip that
  // flickers.
  tip.innerHTML = head;
  tip.hidden = false;
  tip.style.left = Math.min(ev.clientX + 14, window.innerWidth - 440) + 'px';
  tip.style.top = (ev.clientY + 14) + 'px';

  if (hit.stateChar === '-') return;
  const token = ++hoverToken;
  detailFor(number, c).then((d) => {
    // A later hover has already replaced this one; do not overwrite it.
    if (token !== hoverToken || !d) return;
    tip.innerHTML = head +
      (d.txid ? `${d.txid}<br><span class="hint">click to open in arcade</span>` : '') +
      (d.err ? `<br><span style="color:var(--failed)">${d.err}</span>` : '');
  });
};
canvas.onmouseleave = () => { tip.hidden = true; };
window.addEventListener('resize', scheduleRender);

// Live updates. EventSource reconnects on its own, so a server restart or a
// dropped proxy connection recovers without a page reload.
const events = new EventSource('/api/events');
events.onmessage = (ev) => apply(JSON.parse(ev.data));

/** Learn how much history exists, so the spacer — and therefore the scrollbar —
 *  can be as tall as the whole run. */
async function loadExtent() {
  const res = await fetch('/api/extent');
  if (!res.ok) return;
  const e = await res.json();
  if (e.empty) return;
  state.extent = { oldest: e.oldest, newest: e.newest, count: e.count, empty: false };
  scheduleRender();
}

/** Put generation n at the top of the viewport. */
function jumpTo(n) {
  if (state.extent.empty) return;
  const clamped = Math.min(Math.max(n, state.extent.oldest), state.extent.newest);
  follow = false;
  followBox.checked = false;
  wrap.scrollTop = (clamped - state.extent.oldest) * cellPx;
  scheduleRender();
}

const jumpInput = document.getElementById('jump');
const jumpBtn = document.getElementById('jumpBtn');
if (jumpBtn) {
  const go = () => {
    const n = Number(jumpInput.value);
    if (!Number.isFinite(n)) return;
    jumpTo(n);
    // Linkable: someone who finds something interesting can send it to
    // somebody else.
    history.replaceState(null, '', '#gen=' + Math.round(n));
  };
  jumpBtn.onclick = go;
  jumpInput.onkeydown = (ev) => { if (ev.key === 'Enter') go(); };
}

loadExtent()
  .then(() => {
    const m = /(?:^|#|&)gen=(\d+)/.exec(location.hash);
    if (m) {
      jumpTo(Number(m[1]));
      return;
    }
    // No deep link: open at the live edge, which is where the tail-anchored
    // version always was and what someone arriving at a running automaton
    // expects to see. Scrolling back is the new capability; landing at
    // generation 0 of a 20,000-generation run would be a regression dressed as
    // a feature.
    wrap.scrollTop = wrap.scrollHeight;
    scheduleRender();
  })
  .catch(() => {});
fetch('/api/state').then(r => r.json()).then(apply).catch(() => {});
loadFundTarget().catch(() => {});

// Exposed for the renderer test, which drives this file under a DOM stub. Not
// used by the page itself.
if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    apply, state, paint, MAX_CACHED, MAX_CANVAS_PX, MAX_CANVAS_AREA, OVERSCAN,
    draw, jumpTo, windowRows,
    // The panel is driven by GET /api/funding, which the harness has no network
    // for. Exposing the setter is cheaper than stubbing fetch well enough to
    // deliver a body, and it is the same field loadFundTarget assigns.
    setFundTarget(t) { fundTarget = t; },
    setExtent(e) { state.extent = e; },
    setFollow(f) { follow = f; },
    wanted: () => wanted,
    view: () => view,
    canvas,
  };
}
