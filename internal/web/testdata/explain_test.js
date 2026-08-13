// Tests for the explainer dialogs, internal/web/static/explain.js, run by
// TestExplainers in the Go suite.
//
// The DOM is stubbed by hand here for the same reason render_test.js does it:
// the file under test is a plain browser script with no build step. This stub
// is a little richer than the renderer's — it needs createElement/appendChild
// for the truth table and a <dialog> that records showModal/close — and it is
// deliberately its own file rather than a shared harness, so that a change to
// one set of tests cannot quietly alter the environment of the other.
//
// The load-bearing assertion is the last one: the demo automaton is driven
// through all eight neighbourhoods and checked against the canonical Rule 110
// truth table, independently of anything explain.js defines. That is the same
// table contracts/Cell_test.go checks the on-chain contract against, so the
// picture on the page cannot drift away from the thing the chain is proving.

'use strict';

const path = require('node:path');
const assert = require('node:assert');

// ---------------------------------------------------------------------------
// DOM stub
// ---------------------------------------------------------------------------

function newContext() {
  return {
    fillStyle: '',
    fillRect() {},
  };
}

function newElement(tag) {
  const el = {
    tagName: tag,
    style: {},
    children: [],
    hidden: false,
    open: false,
    textContent: '',
    innerHTML: '',
    className: '',
    value: '',
    max: '',
    checked: false,
    type: '',
    width: 0,
    height: 0,
    clientWidth: 640,
    listeners: {},
    modalCount: 0,
    closeCount: 0,
    appendChild(kid) { el.children.push(kid); return kid; },
    append(...kids) { el.children.push(...kids); },
    setAttribute(k, v) { el[k] = v; },
    addEventListener(name, fn) { (el.listeners[name] ||= []).push(fn); },
    dispatch(name, ev) { (el.listeners[name] || []).forEach((fn) => fn(ev || {})); },
    getBoundingClientRect() { return { top: 0, left: 0, right: 100, bottom: 100 }; },
    showModal() { el.open = true; el.modalCount++; },
    close() { el.open = false; el.closeCount++; el.dispatch('close'); },
  };
  if (tag === 'canvas') {
    const c = newContext();
    el.getContext = () => c;
  }
  // A created element becomes reachable by getElementById once it has an id.
  // The real DOM waits for insertion; nothing here creates an element it does
  // not then append, so registering on assignment is close enough and saves
  // the stub from having to model a tree.
  let id = '';
  Object.defineProperty(el, 'id', {
    get() { return id; },
    set(v) { id = v; elements.set(v, el); },
  });
  return el;
}

const elements = new Map();
function byId(id) {
  if (!elements.has(id)) {
    const tag = id === 'rulePattern' ? 'canvas'
      : (id === 'dlgRule110' || id === 'dlgScript') ? 'dialog' : 'div';
    elements.set(id, newElement(tag));
  }
  return elements.get(id);
}

// The N-substitution targets. explain.js looks these up by class name.
const byClass = new Map();
function classList(cls) {
  if (!byClass.has(cls)) byClass.set(cls, [newElement('span'), newElement('span')]);
  return byClass.get(cls);
}

global.document = {
  documentElement: {},
  getElementById: byId,
  createElement: newElement,
  getElementsByClassName: classList,
};
global.getComputedStyle = () => ({ getPropertyValue: () => '' });

let FETCHED = null;
global.fetch = (url) => {
  if (url.startsWith('/api/state') && FETCHED) {
    return Promise.resolve({ ok: true, json: () => Promise.resolve(FETCHED) });
  }
  return Promise.resolve({ ok: false, json: () => Promise.resolve({}) });
};

// location and history are left alone: location is not writable in Node, and
// explain.js is written to cope with both being absent for exactly this reason.

const explain = require(path.join(__dirname, '..', 'static', 'explain.js'));

// ---------------------------------------------------------------------------
// Test registration
// ---------------------------------------------------------------------------

const tests = [];
function test(name, fn) { tests.push([name, fn]); }

// ---------------------------------------------------------------------------
// The automaton
// ---------------------------------------------------------------------------

// Canonical Rule 110, neighbourhood (l,c,r) -> next. Written out rather than
// derived, so a bug in the derivation cannot agree with itself.
const RULE110 = {
  '111': 0,
  '110': 1,
  '101': 1,
  '100': 0,
  '011': 1,
  '010': 1,
  '001': 1,
  '000': 0,
};

test('the demo automaton reproduces the Rule 110 truth table', () => {
  // A ring of 8 puts a whole neighbourhood well inside the row. Cell 1's left
  // neighbour is cell 2 and its right neighbour is cell 0, matching
  // internal/ca/ca.go — the convention explain.js documents and uses.
  for (const [nbhd, expect] of Object.entries(RULE110)) {
    const [l, c, r] = nbhd.split('').map(Number);
    const row = new Uint8Array(8);
    row[2] = l;
    row[1] = c;
    row[0] = r;
    const next = explain.stepRow(row, 110);
    assert.strictEqual(next[1], expect,
      `neighbourhood ${nbhd} should give ${expect}, got ${next[1]}`);
  }
});

