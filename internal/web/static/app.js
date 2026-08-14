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

/** How long to trust the archive's extent before re-reading it. */
const EXTENT_RECHECK_MS = 5000;

/** How far behind the frontier an empty archive window has to be before it
 *  means the archive moved rather than simply not having been written yet.
 *
 * The extent's `newest` tracks the LIVE generation, which always runs ahead of
 * what has been persisted, so a miss near the frontier is ordinary write lag and
 * must not be mistaken for a stale extent — treating it as one would re-read the
 * extent on every frame at the live edge. */
const ARCHIVE_LAG_SLACK = 512;

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
const gutter = document.getElementById('gutter');

// 4px, not 8. Width is cells x cellPx and the ring is 256: at 8px the diagram
// opens 2048px wide, which is wider than most viewports, so the live edge starts
// off-screen behind a horizontal scrollbar. Zooming in is a deliberate act;
// having to zoom out to see anything is not.
//
// It is also the width this STARTS at, not the width it is stuck at: on a
// viewport too narrow to hold the ring even at 4px, fitZoom() takes it down to
// whatever does fit. See fitCellPx.
let cellPx = 4;       // driven by the zoom control, not the viewport
// Off by default, and the page still OPENS at the live edge — the two are
// different things. Landing on the newest generation is what someone arriving
// at a running automaton expects; being dragged to it once a second afterwards
// is what makes the history unreadable the moment they scroll back. "Go to
// bottom" covers the case this used to cover by force.
let follow = false;   // auto-scroll only while pinned to the newest row

/** Whether the one-time landing on the newest generation is still owed.
 *
 * Cleared by taking it, and by anything that says where to look instead — a
 * #gen= deep link, or a jump. */
let openAtLiveEdge = true;

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
  /** The newest generation the tail carries; -1 when it carries none. */
  let top = -1;
  for (const g of s.history || []) {
    const genCells = g.cells || [];
    let chars = '';
    for (let i = 0; i < cells; i++) {
      const c = genCells[i];
      chars += c && c.state ? c.state[0] : 'p';
    }
    state.rows.set(g.number, { number: g.number, row: g.row, s: chars });
    if (g.number > top) top = g.number;
  }
  // The stream is the authority on how far the run has got — and, crucially, on
  // whether it is still the same run. Ratcheting `newest` upward is right while
  // a run grows and wrong the moment one restarts: see forgetRun.
  //
  // The tail's HIGHEST NUMBER, and deliberately not `s.generation`. The two
  // measure different cells. `s.generation` is the frontier — the SLOWEST cell,
  // engine.frontierLocked — while `extent.newest` is MAX(number) FROM
  // generations, and that row is written by whichever cell reached the
  // generation FIRST. Cells run up to chain MaxLag (32) apart, so on a healthy
  // ring the frontier sits far enough below the extent to trip the restart test
  // below, whose threshold is 2. It did, on nearly every push: the cache was
  // cleared several times a second, the canvas blanked, and the archive refilled
  // it just in time for the next push to blank it again. Measured live —
  // /api/state said generation 219 while /api/extent said newest 251.
  //
  // The tail's top is the RIGHT side of that comparison because it is the same
  // quantity: the newest entry of the engine's history is created by the same
  // ensureGeneration call that writes the generations row the extent reads. It
  // rises monotonically within a run (the rolling window drops from the front,
  // never the top) and collapses on a re-genesis, which is exactly the signal
  // forgetRun wants.
  //
  // A deployment that has never run sends no history at all, so there is no
  // position to read and neither branch is taken — falling back to
  // `s.generation` here would reintroduce the bug at the one moment it looks
  // safest.
  if (top >= 0) {
    if (top > state.extent.newest) {
      state.extent.newest = top;
      state.extent.count = state.extent.newest - state.extent.oldest + 1;
      state.extent.empty = false;
    } else if (top < state.extent.newest - RESTART_DROP) {
      forgetRun();
    }
  }
  trimCache();
}

/** How far the run has to appear to go BACKWARDS before that means a restart
 *  rather than jitter at the frontier. */
const RESTART_DROP = 2;

/** How long after forgetting a run before another forget is believed.
 *
 * A run restarts once, so a second forget four pushes later is not a second
 * restart — it is whatever the detector above got wrong. The two failure modes
 * are not comparable, which is why this guard is unconditional rather than
 * tuned: refusing to forget costs one stale window, corrected by the next extent
 * read a moment later, while forgetting on a loop empties the cache faster than
 * the archive can refill it and the diagram strobes. Comfortably more than
 * engine.PublishInterval, so an ordinary push can never be the thing that
 * releases it. */
