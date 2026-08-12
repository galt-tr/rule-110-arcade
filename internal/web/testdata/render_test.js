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
    clientHeight: 0,
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
global.fetch = () => Promise.reject(new Error('no network in the renderer test'));
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

test('the first snapshot paints every generation it carries', () => {
  const first = measure(() => apply(snapshot(tail(0, 99))));
  assert.ok(first.rects > 100 * 60,
    `a cold start must paint all 100 rows, saw ${first.rects} rectangles`);
  assert.strictEqual(state.history.length, 100);
});

test('a re-sent tail with nothing changed paints nothing', () => {
  apply(snapshot(tail(0, 99)));
  flushFrames();

  // Exactly what the server does ten times a second: the same 48 generations
  // again, as new objects, with identical contents.
  const again = measure(() => apply(snapshot(tail(52, 99))));
  assert.strictEqual(again.rects, 0,
    `an unchanged tail must paint nothing, saw ${again.rects} rectangles`);
});

test('a tail with one changed generation paints one row', () => {
  apply(snapshot(tail(0, 99)));
  flushFrames();

  const changed = tail(52, 99);
  changed[10].cells[3].state = 'mined'; // generation 62, one cell
  const push = measure(() => apply(snapshot(changed)));

  assert.ok(push.rects > 0, 'the changed row must be painted');
  // One row is one clear plus at most one rect per cell.
  assert.ok(push.rects <= CELLS + 1,
    `one changed generation must cost one row, saw ${push.rects} rectangles`);
});

test('a new generation paints one row, not the whole diagram', () => {
  apply(snapshot(tail(0, 99)));
  flushFrames();

  const push = measure(() => apply(snapshot(tail(52, 100))));
  assert.ok(push.rects <= CELLS + 1,
    `appending a generation must cost one row, saw ${push.rects} rectangles`);
  assert.strictEqual(state.history.length, 101);
});

test('several pushes in one frame render once', () => {
  apply(snapshot(tail(0, 99)));
  flushFrames();

  rectCount = 0;
  // Three pushes, no frame in between — the display cannot show more than one.
  apply(snapshot(tail(52, 100)));
  apply(snapshot(tail(52, 101)));
  apply(snapshot(tail(52, 102)));
  assert.strictEqual(rectCount, 0, 'nothing may be painted before the frame runs');
  assert.strictEqual(frames.length, 1, `three pushes queued ${frames.length} frames, want 1`);

  flushFrames();
  assert.ok(rectCount > 0 && rectCount <= 3 * (CELLS + 1),
    `one frame must paint only the new rows, saw ${rectCount} rectangles`);
});

test('client history is bounded', () => {
  // Well past the cap, in tail-sized pushes as the server sends them.
  for (let n = 0; n <= MAX_HISTORY + 500; n += 48) {
    apply(snapshot(tail(n, Math.min(n + 47, MAX_HISTORY + 500))));
  }
  flushFrames();

  assert.ok(state.history.length <= MAX_HISTORY + 64,
    `history grew to ${state.history.length}, past the ${MAX_HISTORY} bound`);
  // And the newest generation is still the one we hold.
  const newest = state.history[state.history.length - 1].number;
  assert.strictEqual(newest, MAX_HISTORY + 500);
});

test('the canvas never exceeds the browser dimension limit', () => {
  for (let n = 0; n <= MAX_HISTORY + 200; n += 48) {
    apply(snapshot(tail(n, Math.min(n + 47, MAX_HISTORY + 200))));
  }
  flushFrames();

  // Zoomed all the way in, which is where the old renderer went blank: 2048
  // generations at 16px is 32768px, one over the limit.
  byId('zoom').value = '16';
  byId('zoom').oninput();
  flushFrames();

  assert.ok(grid.height <= MAX_CANVAS_PX,
    `canvas height ${grid.height} exceeds the ${MAX_CANVAS_PX} ceiling`);
  assert.ok(grid.height > 0, 'the canvas must not collapse');
  assert.ok(paint.rows > 0, 'rows must still be rendered at maximum zoom');
});

test('scrolling the window forgets the rows that left it', () => {
  for (let n = 0; n <= MAX_HISTORY + 200; n += 48) {
    apply(snapshot(tail(n, Math.min(n + 47, MAX_HISTORY + 200))));
  }
  flushFrames();

  const base = paint.base;
  for (const number of paint.drawn.keys()) {
    assert.ok(number >= base && number < base + paint.rows,
      `generation ${number} is remembered as painted but is outside the window`);
  }
  assert.strictEqual(paint.drawn.size, paint.sig.size,
    'the identity and signature caches must be pruned together');
});

// ---------------------------------------------------------------------------

let failed = 0;
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

for (const [name, fn] of tests) {
  // Each test starts from a clean surface; the module is a singleton.
  state.history.length = 0;
  state.index.clear();
  state.snap = null;
  paint.base = null;
  paint.rows = 0;
  paint.cellPx = null;
  paint.cells = null;
  paint.drawn.clear();
  paint.sig.clear();
  byId('zoom').value = '8';
  byId('zoom').oninput();
  // Drain rather than discard. app.js keeps a "a frame is already queued" flag,
  // and throwing the callback away would leave it set for the rest of the run —
  // after which nothing ever renders again and every assertion here would be
  // measuring an idle renderer.
  flushFrames();

  try {
    fn();
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