test('every rule number is its own truth table', () => {
  // The claim the demo makes: changing a bit of the rule number changes exactly
  // one answer. Checked across all 256 rules for one neighbourhood.
  for (let rule = 0; rule < 256; rule++) {
    const row = new Uint8Array(8);
    row[2] = 1; row[1] = 1; row[0] = 0; // neighbourhood 110 -> bit 6
    assert.strictEqual(explain.stepRow(row, rule)[1], (rule >> 6) & 1,
      `rule ${rule}: neighbourhood 110 should read bit 6`);
  }
});

test('the ring wraps at both ends', () => {
  // Cell 0's right neighbour is the last cell. Light only that one: (0,0,1),
  // which Rule 110 maps to 1.
  const a = new Uint8Array(16);
  a[15] = 1;
  assert.strictEqual(explain.stepRow(a, 110)[0], 1,
    'cell 0 did not see the last cell as its right neighbour');

  // The last cell's left neighbour is cell 0. Light only cell 0: (1,0,0) -> 0.
  const b = new Uint8Array(16);
  b[0] = 1;
  assert.strictEqual(explain.stepRow(b, 110)[15], 0,
    'the last cell did not see cell 0 as its left neighbour');
});

// ---------------------------------------------------------------------------
// Bit extraction — the same arithmetic the locking script performs
// ---------------------------------------------------------------------------

test('the bit widget agrees with a reference over a whole ring', () => {
  const cells = 256;
  const row = explain.sampleRow(cells);
  for (let i = 0; i < cells; i++) {
    const k = explain.cellConstants(cells, i);
    // What the script does: read the byte, divide by the divisor, take mod 2.
    const viaScript = Math.floor(row[k.byteC] / k.divC) % 2;
    // What the row layout says, directly.
    const viaLayout = (row[i >> 3] >> (i & 7)) & 1;
    assert.strictEqual(viaScript, viaLayout, `cell ${i} disagrees`);
    assert.strictEqual(explain.bitOfRow(row, i), viaLayout, `bitOfRow wrong at ${i}`);
  }
});

test('the neighbourhood constants encode the wrap and nothing else', () => {
  const cells = 128;
  for (let i = 0; i < cells; i++) {
    const k = explain.cellConstants(cells, i);
    assert.strictEqual(k.indexL, (i + 1) % cells, `left of ${i}`);
    assert.strictEqual(k.indexR, ((i - 1) % cells + cells) % cells, `right of ${i}`);
    // The divisor is always a power of two that shifts the wanted bit to 0.
    assert.strictEqual(k.divC, 1 << (i % 8), `divC of ${i}`);
    assert.strictEqual(k.byteC, Math.floor(i / 8), `byteC of ${i}`);
  }
  // The two cells where the ring actually closes.
  assert.strictEqual(explain.cellConstants(cells, 0).indexR, cells - 1);
  assert.strictEqual(explain.cellConstants(cells, cells - 1).indexL, 0);
});

// ---------------------------------------------------------------------------
// The dialogs
// ---------------------------------------------------------------------------

test('the cards open their dialogs, and opening one closes the other', () => {
  const rule110 = byId('dlgRule110');
  const script = byId('dlgScript');

  byId('openRule110').onclick();
  assert.strictEqual(rule110.open, true, 'the Rule 110 dialog did not open');
  assert.strictEqual(script.open, false);

  byId('openScript').onclick();
  assert.strictEqual(script.open, true, 'the script dialog did not open');
  assert.strictEqual(rule110.open, false, 'both dialogs were open at once');

  script.close();
  assert.strictEqual(script.open, false);
});

test('the cross-links move between the dialogs', () => {
  byId('dlgRule110').close();
  byId('dlgScript').close();

  byId('gotoScript').onclick();
  assert.strictEqual(byId('dlgScript').open, true);

  byId('gotoRule110').onclick();
  assert.strictEqual(byId('dlgRule110').open, true);
  assert.strictEqual(byId('dlgScript').open, false);
});

// ---------------------------------------------------------------------------
// The truth-table demo
// ---------------------------------------------------------------------------

test('the truth table is eight clickable answers', () => {
  const table = byId('ruleTable');
  assert.strictEqual(table.children.length, 8,
    `expected 8 neighbourhoods, got ${table.children.length}`);
});

test('flipping an answer moves the rule number by one power of two', () => {
  explain.setRule(explain.RULE_DEFAULT);
  assert.strictEqual(explain.ruleNumberNow(), 110);
  assert.strictEqual(byId('ruleNum').textContent, '110');
  assert.strictEqual(byId('ruleBits').textContent, '01101110');

  // Neighbourhood 111 is answer bit 7, which Rule 110 has off.
  byId('ruleOut7').onclick();
  assert.strictEqual(explain.ruleNumberNow(), 110 + 128,
    'flipping the 111 answer should add 128');
  assert.strictEqual(byId('ruleBits').textContent, '11101110');

  // And back.
  byId('ruleOut7').onclick();
  assert.strictEqual(explain.ruleNumberNow(), 110);

  // Every one of the eight, independently.
  for (let k = 0; k < 8; k++) {
    explain.setRule(explain.RULE_DEFAULT);
    byId('ruleOut' + k).onclick();
    assert.strictEqual(explain.ruleNumberNow(), 110 ^ (1 << k),
      `answer ${k} should toggle bit ${k}`);
  }
});

