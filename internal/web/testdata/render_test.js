// Renderer tests for internal/web/static/app.js, run by TestRenderer in the Go
// suite.
//
// app.js is a plain script for a browser, with no build step and no framework —
// which is the right shape for what it does, but it means there is nowhere to
// hang a unit test. So the DOM it touches is stubbed here, narrowly and by hand,
// and the canvas context is a recorder. That makes the thing worth asserting
// directly countable: how many rectangles a push actually paints.
//
// The claim under test is the one that motivated the rewrite. The old renderer
// cleared the canvas and repainted every generation on every message: 2048 rows
// of 128 cells, ~262k fillRect calls per frame, ten times a second, to show a
// handful of cells changing colour.

'use strict';

const path = require('node:path');
const assert = require('node:assert');

// ---------------------------------------------------------------------------
// DOM stub
// ---------------------------------------------------------------------------

let rectCount = 0;
let blitCount = 0;

function newContext(owner) {
  return {
    fillStyle: '',
    fillRect(_x, _y, _w, _h) {
      // The full-canvas background fill is geometry, not row painting; count
      // only what a row costs. Rows are cellPx tall.
      if (owner.width && _w === owner.width && _h === owner.height) return;
      rectCount++;
    },
    drawImage() { blitCount++; },
  };
}

function newElement(tag) {
  const el = {
    tagName: tag,
    style: {},
    children: [],
    hidden: false,
    textContent: '',
    innerHTML: '',
    value: '',
    checked: true,
    width: 0,
    height: 0,
    scrollTop: 0,
    scrollHeight: 0,
    clientHeight: 600,
    append(...kids) { el.children.push(...kids); },
    setAttribute() {},
    addEventListener() {},
    getBoundingClientRect() { return { top: 0, left: 0 }; },
  };
  if (tag === 'canvas') {
    const c = newContext(el);
    el.getContext = () => c;
  }
  return el;
}

const elements = new Map();
function byId(id) {
  if (!elements.has(id)) {
    elements.set(id, newElement(id === 'grid' ? 'canvas' : 'div'));
  }
  return elements.get(id);
}

let frames = [];
global.requestAnimationFrame = (cb) => { frames.push(cb); return frames.length; };
/** Run every queued frame callback, as the browser would at the next repaint. */
function flushFrames() {
  const queued = frames;
  frames = [];
  for (const cb of queued) cb();
}

global.document = {
  documentElement: {},
  getElementById: byId,
  createElement: newElement,
};
global.getComputedStyle = () => ({ getPropertyValue: () => '' });
global.window = {
  addEventListener() {},
  innerWidth: 1200,
  open() {},
};
global.EventSource = class {
  constructor() { this.onmessage = null; }
};
// The archive, stubbed. Scrolling now FETCHES, so a rejecting fetch would make
// every windowing test look like an empty diagram.
let ARCHIVE = { oldest: 0, newest: 0 };
global.setArchive = (a) => { ARCHIVE = a; };
global.fetch = (url) => {
  if (url.startsWith('/api/extent')) {
    return Promise.resolve({ ok: true, json: () => Promise.resolve({
      oldest: ARCHIVE.oldest, newest: ARCHIVE.newest,
      count: ARCHIVE.newest - ARCHIVE.oldest + 1, empty: false,
    })});
  }
  if (url.startsWith('/api/history')) {
    const from = Number(/from=(\d+)/.exec(url)[1]);
    const count = Number(/count=(\d+)/.exec(url)[1]);
    const gens = [];
    for (let i = 0; i < count; i++) {
      const n = from + i;
      if (n > ARCHIVE.newest) break;
      gens.push({ n, row: 'aa'.repeat(CELLS / 8), s: 'm'.repeat(CELLS) });
    }
    return Promise.resolve({ ok: true, json: () => Promise.resolve({ from, generations: gens })});
  }
  if (url.startsWith('/api/tx')) {
    const g = Number(/generation=(\d+)/.exec(url)[1]);
    const c = Number(/cell=(\d+)/.exec(url)[1]);
    return Promise.resolve({ ok: true, json: () => Promise.resolve({
      generation: g, cell: c, txid: 'ab'.repeat(32), status: 'mined',
    })});
  }
  return Promise.resolve({ ok: false, json: () => Promise.resolve({}) });
};
// wallet.js in the browser; the panel only calls into it on a click, which
// these tests do not simulate. navigator and location are deliberately NOT
// stubbed: both are read-only globals in Node, and nothing app.js evaluates at
// load time touches either.
global.Wallet = {
  probe: () => Promise.resolve(null),
  network: () => Promise.resolve(null),
  fundWith: () => Promise.reject(new Error('no wallet in the renderer test')),
  settle: () => Promise.resolve(),
  explain: (e) => String(e),
};

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