const FORGET_COOLDOWN_MS = 2000;
let forgotAt = 0;

/** The run went backwards, so it is not the run we have been drawing.
 *
 * Generation numbers are reused from zero by the next run, so every cached row
 * now describes a different automaton that happens to share numbering — keeping
 * them would draw one run's history under another's. The archive is re-read
 * rather than adjusted by guesswork, because believing an unchecked number is
 * what broke the page in the first place.
 *
 * Rate-limited, because everything below this line is destructive and the cost
 * of doing it when it was not warranted is a blank canvas. */
function forgetRun() {
  const now = Date.now();
  if (now - forgotAt < FORGET_COOLDOWN_MS) return;
  forgotAt = now;
  state.rows.clear();
  state.inflight.clear();
  paint.drawn.clear();
  paint.sig.clear();
  paint.base = null;
  refreshExtent(true);
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

/** Whether the layout is in its phone form. Matches the 640px breakpoint in
 *  style.css; kept as a width read rather than matchMedia so it costs nothing at
 *  load time and works under the renderer test's DOM stub. */
function isNarrow() {
  return (window.innerWidth || 0) > 0 && window.innerWidth <= 640;
}

/** True once the viewer has moved the zoom slider themselves. */
let zoomChosen = false;

/** The largest cell size that fits the whole ring, or null to leave zoom alone.
 *
 * Only ever SHRINKS, and that asymmetry is the point. On a desktop the 4px
 * default already fits a 256-cell ring inside 1024px, so this returns null and
 * a zoom nobody complained about does not move. On a phone the same default
 * opens the canvas 1024px wide against 390px of viewport: you land in the middle
 * of the ring, seeing a third of it, with nothing to say the rest is there.
 *
 * Growing to fit would be the mirror image and is deliberately not done — it
 * would change the default zoom on every wide monitor to chase a bug that only
 * exists on narrow ones. */
function fitCellPx(cells) {
  const avail = (wrap.clientWidth || 0) - (gutter.offsetWidth || 0);
  if (!cells || avail <= 0) return null;
  if (cells * cellPx <= avail) return null;
  return Math.max(1, Math.min(8, Math.floor(avail / cells)));
}

/** Apply fitCellPx, unless the viewer has taken the zoom into their own hands.
 *
 * Runs on every draw rather than once at startup because `cells` is not known
 * until the first snapshot lands, and because a rotation changes the answer.
 * Guarded on zoomChosen so it can never argue with a slider somebody moved. */
function fitZoom(cells) {
  if (zoomChosen) return false;
  const fit = fitCellPx(cells);
  if (fit === null || fit === cellPx) return false;
  cellPx = fit;
  if (zoom) zoom.value = String(cellPx);
  if (zoomOut) zoomOut.textContent = cellPx + 'px';
  return true;
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
      // We asked for a range the extent says exists, well behind the frontier,
      // and the archive had nothing. The extent is wrong — the archive moved
      // under us (a restart, or pruning) — so re-read it rather than leaving the
      // viewer parked on a region that will never fill in.
      if (!(body.generations || []).length && b < state.extent.newest - ARCHIVE_LAG_SLACK) {
        refreshExtent(false);
      }
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
    // The dimensions of what is being carried, taken BEFORE the resize below
    // wipes the bitmap. The blit back uses them as an explicit source rectangle,
    // because the scratch canvas is generally larger than this frame's content
    // and the whole-image form would drag the leftovers of a previous, bigger
    // frame in with it.
    const carriedW = canvas.width;
    const carriedH = canvas.height;
    const carry = carriedW > 0 && carriedH > 0 ? carryFrom(canvas) : null;
    if (resized) {
      canvas.width = w;
      canvas.height = h;
    }
    fillBackground();
    if (carry) {
      ctx.drawImage(carry, 0, 0, carriedW, carriedH,
                    0, -shift * cellPx, carriedW, carriedH);
    }
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

/** The scratch bitmap reflow carries already-painted rows on, kept across
 *  frames.
 *
 * reflow runs on every frame where the window moves — every frame of a scrollbar
 * drag, and once per generation at the live edge, since `base` advances with the
 * archive whether or not anyone is following it. Allocating a full-size canvas
 * each time is about 2.8MB per frame at 256 cells, which is megabytes of garbage
 * a second for a bitmap that is read once and dropped; the collector pauses that
 * buys arrive as dropped frames in the middle of the drag.
 *
 * Grown to fit and never shrunk. The sizes it sees are already bounded by
 * MAX_CANVAS_AREA, so the high-water mark is bounded too, and a scratch that is
 * merely too big costs nothing — reflow blits an explicit source rectangle out
 * of it rather than the whole image. */
let carryCanvas = null;
let carryCtx = null;
function carryFrom(src) {
  if (!carryCanvas) {
    carryCanvas = document.createElement('canvas');
    carryCtx = carryCanvas.getContext('2d');
    // `copy`, not the default `source-over`, and set once because this context
    // draws nothing else. It makes the scratch EQUAL the source rather than the
    // source composited over whatever the last frame left, so a palette with any
    // alpha in it — --dead is opaque today, and nothing here should depend on
    // that staying true — cannot ghost the previous frame through this one.
    carryCtx.globalCompositeOperation = 'copy';
  }
  if (carryCanvas.width < src.width || carryCanvas.height < src.height) {
    // Assigning either dimension clears the bitmap, which is fine here — it is
    // about to be overwritten — but it is why both are raised in one go rather
    // than one at a time.
    carryCanvas.width = Math.max(carryCanvas.width, src.width);
    carryCanvas.height = Math.max(carryCanvas.height, src.height);
  }
  carryCtx.drawImage(src, 0, 0);
  return carryCanvas;
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
const gutterState = { base: null, rows: null, cellPx: null, narrow: null };
function drawGutter(base, rows) {
  // On a phone this column was costing about 37px of a 390px viewport to print
  // numbers at 6px, which is to say it was spending a tenth of the diagram's
  // width on something nobody could read. Narrow, it pads to the width the
  // numbers actually need and sets a floor of 9px on the type.
  const narrow = isNarrow();
  if (gutterState.base === base && gutterState.rows === rows &&
      gutterState.cellPx === cellPx && gutterState.narrow === narrow) {
    return;
  }
  const g = gutter;
  const every = Math.max(1, Math.ceil(14 / cellPx));
  // padStart(8), not 6: six digits stops aligning at a million generations,
  // which at half a generation per second is under a month away. Narrow, the
  // alignment comes from the widest number actually on screen instead.
  const width = narrow ? String(base + rows).length : 8;
  const lines = [];
  for (let i = 0; i < rows; i++) {
    lines.push(i % every === 0 ? String(base + i).padStart(width) : '');
  }
  g.style.lineHeight = cellPx + 'px';
  g.style.fontSize = (narrow ? Math.min(10, Math.max(9, cellPx))
                             : Math.min(10, Math.max(6, cellPx))) + 'px';
  g.textContent = lines.join('\n');
  gutterState.base = base;
  gutterState.rows = rows;
  gutterState.cellPx = cellPx;
  gutterState.narrow = narrow;
}

function draw() {
  const s = state.snap;
  if (!s) return;
  const cells = s.cells || 0;

  // Before anything is measured in cells: the cell size itself may still be too
  // big for the viewport. Nothing below this line is right if the ring does not
  // fit, because rows and scroll offsets are both denominated in cellPx.
  if (fitZoom(cells)) invalidate();

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

  // The one-time landing, taken now that reflow has given the spacer its real
  // height. Deliberately not the same thing as following: it puts the viewer at
  // the newest generation once, and leaves them free to scroll away.
  if (openAtLiveEdge && !state.extent.empty && state.rows.size) {
    openAtLiveEdge = false;
    wrap.scrollTop = wrap.scrollHeight;
    scheduleRender();   // the window moved; draw where it now is
    return;
  }

  // Only chase the leading edge when the viewer is already there. Scrolling
  // them back down while they are reading history is worse than losing the
  // live edge, which the follow toggle restores in one click.
  //
  // Not while a finger is down, though: this assignment lands mid-frame, and a
  // touch fling is a scroll the browser is already animating. Writing scrollTop
  // underneath it makes the diagram stutter and can cancel the gesture
  // outright. The next frame after the finger lifts catches up.
  if (pinned && !pointerDown) wrap.scrollTop = wrap.scrollHeight;
}

/** Within a row or so of the bottom — treated as "watching the live edge".
 *
 * The floor of 8px is for touch. The tolerance used to be cellPx * 2, which at
 * the 1px cells a phone now opens at is two pixels — and iOS reports a
 * fractional scrollTop and rubber-bands past the end, so "at the bottom" was
 * never true and follow released itself the moment it was armed. */
function atBottom() {
  const slack = Math.max(8, cellPx * 2);
  return wrap.scrollHeight - wrap.scrollTop - wrap.clientHeight <= slack;
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
  // From here on the zoom is the viewer's, and fitZoom stops touching it — a
  // fit that reasserted itself on the next resize would be a control that
  // undoes what you just did with it.
  zoomChosen = true;
  cellPx = +zoom.value;
  zoomOut.textContent = cellPx + 'px';
  // A zoom changes every row's height and position, so nothing on the bitmap
  // survives — but it still goes through the frame queue, because the slider
  // fires far faster than the display refreshes.
  invalidate();
};

/* The footer's overflow sheet.
 *
 * Only ever raised on a phone, because .more-toggle is display:none above the
 * breakpoint and .more is display:contents there — so the attribute this sets
 * has nothing to act on and the desktop footer is untouched by all of it.
 *
 * Not a <dialog>, which is what the explainers use and what would otherwise be
 * the house idiom: showModal() brings a focus trap and an inert page, and this
 * is a strip of controls sitting directly above the button that raised it, not
 * something to be shut out of the rest of the page for. */
const moreBtn = document.getElementById('moreBtn');
const moreBox = document.getElementById('more');
function setMore(open) {
  if (!moreBox || !moreBtn) return;
  if (open) moreBox.setAttribute('data-open', '');
  else moreBox.removeAttribute('data-open');
  moreBtn.setAttribute('aria-expanded', open ? 'true' : 'false');
}
if (moreBtn) {
  moreBtn.onclick = (ev) => {
    // Or the document listener below sees this same click and closes it again.
    ev.stopPropagation();
    setMore(!moreBox.hasAttribute('data-open'));
  };
  document.addEventListener('click', (ev) => {
    if (!moreBox.hasAttribute('data-open')) return;
    if (moreBox.contains(ev.target)) return;
    setMore(false);
  });
  document.addEventListener('keydown', (ev) => {
    if (ev.key === 'Escape' && moreBox.hasAttribute('data-open')) setMore(false);
  });
}

const followBox = document.getElementById('follow');
// The checkbox is the authority at startup, not the initial value of `follow`:
// a browser restores a checkbox across a reload, so reading it here keeps the
// two from disagreeing about a box the viewer can see.
follow = followBox.checked;
followBox.onchange = () => {
  follow = followBox.checked;
  if (follow) toBottom();
};

/** Put the newest generation in view and stay there.
 *
 * Both halves, because "catch me up" and "keep me caught up" are the same wish
 * at the live edge: arriving at the newest row only to drift off it a second
 * later would leave the button to be pressed again every generation. The
 * checkbox is set too, so the page never claims to be doing something
 * different from what it is doing. */
function toBottom() {
  follow = true;
  followBox.checked = true;
  wrap.scrollTop = wrap.scrollHeight;
  scheduleRender();
}
document.getElementById('toBottom').onclick = toBottom;

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

/** Set when a payment is credited, cleared when the fuel it bought appears.
 *
 * There is about a minute between the two: the payment lands in the wallet as
 * ordinary coin, and the fuel keeper then has to mint it into the denominated
 * pool the cells actually spend from. Nothing observable happens in between, so
 * without this a payer sees "thank you" and then an automaton that is still
 * stopped — which looks exactly like a deployment that took the money and did
 * nothing.
 *
 * `poolAt` is the coin count at the moment of payment, because the honest
 * signal that the money became fuel is that the pool GREW, not that time
 * passed. */
let awaitingFuel = null;

/** How long to keep saying "minting" before admitting it is taking too long.
 *
 * A spinner that never stops is worse than no spinner: it asserts progress it
 * cannot see. The keeper normally fills the pool in about a minute. */
const MINTING_PATIENCE_MS = 3 * 60 * 1000;

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
  const spin = document.getElementById('fundSpin');
  const boot = s.bootstrap;
  const phase = boot?.phase;

  // The pool growing is what clears the wait. Time alone does not: a keeper
  // that is stuck would otherwise be reported as finished.
  if (awaitingFuel && (s.poolCoins || 0) > awaitingFuel.poolAt) awaitingFuel = null;

  const working = (phase && phase !== 'funding' && phase !== 'waiting') || awaitingFuel;
  if (spin) spin.hidden = !working;

  // Mid-bootstrap and already paid: money has arrived and is being spent.
  // Keep the panel up so the sequence is visible, but stop asking.
  if (phase && phase !== 'funding' && phase !== 'waiting') {
    panel.className = 'working';
    title.textContent = phase === 'fuel' ? 'Minting fuel…' : 'Creating generation 0…';
    why.textContent = 'Funded — this takes a few seconds.';
    btn.disabled = true;
    return;
  }

  // Paid, on a deployment that was already running or starved. The coin is in
  // the wallet; the keeper is turning it into the denominated coins the cells
  // spend. About a minute, and nothing else on the page would show it.
  if (awaitingFuel) {
    const waited = Date.now() - awaitingFuel.at;
    panel.className = 'working';
    title.textContent = 'Payment accepted — minting fuel…';
    why.textContent = waited > MINTING_PATIENCE_MS
      ? `${awaitingFuel.satoshis.toLocaleString()} sat credited, but the pool has not grown yet. ` +
        `The keeper mints it in the background; this can lag when the network is slow.`
      : `${awaitingFuel.satoshis.toLocaleString()} sat credited. It has to be minted into the ` +
        `coins cells spend, which takes about a minute — ${s.poolCoins.toLocaleString()} so far.`;
    btn.disabled = false;
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

  // `resting`, rather than no class at all. The other three states each say
  // what they are, and the quiet one needs to as well now that it is the state
  // that collapses to a single line — a stylesheet cannot hang a rule on the
  // absence of every class it does not have without listing them all.
  panel.className = 'resting';
  title.textContent = 'Keep it running';
  why.textContent =
    `Every one of the ${s.cells} cells pays a fee to advance. ${s.poolCoins.toLocaleString()} ` +
    `coins in hand. Anyone can top it up, any time.`;
}

/* The chevron beside the title.
 *
 * Expansion is the viewer's and it sticks: someone who opened the panel to read
 * why it is asking has said they want that on screen, and re-collapsing it under
 * them on the next push — which arrives several times a second — would be the
 * page arguing with them. Need still overrides it in the other direction, since
 * low and stopped are shown by their own class regardless of this attribute. */
const fundToggle = document.getElementById('fundToggle');
function setFundExpanded(open) {
  const panel = document.getElementById('fund');
  if (!panel) return;
  if (open) panel.setAttribute('data-expanded', '');
  else panel.removeAttribute('data-expanded');
  if (fundToggle) fundToggle.setAttribute('aria-expanded', open ? 'true' : 'false');
}
// Bound to the message rather than the chevron: the chevron is a child, so a
// press on it — by finger, mouse or keyboard — arrives here by bubbling, and
// binding both would toggle twice and land back where it started.
const fundMsg = document.getElementById('fundMsg');
if (fundMsg) {
  fundMsg.onclick = () => {
    const panel = document.getElementById('fund');
    // Only where there is something to reveal. low, stopped and working are
    // already showing everything they have.
    if (!panel || !panel.classList.contains('resting')) return;
    setFundExpanded(!panel.hasAttribute('data-expanded'));
  };
}

/** How much headroom counts as out of trouble, as a multiple of `cells`.
 *
 * Entering `low` and leaving it are deliberately different thresholds. Sharing
 * one means the panel changes state every time the pool crosses it, and the
 * pool crosses it constantly: coins are spent a generation at a time and minted
 * in batches, so `poolCoins` oscillates around `cells` for as long as the keeper
 * is keeping up. A generation and a half in hand is the first point at which
 * saying "running low" would be overstating it. */
const LOW_CLEAR_RATIO = 1.5;

/** How many healthy renders it takes to put the warning away.
 *
 * The deadband above handles the pool sawtoothing across the threshold;
 * this handles waitingOnCoin, which is a count of cells retrying RIGHT NOW and
 * can read zero for a single push in the middle of a genuine shortage. Ten
 * renders is about five seconds at engine.PublishInterval.
 *
 * Counted in renders rather than measured on the clock because renders are what
 * actually resize the panel, and because a page whose stream has stopped should
 * hold whatever it last said rather than quietly recovering on a timer while no
 * new evidence arrives. */
const LOW_CLEAR_PUSHES = 10;

/** Whether the panel is currently reporting a fuel shortfall, and how many
 *  consecutive healthy renders have argued for taking that back. */
let lowLatched = false;
let clearRun = 0;

/** Whether the pool can still fund a whole generation — damped.
 *
 * One coin funds one cell transition, so fewer coins than cells means the next
 * generation cannot complete, which is the honest definition of "low" here and
 * is visible well before the engine declares starvation. waitingOnCoin catches
 * the same thing from the other side: cells already retrying a shortfall.
 *
 * That answer is honest and, undamped, unusable. Both inputs move on every push,
 * so the panel flipped between `low` and `resting` several times a second — and
 * those two states are 38px apart in a layout where the diagram's pane absorbs
 * the difference, so every flip reallocated the canvas and jumped the whole
 * diagram. Measured in a browser at 1400x900: the canvas went 472px to 508px and
 * back on a one-coin change.
 *
 * The escalation itself is unchanged. `low` is still entered the moment the
 * engine's own numbers say so, which is what makes it a leading indicator; only
 * the release is damped. */
function isLowOnFuel(s) {
  const short = s.waitingOnCoin > 0 || (s.poolCoins || 0) < s.cells;
  if (short) {
    lowLatched = true;
    clearRun = 0;
    return true;
  }
  if (!lowLatched) return false;
  // Over the line but not clear of it: the next push can put it back, so this
  // is not evidence of anything yet.
  if ((s.poolCoins || 0) < s.cells * LOW_CLEAR_RATIO) {
    clearRun = 0;
    return true;
  }
  if (++clearRun < LOW_CLEAR_PUSHES) return true;
  lowLatched = false;
  clearRun = 0;
  return false;
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
      // Expand first. In the resting state the pay-by-hand block is collapsed,
      // so opening the <details> inside it would put the address and the txid
      // box behind a display:none — an error telling the viewer to pay by hand
      // with no visible way to do it.
      setFundExpanded(true);
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
    // Start showing the mint, from the coin count as it stands right now.
    awaitingFuel = {
      at: Date.now(),
      satoshis: out.satoshis,
      poolAt: (state.snap && state.snap.poolCoins) || 0,
    };
    scheduleRender();
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
  awaitingFuel = {
    at: Date.now(),
    satoshis: out.satoshis,
    poolAt: (state.snap && state.snap.poolCoins) || 0,
  };
  scheduleRender();
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

/** Arcade's status page for a transaction.
 *
 * This function was lost in the move to virtualized history while its call site
 * survived, so every click threw a ReferenceError. Inside an async handler that
 * surfaces as an unhandled rejection — nothing in the page, nothing in the way
 * of feedback, just a cell that advertises itself as clickable and then does
 * nothing at all. */
function arcadeTxURL(txid) {
  const base = (state.snap && state.snap.arcadeUrl) || '';
  return base.replace(/\/+$/, '') + '/tx/' + txid;
}

/** What a cell is, as markup. Shared by the hover tooltip and the touch sheet so
 *  the two cannot drift into describing the same cell differently. */
function cellSummary(hit) {
  const bits = bitsOf(hit.gen, state.snap.cells);
  const cellState = STATE_OF[hit.stateChar] || 'pending';
  return `<b>cell ${hit.index}</b> · generation ${hit.number}<br>` +
    `state: ${bits[hit.index] ? 'alive' : 'dead'} · tx: ${cellState}<br>`;
}

/** Open a transaction in the arcade explorer. */
function openTx(txid) {
  window.open(arcadeTxURL(txid), '_blank', 'noopener');
}

/* --- The touch sheet ------------------------------------------------------ */

const cellsheet = document.getElementById('cellsheet');
const cellsheetBody = document.getElementById('cellsheetBody');
const cellsheetOpen = document.getElementById('cellsheetOpen');
const cellsheetClose = document.getElementById('cellsheetClose');
/** Guards against a slow lookup filling in a sheet that has moved on. */
let sheetToken = 0;

function hideSheet() {
  sheetToken++;
  if (cellsheet) cellsheet.hidden = true;
  if (cellsheetOpen) cellsheetOpen.hidden = true;
}

function showSheet(hit) {
  if (!cellsheet) return;
  const head = cellSummary(hit);
  cellsheetBody.innerHTML = head;
  cellsheet.hidden = false;
  cellsheetOpen.hidden = true;

  if (hit.stateChar === '-') return;
  const token = ++sheetToken;
  detailFor(hit.number, hit.index).then((d) => {
    if (token !== sheetToken || !d) return;
    // The head is markup this file wrote out of two numbers and a value from a
    // fixed table. The id and the refusal reason are strings that arrived over
    // the wire, so they go in as text and not as markup.
    cellsheetBody.innerHTML = head;
    if (d.txid) cellsheetBody.append(d.txid);
    if (d.err) {
      const why = document.createElement('div');
      why.className = 'err';
      why.textContent = d.err;
      cellsheetBody.append(why);
    }
    if (d.txid) {
      cellsheetOpen.hidden = false;
      // Assigned per cell rather than read from a variable at click time: the
      // handler then closes over the id it was shown with, and a sheet that has
      // since been replaced cannot navigate to the wrong transaction.
      cellsheetOpen.onclick = () => openTx(d.txid);
    }
  });
}

if (cellsheetClose) cellsheetClose.onclick = hideSheet;

/* --- Pointers ------------------------------------------------------------- */

/** The gesture in progress, if it might still turn out to be a tap. */
let tapStart = null;
let pointerDown = false;
/** Which kind of device produced the last gesture. See canvas.onclick. */
let lastPointerType = 'mouse';

/** How far a finger may travel and how long it may rest and still be a tap.
 *
 * #canvas-wrap scrolls in both axes, so on a touchscreen every drag over the
 * canvas begins exactly like a tap: the only thing separating "look at this
 * cell" from "pan the diagram" is what happens next. */
const TAP_SLOP_PX = 10;
const TAP_MS = 500;

canvas.onpointerdown = (ev) => {
  lastPointerType = ev.pointerType || 'mouse';
  pointerDown = true;
  tapStart = { x: ev.clientX, y: ev.clientY, t: Date.now() };
};

canvas.onpointerup = (ev) => {
  pointerDown = false;
  const start = tapStart;
  tapStart = null;
  // A mouse keeps the behaviour it has always had, which onclick implements.
  if (!start || (ev.pointerType || 'mouse') === 'mouse') return;
  if (Math.abs(ev.clientX - start.x) > TAP_SLOP_PX ||
      Math.abs(ev.clientY - start.y) > TAP_SLOP_PX ||
      Date.now() - start.t > TAP_MS) {
    return;                       // a pan, or a long press: not a tap
  }

  const hit = cellAt(ev);
  // A tap on empty space is how the sheet is dismissed, so this is not a
  // no-op — it is the other half of the gesture.
  if (!hit) { hideSheet(); return; }
  showSheet(hit);
};

// A touch that becomes a scroll never delivers pointerup at all; the browser
// takes the gesture and cancels the pointer. Without this the abandoned start
// would be matched against whatever pointerup came next.
canvas.onpointercancel = () => { pointerDown = false; tapStart = null; };

canvas.onclick = (ev) => {
  // Touch has already been served by onpointerup, and a browser synthesises a
  // click after a tap regardless — so without this a tap would open the sheet
  // and navigate away from it in the same gesture.
  if (lastPointerType !== 'mouse') return;

  const hit = cellAt(ev);
  if (!hit || hit.stateChar === '-') return;

  // The tab has to be opened INSIDE the click. A browser honours window.open
  // only while the user gesture is still live, and awaiting a lookup outlives
  // it — so this handler is deliberately not async. Hovering populates the
  // cache, which on a pointer device has almost always happened by the time the
  // click lands, and then the open is straightforwardly synchronous.
  const cached = txCache.get(hit.number + ':' + hit.index);
  if (cached !== undefined) {
    if (cached && cached.txid) openTx(cached.txid);
    return;
  }

  // Nothing cached — a click faster than the lookup. Claim the tab now while
  // the gesture still counts and point it at the transaction when the id
  // arrives. Opened without 'noopener' because that form returns null, leaving
  // nothing to navigate; the opener is severed by hand instead, which is the
  // same protection.
  const tab = window.open('', '_blank');
  if (tab) tab.opener = null;
  detailFor(hit.number, hit.index).then((d) => {
    if (!tab) return;
    if (d && d.txid) tab.location = arcadeTxURL(d.txid);
    else tab.close();
  });
};

canvas.onmousemove = (ev) => {
  const hit = cellAt(ev);
  if (!hit) { tip.hidden = true; return; }
  const { number, index: c } = hit;
  canvas.style.cursor = hit.stateChar === '-' ? 'default' : 'pointer';

  const head = cellSummary(hit);

  // The id and the refusal reason are not in the bulk payload any more — that
  // is the trade that made the archive viewable — so they are fetched for the
  // cell under the pointer and cached. Render immediately with what is known,
  // then fill them in: a tooltip that waits on a request is a tooltip that
  // flickers.
  tip.innerHTML = head;
  tip.hidden = false;
  placeTip(ev);

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

/** Put the tooltip beside the pointer, and inside the window.
 *
 * The clamp used to be `window.innerWidth - 440`, which assumes a viewport
 * wider than the tooltip: on anything under 440px it is a negative left, so the
 * box rendered off the left edge entirely. There was no bottom clamp at all, so
 * a pointer low on the screen pushed it below the fold. */
function placeTip(ev) {
  const vw = window.innerWidth || 1024;
  const vh = window.innerHeight || 768;
  const tw = tip.offsetWidth || 320;
  const th = tip.offsetHeight || 80;
  tip.style.left = Math.max(8, Math.min(ev.clientX + 14, vw - tw - 8)) + 'px';
  tip.style.top = Math.max(8, Math.min(ev.clientY + 14, vh - th - 8)) + 'px';
}

canvas.onmouseleave = () => { tip.hidden = true; };
window.addEventListener('resize', scheduleRender);

// Live updates. EventSource reconnects on its own, so a server restart or a
// dropped proxy connection recovers without a page reload.
const events = new EventSource('/api/events');
events.onmessage = (ev) => apply(JSON.parse(ev.data));

/** When the extent was last re-read, and whether a read is outstanding. */
let extentAt = 0;
let extentBusy = false;

/** Read how much history exists, so the spacer — and therefore the scrollbar —
 *  can be as tall as the whole run.
 *
 * Re-readable, not read-once. `extent` sizes the spacer and the spacer IS the
 * scrollbar, so a stale extent is not a cosmetic error: `follow` pins the
 * viewport to the bottom of it, and if that is past the end of the archive then
 * every row is blank and no cell can be clicked, because there is nothing there
 * to draw or to hit-test. A page left open across a restart looked exactly like
 * a dead deployment for precisely this reason.
 *
 * Debounced, since the callers can fire on every frame; `force` is for the
 * restart case, where the current numbers are known to be wrong. */
async function refreshExtent(force) {
  if (extentBusy) return;
  const now = Date.now();
  if (!force && now - extentAt < EXTENT_RECHECK_MS) return;
  extentBusy = true;
  extentAt = now;
  try {
    const res = await fetch('/api/extent');
    if (!res.ok) return;
    const e = await res.json();
    state.extent = e.empty
      ? { oldest: 0, newest: 0, count: 0, empty: true }
      : { oldest: e.oldest, newest: e.newest, count: e.count, empty: false };
    // Anything below the archive's start has been pruned away; drawing it would
    // show history the server can no longer stand behind.
    for (const n of [...state.rows.keys()]) {
      if (n < state.extent.oldest) state.rows.delete(n);
    }
    scheduleRender();
  } catch {
    /* keep what we have; the next scroll asks again */
  } finally {
    extentBusy = false;
  }
}

/** Put generation n at the top of the viewport. */
function jumpTo(n) {
  if (state.extent.empty) return;
  const clamped = Math.min(Math.max(n, state.extent.oldest), state.extent.newest);
  // Someone has said where to look, so the landing is no longer owed; without
  // this it would fire afterwards and throw the view to the live edge.
  openAtLiveEdge = false;
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

refreshExtent(true)
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
    //
    // Recorded as an intent rather than done here, because the spacer has not
    // been sized yet: scrollHeight is still a placeholder until draw() has both
    // the extent and a snapshot, so setting scrollTop now scrolls to the bottom
    // of nothing. This used to be covered by follow being on, which pinned the
    // view a moment later; with follow off by default there is nothing to fall
    // back on and the page opened at generation 0.
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
    draw, jumpTo, windowRows, fitCellPx, isNarrow,
    TAP_SLOP_PX, TAP_MS,
    cellPx: () => cellPx,
    setZoomChosen(v) { zoomChosen = v; },
    // The panel is driven by GET /api/funding, which the harness has no network
    // for. Exposing the setter is cheaper than stubbing fetch well enough to
    // deliver a body, and it is the same field loadFundTarget assigns.
    setFundTarget(t) { fundTarget = t; },
    setExtent(e) { state.extent = e; },
    setFollow(f) { follow = f; },
    setOpenAtLiveEdge(v) { openAtLiveEdge = v; },
    setAwaitingFuel(a) { awaitingFuel = a; },
    // The restart guard is deliberately a wall-clock cooldown — see
    // FORGET_COOLDOWN_MS — so tests that restart a run several times in a row
    // need the page's "this has not happened yet" state back between them.
    resetRestartGuard() { forgotAt = 0; },
    // Same reason: the fuel warning latches across pushes, so a test that wants
    // a panel in no particular state has to say so.
    resetFuelLatch() { lowLatched = false; clearRun = 0; },
    LOW_CLEAR_PUSHES,
    wanted: () => wanted,
    view: () => view,
    canvas,
  };
}