test('reset returns to 110', () => {
  explain.setRule(0);
  assert.strictEqual(explain.ruleNumberNow(), 0);
  byId('ruleReset').onclick();
  assert.strictEqual(explain.ruleNumberNow(), 110);
  assert.strictEqual(byId('ruleNum').textContent, '110');
});

// ---------------------------------------------------------------------------
// Deployment facts
// ---------------------------------------------------------------------------

test('the ring size comes from the snapshot, not from the markup', () => {
  explain.applyFacts({ cells: 512, rule: 110, history: [] });
  for (const el of classList('js-cells')) {
    assert.strictEqual(el.textContent, '512',
      'the cell count was not substituted into the prose');
  }
  for (const el of classList('js-rowbytes')) {
    assert.strictEqual(el.textContent, '64', 'the row byte count is wrong');
  }
  assert.strictEqual(byId('bitCell').max, '511',
    'the cell picker was not bounded by the ring size');
});

test('a live row is preferred over the sample, and read correctly', () => {
  // 16 cells, one byte 0x01 and one byte 0x80: cell 0 and cell 15 are alive.
  explain.applyFacts({ cells: 16, rule: 110, history: [{ n: 3, row: '0180', s: 'mm' }] });
  byId('bitCell').value = '15';
  explain.renderBits();
  const out = byId('bitOut').innerHTML;
  assert.ok(out.includes('0x80'), `expected the 0x80 byte in the readout, got:\n${out}`);
  assert.ok(out.includes('10000000'), 'expected the byte in binary');
  assert.ok(/bit-hi">1</.test(out), 'cell 15 of 0x0180 is alive and should read 1');

  byId('bitCell').value = '14';
  explain.renderBits();
  assert.ok(/bit-hi">0</.test(byId('bitOut').innerHTML), 'cell 14 should read 0');
});

test('an all-dead live row falls back to the sample', () => {
  // Roughly half a Rule 110 row is dead and a stalled ring is all dead. A
  // worked example reading 0x00 and dividing it by 8 explains nothing, so the
  // widget must not present one.
  explain.applyFacts({ cells: 64, rule: 110, history: [{ n: 1, row: '00'.repeat(8), s: '' }] });
  explain.renderBits();
  const out = byId('bitOut').innerHTML;
  assert.ok(!out.includes('0x00 '), `fell through to an empty byte:\n${out}`);
});

test('the default cell lands on a byte worth looking at', () => {
  // Byte 0 empty, byte 1 half full: the widget should open on byte 1.
  explain.applyFacts({ cells: 16, rule: 110, history: [{ n: 1, row: '000f', s: '' }] });
  assert.ok(Number(byId('bitCell').value) >= 8,
    `expected a cell in the non-empty byte, got ${byId('bitCell').value}`);

  // And when NO byte is the ideal weight it must still prefer a populated one
  // over byte 0. Byte 1 has two bits set, byte 0 none — neither is the four
  // this scores towards, and picking the best of a bad lot is the whole job.
  explain.applyFacts({ cells: 16, rule: 110, history: [{ n: 2, row: '0003', s: '' }] });
  assert.ok(Number(byId('bitCell').value) >= 8,
    `settled for the empty byte when nothing scored perfectly: got ${byId('bitCell').value}`);
});

test('the readout names the wrap at the ends of the ring', () => {
  explain.applyFacts({ cells: 16, rule: 110, history: [] });
  byId('bitCell').value = '0';
  explain.renderBits();
  assert.ok(byId('bitOut').innerHTML.includes('cell 15'),
    "cell 0's right neighbour should be cell 15");
  assert.ok(byId('bitOut').innerHTML.includes('the ring wrapped'),
    'the wrap should be called out where it happens');
});

test('an out-of-range cell is folded onto the ring rather than breaking', () => {
  explain.applyFacts({ cells: 16, rule: 110, history: [] });
  byId('bitCell').value = '99';
  explain.renderBits();
  assert.ok(byId('bitOut').innerHTML.length > 0, 'the readout went empty');
  byId('bitCell').value = '-5';
  explain.renderBits();
  assert.ok(byId('bitOut').innerHTML.length > 0, 'a negative index broke the readout');
});

// ---------------------------------------------------------------------------
// Run
// ---------------------------------------------------------------------------

let failed = 0;
for (const [name, fn] of tests) {
  try {
    fn();
    console.log('ok   ' + name);
  } catch (err) {
    failed++;
    console.log('FAIL ' + name);
    console.log('     ' + (err && err.message ? err.message : err));
  }
}
console.log(`\n${tests.length - failed}/${tests.length} passed`);
if (failed) process.exit(1);