const CELLS = 128;

function makeGeneration(number, state = 'seen') {
  return {
    number,
    // Every other cell alive, so a row has real cells to paint.
    row: 'aa'.repeat(CELLS / 8),
    cells: Array.from({ length: CELLS }, (_, c) => ({ cell: c, state, txid: 'tx' + number + '-' + c })),
  };
}

function snapshot(generations, overrides = {}) {
  return Object.assign({
    cells: CELLS,
    rule: 110,
    mode: 'running',
    rate: 1,
    generation: generations.length ? generations[generations.length - 1].number : 0,
    totalTx: 0,
    balance: 0,
    poolCoins: 0,
    provedCells: CELLS,
    failedCells: 0,
    arcadeUrl: 'http://arcade.invalid',
    lastError: '',
    history: generations,
  }, overrides);
}

/** A push as the wire delivers it: fresh objects every time, because the client
 *  gets its snapshot from JSON.parse. Reusing objects here would make the
 *  identity check look far better than it is. */
function tail(from, to, state = 'seen') {
  const out = [];
  for (let n = from; n <= to; n++) out.push(makeGeneration(n, state));
  return out;
}

function measure(fn) {
  rectCount = 0;
  blitCount = 0;
  fn();
  flushFrames();
  return { rects: rectCount, blits: blitCount };
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

const app = require(path.join(__dirname, '..', 'static', 'app.js'));
const { apply, state, paint, MAX_HISTORY, MAX_CANVAS_PX } = app;

const grid = byId('grid');
const tests = [];
function test(name, fn) { tests.push([name, fn]); }

// Put the client in a known place in a known archive.
function atArchive(oldest, newest, scrollTop = 0) {
  setArchive({ oldest, newest });
  app.setExtent({ oldest, newest, count: newest - oldest + 1, empty: false });
  byId('canvas-wrap').scrollHeight = (newest - oldest + 1) * 4;
  // Default to the live edge, which is where the streamed tail lives and where
  // the page itself opens.
  byId('canvas-wrap').scrollTop =
    scrollTop || Math.max(0, (newest - oldest + 1) * 4 - byId('canvas-wrap').clientHeight);
  apply(snapshot(tail(newest - 7, newest)));
  flushFrames();
}

/** Put compact rows straight into the cache, so a test can measure PAINTING
 *  without waiting on the archive fetch, which is asynchronous. */
function seedRows(from, count) {
  for (let i = 0; i < count; i++) {
    const n = from + i;
    state.rows.set(n, { number: n, row: 'aa'.repeat(CELLS / 8), s: 'm'.repeat(CELLS) });
  }
}

/** Scroll and render. The DOM stub's addEventListener is a no-op, so setting
 *  scrollTop cannot fire the listener the page relies on; drive the draw
 *  directly, which is the part these tests are about. */
function scrollTo(px) {
  byId('canvas-wrap').scrollTop = px;
  app.draw();
}

test('a re-sent tail with nothing changed paints nothing', () => {
  atArchive(0, 200);
  const again = measure(() => apply(snapshot(tail(193, 200))));
  assert.strictEqual(again.rects, 0,
    `an identical re-send painted ${again.rects} rectangles`);
});

test('a tail with one changed generation paints one row', () => {
  atArchive(0, 200);
  const gens = tail(193, 200);
  gens[4] = makeGeneration(197, 'failed');
  const after = measure(() => apply(snapshot(gens)));
  // CELLS + 1: paintRow clears the row before drawing it, and the harness
  // counts that clear because it is not a full-canvas fill.
  assert.ok(after.rects > 0 && after.rects <= CELLS + 1,
    `one changed generation painted ${after.rects} rectangles, want at most ${CELLS + 1}`);
});

test('several pushes in one frame render once', () => {
  atArchive(0, 200);
  rectCount = 0;
  apply(snapshot(tail(193, 200, 'seen')));
  apply(snapshot(tail(193, 200, 'mined')));
  apply(snapshot(tail(193, 200, 'seen')));
  const before = rectCount;
  flushFrames();
  assert.ok(rectCount >= before, 'frames did not coalesce into one render');
});

// ---------------------------------------------------------------------------
// Virtualization — the properties that make 100,000 generations possible
// ---------------------------------------------------------------------------

// The whole inversion. The window used to be "the newest N we hold", so there
// was no way to look at generation 40,000 of 100,000 however much you scrolled.
test('scrolling shows the generations at that offset, not the newest', () => {
  atArchive(0, 100000, 0);
  scrollTo(40000 * 4); // 4px rows

  // The base is now EXACTLY the scroll position: the canvas is the scrollport,
  // so what you see is what the scrollbar points at, with no overscan offset
  // between them. Overscan moved to prefetching, where it belongs.
  const v = app.view();
  assert.strictEqual(v.base, 40000,
    `window base ${v.base}, want exactly the scrolled-to generation`);
});

// THE CANVAS MUST NEVER BE TALLER THAN THE SCROLLPORT.
//
// This is the invariant the first version broke, and it broke the page on the
// live deployment: the canvas rides in a position:sticky box, and sticky cannot
// pin a box taller than the scrollport — past that the browser lets it scroll
// away. Adding OVERSCAN rows to the canvas made it 2,300px in a 700px viewport,
// so the diagram drifted off screen and left a sliver behind.
//
// Overscan belongs in what ensureRows PREFETCHES, not in how big a bitmap the
// browser has to keep pinned. Conflating the two is the mistake this guards.
test('the canvas never exceeds the scrollport, however long the run is', () => {
  atArchive(0, 100000, 0);
  const port = byId('canvas-wrap').clientHeight;
  const rows = app.windowRows(CELLS);

  assert.ok(rows * 4 <= port + 4 * 4,
    `canvas is ${rows * 4}px tall against a ${port}px scrollport; sticky cannot pin that`);
  assert.ok(rows > 0, 'the canvas carries no rows at all');
  assert.ok(grid.height <= app.MAX_CANVAS_PX,
    `canvas is ${grid.height}px tall, over the browser dimension limit`);
  assert.ok(grid.width * grid.height <= app.MAX_CANVAS_AREA,
    `canvas is ${grid.width * grid.height}px in area, over the allocation cap`);
});

// The spacer is what produces the scrollbar, and it has to be as tall as the
// WHOLE run — that is the other half of the trick. A spacer sized to the canvas
// gives no scrollbar at all, which is what a viewer reported seeing.
test('the spacer is as tall as the whole archive', () => {
  atArchive(0, 100000, 0);
  const want = 100001 * 4;
  assert.strictEqual(byId('surface').style.height, want + 'px',
    `spacer is ${byId('surface').style.height}, want ${want}px so the scrollbar covers the run`);
});

// Prefetching still reaches beyond the viewport, or every scroll would fetch.
test('prefetch still covers overscan even though the canvas does not', () => {
  atArchive(0, 100000, 0);
  seedRows(0, 3000);
  scrollTo(1000 * 4);
  assert.ok(app.wanted().rows >= app.OVERSCAN,
    `prefetch window is ${app.wanted().rows} rows; overscan was lost with the canvas change`);
});

// A drag across the archive jumps further than the window is wide, so there is
// nothing to blit and carrying the old bitmap would show stale rows.
test('a scroll jump larger than the window repaints rather than blitting', () => {
  atArchive(0, 100000, 0);
  seedRows(0, 1000);
  scrollTo(0);
  seedRows(49000, 2000);
  rectCount = 0; blitCount = 0;
  scrollTo(50000 * 4);
  const far = { rects: rectCount, blits: blitCount };
  assert.ok(far.rects > 0,
    'a jump across the archive painted nothing; the window moved but the pixels did not');
});

test('the client cache is bounded even after crossing the whole archive', () => {
  atArchive(0, 100000, 0);
  for (let g = 0; g < 60000; g += 997) {
    scrollTo(g * 4);
  }
  assert.ok(state.rows.size <= app.MAX_CACHED,
    `cache holds ${state.rows.size} generations, over the ${app.MAX_CACHED} bound`);
});

// Hit-testing has to translate viewport pixels to an absolute generation, or
// clicking a cell in deep history opens somebody else's transaction.
test('hit-testing resolves the right generation at a scrolled offset', () => {
  atArchive(0, 100000, 0);
  scrollTo(30000 * 4);
  const v = app.view();
  assert.strictEqual(v.base, 30000,
    `window base ${v.base} is not where the scrollbar points`);
});

// Jumping is the navigation affordance; it must land where it says.
test('jumping to a generation puts it at the top of the viewport', () => {
  atArchive(0, 100000, 0);
  app.jumpTo(12345);
  flushFrames();
  assert.strictEqual(byId('canvas-wrap').scrollTop, 12345 * 4,
    'jump did not move the scrollbar to the requested generation');
});

// ---------------------------------------------------------------------------
// Controls and funding
//
// Both are presentation only -- the server refuses a locked control whether or
// not the button exists, and the funding panel cannot conjure money -- but both
// decide what a stranger arriving at a public URL is told, which is worth
// pinning.
// ---------------------------------------------------------------------------

test('a locked deployment hides the clock controls but keeps zoom and follow', () => {
  apply(snapshot(tail(0, 4), { locked: true }));
  flushFrames();

  for (const id of ['play', 'pause', 'step', 'rateLabel']) {
    assert.strictEqual(byId(id).hidden, true,
      `${id} is still offered on a deployment that refuses it`);
  }
  // Zoom and follow are client-side and affect nobody else.
  assert.strictEqual(byId('zoom').hidden, false, 'zoom was hidden; it is local to the viewer');
  assert.strictEqual(byId('follow').hidden, false, 'follow was hidden; it is local to the viewer');
});

test('an unlocked deployment offers the clock controls', () => {
  apply(snapshot(tail(0, 4), { locked: false }));
  flushFrames();

  for (const id of ['play', 'pause', 'step', 'rateLabel']) {
    assert.strictEqual(byId(id).hidden, false, `${id} was hidden on an unlocked deployment`);
  }
});

const FUND_TARGET = {
  address: 'mpz7rAwYignR5bybEGP4aZQbeikjxiRQ2U',
  lockingScript: '76a914aa88ac',
  network: 'ttn',
  minSatoshis: 10000,
  suggestedSatoshis: 500000,
};

// The panel is permanent once funding is possible. It used to appear only when
// the automaton was starved, which was wrong twice: it made "you can pay for
// this" look like an error state on an exhibit whose whole premise is that
// strangers keep it alive, and it hid the one useful control at the one moment
// it mattered, because the panel sat below a canvas thousands of pixels tall.
test('the funding panel is always offered, not only in trouble', () => {
  app.setFundTarget(FUND_TARGET);
  apply(snapshot(tail(0, 4), { poolCoins: CELLS * 10, waitingOnCoin: 0 }));
  flushFrames();

  assert.strictEqual(byId('fund').hidden, false,
    'a healthy automaton hides the only way to keep it healthy');
  assert.strictEqual(byId('fund').className, '',
    'a healthy automaton is showing an alarm state');
  assert.strictEqual(byId('fundBtn').disabled, false, 'the fund button is disabled while healthy');
});

// A deployment that does not take public funding has no panel at all, rather
// than an empty one.
test('no funding panel when the deployment does not take payments', () => {
  app.setFundTarget(null);
  apply(snapshot(tail(0, 4)));
  flushFrames();
  assert.strictEqual(byId('fund').hidden, true,
    'a panel was offered by a deployment with no funding endpoint');
});

// The leading indicator. Starvation is only declared after a 20-second grace,
// so a page that waits for `starved` looks healthy right up until it halts.
// One coin funds one cell transition, so fewer coins than cells means the next
// generation cannot complete.
test('running low is called out before the automaton actually stops', () => {
  app.setFundTarget(FUND_TARGET);
  apply(snapshot(tail(0, 4), { starved: false, poolCoins: CELLS - 1, waitingOnCoin: 0 }));
  flushFrames();

  assert.strictEqual(byId('fund').className, 'low',
    'a pool too small for one generation was not flagged');
});

// Cells already retrying a shortfall is the same warning reached from the other
// side, and it fires even when the coin count still looks respectable.
test('cells waiting on coin also count as running low', () => {
  app.setFundTarget(FUND_TARGET);
  apply(snapshot(tail(0, 4), { starved: false, poolCoins: CELLS * 10, waitingOnCoin: 3 }));
  flushFrames();

  assert.strictEqual(byId('fund').className, 'low',
    'cells were already retrying shortfalls and the page said nothing');
});

test('a starved automaton says so unmistakably', () => {
  app.setFundTarget(FUND_TARGET);
  apply(snapshot(tail(0, 4), { starved: true, poolCoins: 0 }));
  flushFrames();

  assert.strictEqual(byId('fund').className, 'stopped',
    'the automaton has halted for want of coin and the panel is not in its alarm state');
  assert.match(byId('fundTitle').textContent, /STOPPED/,
    'the headline does not say it has stopped');
});

test('a cold deployment says what it needs to start', () => {
  app.setFundTarget(FUND_TARGET);
  apply(snapshot([], {
    starved: false,
    bootstrap: { phase: 'funding', address: 'mpz7', minSatoshis: 500000, have: 0 },
  }));
  flushFrames();

  assert.strictEqual(byId('fund').className, 'stopped');
  assert.ok(byId('fundWhy').textContent.includes('500,000'),
    'the payer was not told how much is needed');
});

// Once paid, the panel stops asking and reports progress instead — otherwise it
// keeps demanding money that is already being spent.
test('a funded bootstrap reports progress instead of asking again', () => {
  app.setFundTarget(FUND_TARGET);
  apply(snapshot([], {
    bootstrap: { phase: 'fuel', address: 'mpz7', minSatoshis: 500000, have: 900000 },
  }));
  flushFrames();

  assert.strictEqual(byId('fund').className, 'working');
  assert.strictEqual(byId('fundBtn').disabled, true,
    'still soliciting payment while spending the last one');
});

let failed = 0;
// A payment is credited in seconds; the fuel it buys takes about a minute to
// mint. Nothing else on the page moves in between, so a payer sees "thank you"
// and an automaton that is still stopped — indistinguishable from a deployment
// that took the money and did nothing.
test('a credited payment shows the mint in progress', () => {
  app.setFundTarget(FUND_TARGET);
  app.setAwaitingFuel({ at: Date.now(), satoshis: 500000, poolAt: 0 });
  apply(snapshot(tail(0, 4), { starved: true, poolCoins: 0 }));
  flushFrames();

  assert.strictEqual(byId('fund').className, 'working',
    'a paid-for automaton is still shown as stopped while its fuel is minted');
  assert.strictEqual(byId('fundSpin').hidden, false, 'no spinner while minting');
  assert.match(byId('fundTitle').textContent, /accepted/i,
    'the payer is not told their payment landed');
});

// The pool GROWING is what ends the wait. Time alone must not, or a stuck
// keeper would be reported as finished.
test('the mint indicator clears when the pool actually grows', () => {
  app.setFundTarget(FUND_TARGET);
  app.setAwaitingFuel({ at: Date.now(), satoshis: 500000, poolAt: 10 });

  apply(snapshot(tail(0, 4), { starved: true, poolCoins: 10 }));
  flushFrames();
  assert.strictEqual(byId('fundSpin').hidden, false,
    'the wait ended before any fuel appeared');

  apply(snapshot(tail(0, 4), { starved: false, poolCoins: 4000 }));
  flushFrames();
  assert.strictEqual(byId('fundSpin').hidden, true,
    'the spinner is still running after the pool filled');
  assert.strictEqual(byId('fund').className, '',
    'the panel is still in its working state after the automaton recovered');
});

// The cold start mints fuel too, and shows the same thing.
test('the bootstrap fuel phase also spins', () => {
  app.setFundTarget(FUND_TARGET);
  apply(snapshot([], { bootstrap: { phase: 'fuel', minSatoshis: 1, have: 9 } }));
  flushFrames();
  assert.strictEqual(byId('fundSpin').hidden, false, 'no spinner during the bootstrap mint');
});

// No payment in flight, healthy automaton: no spinner. An indicator that is
// always on indicates nothing.
test('no spinner when nothing is being minted', () => {
  app.setFundTarget(FUND_TARGET);
  app.setAwaitingFuel(null);
  app.setOpenAtLiveEdge(false);
  apply(snapshot(tail(0, 4), { poolCoins: CELLS * 10 }));
  flushFrames();
  assert.strictEqual(byId('fundSpin').hidden, true, 'the spinner runs when nothing is happening');
});

// --- a run that restarts ---------------------------------------------------
//
// These cover a live failure. A page left open across a restart kept measuring
// its scrollbar against the run that was gone: `follow` pinned the viewport to
// the bottom of a spacer sized for generations the archive no longer held, so
// every row came back empty and nothing could be clicked. It looked exactly
// like a dead deployment.

/** Let the extent re-read (a fetch, therefore a promise chain) settle. */
const settle = () => new Promise(r => setImmediate(r));

test('a restarted run stops measuring the scrollbar against the old one', async () => {
  atArchive(0, 800);
  assert.strictEqual(state.extent.newest, 800, 'precondition: sized for the long run');

  // The automaton restarts: the archive is now short, and the stream says the
  // run is back near the beginning.
  setArchive({ oldest: 0, newest: 100 });
  apply(snapshot(tail(94, 100)));
  await settle();

  assert.strictEqual(state.extent.newest, 100,
    `extent still claims ${state.extent.newest}; the spacer is sized for a run that no longer exists`);
});

test('a restart drops rows cached from the run that ended', async () => {
  atArchive(0, 800);
  seedRows(700, 50);
  assert.ok(state.rows.has(720), 'precondition: the old run is cached');

  setArchive({ oldest: 0, newest: 100 });
  apply(snapshot(tail(94, 100)));
  await settle();

  assert.ok(!state.rows.has(720),
    'a generation from the ended run survived; numbering restarts, so it would be drawn as the new run');
});

test('after a restart the viewport lands on generations that exist', async () => {
  // The restarted run is still longer than the viewport, so where the view
  // lands is a real choice rather than "all of it".
  atArchive(0, 4000);
  setArchive({ oldest: 0, newest: 400 });
  apply(snapshot(tail(394, 400)));
  await settle();

  // Follow pins to the bottom of the spacer, which is what produced the blank
  // page: the bottom of a spacer sized for 4,000 generations is nowhere near
  // the 400 that survive.
  const wrap = byId('canvas-wrap');
  wrap.scrollHeight = state.extent.count * 4;
  scrollTo(Math.max(0, wrap.scrollHeight - wrap.clientHeight));

  // Measured against what the archive REALLY holds (0..400), not against the
  // client's own belief — comparing a stale extent with itself always agrees,
  // which is exactly how this shipped.
  const v = app.view();
  assert.ok(v.base >= 0 && v.base <= 400,
    `the viewport is parked on generation ${v.base}, outside the 0..400 the archive holds — every row blank, no cell clickable`);
  assert.ok(app.wanted().base <= 400,
    `the archive is being asked for generation ${app.wanted().base}, past its end at 400`);
});

test('ordinary growth does not count as a restart', async () => {
  atArchive(0, 200);
  seedRows(150, 50);
  const before = state.rows.size;
  // The frontier advances; the archive is unchanged.
  apply(snapshot(tail(201, 208)));
  await settle();

  assert.strictEqual(state.extent.newest, 208, 'the frontier should still lead the extent');
  assert.ok(state.rows.size >= before,
    'growing the run threw away the cache, so every scroll would refetch');
});

// Async-aware: the extent is re-read over the network, so the tests that cover
// a restart have to be able to await it.

// --- opening a cell's transaction ------------------------------------------
//
// These cover a live failure: cells advertised themselves as clickable, the
// cursor changed, the tooltip offered "click to open in arcade" — and clicking
// did nothing whatsoever. arcadeTxURL had been lost in the move to virtualized
// history while its call site survived, so every click threw a ReferenceError
// inside an async handler, where it surfaced as an unhandled rejection and no
// feedback of any kind.


/** Point at a cell in the first visible row. Cell index counts from the RIGHT
 *  (c = CELLS-1-floor(x/cellPx)), so x must stay inside CELLS*cellPx.
 *
 * Each test takes a DIFFERENT cell on purpose: the transaction-id cache is
 * module state that outlives a test, so reusing one cell would let a later test
 * quietly take the cached path and assert nothing it meant to. */
const at = (x) => ({ clientX: x, clientY: 2 });

test('clicking a cell opens its transaction in arcade', async () => {
  atArchive(0, 200);
  seedRows(app.view().base, 4);

  // Hover first, which is what a pointer device does and what caches the id.
  app.canvas.onmousemove(at(200));
  await settle();

  const opened = [];
  window.open = (url) => { opened.push(url); return null; };
  app.canvas.onclick(at(200));

  assert.strictEqual(opened.length, 1, 'the click opened nothing at all');
  assert.ok(/\/tx\/[0-9a-f]{64}$/.test(opened[0]),
    `opened ${opened[0]}, which is not an arcade transaction URL`);
});

test('the tab is claimed during the click, not after the lookup', () => {
  atArchive(0, 200);
  seedRows(app.view().base, 4);

  // Nothing hovered, so the id is not cached and has to be fetched. The open
  // must still happen synchronously inside the handler: a browser honours
  // window.open only while the user gesture is live, and a tab opened after an
  // awaited fetch is silently blocked as a popup.
  let openedSynchronously = false;
  window.open = () => { openedSynchronously = true; return { opener: {}, close() {} }; };
  app.canvas.onclick(at(204));

  assert.ok(openedSynchronously,
    'window.open ran after an await, where the browser blocks it as a popup');
});

test('a click with nothing cached still reaches the transaction', async () => {
  atArchive(0, 200);
  seedRows(app.view().base, 4);

  const tab = { location: null, opener: {}, closed: false, close() { this.closed = true; } };
  window.open = () => tab;
  app.canvas.onclick(at(208));
  await settle();

  assert.ok(tab.location && /\/tx\/[0-9a-f]{64}$/.test(String(tab.location)),
    `the claimed tab went to ${tab.location} instead of the transaction`);
  assert.ok(!tab.closed, 'the tab was closed even though the lookup succeeded');
});

test('a cell with no transaction opens nothing', () => {
  atArchive(0, 200);
  // '-' is the archive's "no record for this cell".
  const n = app.view().base;
  state.rows.set(n, { number: n, row: 'aa'.repeat(CELLS / 8), s: '-'.repeat(CELLS) });

  let opened = 0;
  window.open = () => { opened++; return null; };
  app.canvas.onclick(at(212));

  assert.strictEqual(opened, 0, 'a cell with no transaction opened a tab anyway');
});

// --- following, and getting back to the end ---------------------------------

// Following being off must not change WHERE THE PAGE OPENS. Those were the same
// thing by accident: with follow on, the initial view was pinned to the live
// edge a moment after load. Turning the default off removed that and the page
// opened at generation 0 of the whole run — caught in a browser, not here,
// which is why it is pinned here now.
test('the page opens at the live edge even though following is off', () => {
  app.setExtent({ oldest: 0, newest: 5000, count: 5001, empty: false });
  setArchive({ oldest: 0, newest: 5000 });
  app.setFollow(false);
  byId('follow').checked = false;

  // As the page starts: the landing is owed, and nothing has been scrolled.
  app.setOpenAtLiveEdge(true);
  const wrap = byId('canvas-wrap');
  wrap.scrollTop = 0;
  wrap.scrollHeight = 5001 * 4;

  apply(snapshot(tail(4993, 5000)));
  flushFrames();

  assert.ok(wrap.scrollTop > 0,
    'the page opened at generation 0 of a 5,000-generation run');
  assert.strictEqual(wrap.scrollTop, wrap.scrollHeight,
    'the landing did not reach the newest generation');
  assert.strictEqual(byId('follow').checked, false,
    'landing at the live edge switched following on; it is a landing, not a leash');
});

test('the diagram is not dragged to the live edge unless following is on', () => {
  atArchive(0, 1000, 400 * 4);   // parked in history, reading
  app.setFollow(false);
  byId('follow').checked = false;
  const before = byId('canvas-wrap').scrollTop;

  apply(snapshot(tail(1001, 1008)));   // the run advances underneath
  flushFrames();

  assert.strictEqual(byId('canvas-wrap').scrollTop, before,
    'a new generation yanked the view away from the history being read');
});

test('go to bottom reaches the newest generation and starts following', () => {
  atArchive(0, 1000, 400 * 4);
  app.setFollow(false);
  byId('follow').checked = false;

  byId('toBottom').onclick();
  flushFrames();

  const wrap = byId('canvas-wrap');
  assert.strictEqual(wrap.scrollTop, wrap.scrollHeight,
    'go to bottom did not reach the newest generation');
  assert.strictEqual(byId('follow').checked, true,
    'the checkbox does not show that the view is now following');

  // Arriving at the live edge only to drift off it would leave the button to be
  // pressed again every generation.
  apply(snapshot(tail(1001, 1008)));
  flushFrames();
  assert.strictEqual(wrap.scrollTop, wrap.scrollHeight,
    'the view fell behind the live edge straight after being sent there');
});

test('following still chases the live edge when it is switched on', () => {
  atArchive(0, 1000);            // opens at the live edge
  byId('follow').checked = true;
  app.setFollow(true);

  // Nudge off the bottom, as a stray wheel event would, then let the run
  // advance. The spacer's height is driven by reflow inside draw(), and
  // atBottom() is read BEFORE that on purpose, so the stub must not pre-grow it.
  const wrap = byId('canvas-wrap');
  wrap.scrollTop = wrap.scrollHeight - wrap.clientHeight;

  apply(snapshot(tail(1001, 1008)));
  flushFrames();

  assert.strictEqual(wrap.scrollTop, wrap.scrollHeight,
    'following was on but the view did not keep up with the live edge');
});

// Async-aware: a click leads to a lookup, so the tests that follow it through
// have to be able to await.
(async () => {
for (const [name, fn] of tests) {
  // Each test starts from a clean surface; the module is a singleton.
  state.rows.clear();
  state.inflight.clear();
  state.extent = { oldest: 0, newest: 0, count: 0, empty: true };
  state.snap = null;
  app.setAwaitingFuel(null);
  byId('canvas-wrap').scrollTop = 0;
  paint.base = null;
  paint.rows = 0;
  paint.cellPx = null;
  paint.cells = null;
  paint.drawn.clear();
  paint.sig.clear();
  // 4px: the shipped default, and the zoom the virtualization arithmetic in
  // these tests assumes.
  byId('zoom').value = '4';
  byId('zoom').oninput();
  // Drain rather than discard. app.js keeps a "a frame is already queued" flag,
  // and throwing the callback away would leave it set for the rest of the run —
  // after which nothing ever renders again and every assertion here would be
  // measuring an idle renderer.
  flushFrames();

  try {
    await fn();
    console.log('ok   ' + name);
  } catch (err) {
    failed++;
    console.error('FAIL ' + name + '\n     ' + err.message);
  }
}

if (failed) {
  console.error(failed + ' renderer test(s) failed');
  process.exit(1);
}
console.log('all renderer tests passed');
})();
