/* The two explainer dialogs: what Rule 110 is, and how a cell's bit is proved
 * in Bitcoin Script.
 *
 * Separate from app.js on purpose. render_test.js require()s app.js into a
 * hand-built DOM stub to check the diagram's painting; the demos in here want a
 * real 2D context and a real <dialog>, and dragging those into that harness
 * would cost more than it buys. Nothing in this file touches the renderer.
 *
 * This is a CLASSIC script, not an ES module, for the same reason app.js is —
 * both are loaded with require() by the Node test harness.
 */

/* The automaton, and the neighbourhood convention.
 *
 * "Left" is the NEXT-HIGHER cell index, matching internal/ca/ca.go and the way
 * app.js paints cell 0 rightmost. Those two decisions cancel: a row stored with
 * index increasing and drawn with index decreasing looks exactly like the
 * textbook picture. Keeping the same convention here means the demo is running
 * the same automaton as the contract, which is what the tests assert against.
 */
function stepRow(row, rule) {
  const n = row.length;
  const out = new Uint8Array(n);
  for (let i = 0; i < n; i++) {
    const l = row[(i + 1) % n];
    const c = row[i];
    const r = row[(i - 1 + n) % n];
    // Bit 4l+2c+r of the rule number IS the answer. Same fact the locking
    // script relies on to hold its whole transition table in one byte.
    out[i] = (rule >> (4 * l + 2 * c + r)) & 1;
  }
  return out;
}

/* Bit i of a packed row: byte i/8, bit i%8, least significant first. The
 * layout ca.Row uses and the locking script reads. */
function bitOfRow(bytes, i) {
  return (bytes[i >> 3] >> (i & 7)) & 1;
}

/* The six constants that make one compiled contract into cell i of a ring:
 * which byte to read and what to divide by, per neighbour. The ring wrap lives
 * here and nowhere else. */
function cellConstants(cells, i) {
  const l = (i + 1) % cells;
  const r = ((i - 1) % cells + cells) % cells;
  return {
    byteL: l >> 3, divL: 1 << (l & 7), indexL: l,
    byteC: i >> 3, divC: 1 << (i & 7), indexC: i,
    byteR: r >> 3, divR: 1 << (r & 7), indexR: r,
  };
}

const RULE_DEFAULT = 110;
const NBHDS = [7, 6, 5, 4, 3, 2, 1, 0]; // displayed 111 down to 000

const dlgRule110 = document.getElementById('dlgRule110');
const dlgScript = document.getElementById('dlgScript');

let ruleNumber = RULE_DEFAULT;
let ringCells = 0;       // 0 until /api/state answers
let liveRow = null;      // newest row as bytes, when the deployment has one

/* --- opening and closing -------------------------------------------------- */

/* The hash is shared with app.js, which owns `gen=`. Rewrite only our own key
 * so a deep link to a generation survives opening an explainer and vice versa.
 */
function currentHash() {
  return (typeof location !== 'undefined' && location.hash) ? location.hash : '';
}

function setHash(which) {
  const kept = /(?:^|#|&)gen=(\d+)/.exec(currentHash());
  const parts = [];
  if (which) parts.push('explain=' + which);
  if (kept) parts.push('gen=' + kept[1]);
  const hash = parts.length ? '#' + parts.join('&') : ' ';
  if (typeof history !== 'undefined' && history.replaceState) {
    history.replaceState(null, '', hash);
  }
}

function openExplainer(which) {
  const dlg = which === 'script' ? dlgScript : dlgRule110;
  const other = which === 'script' ? dlgRule110 : dlgScript;
  if (other && other.open && other.close) other.close();
  if (!dlg || !dlg.showModal) return;
  dlg.showModal();
  setHash(which);
  loadDeploymentFacts();
  if (which === 'script') renderBits(); else drawPattern();
}

function wireOpen(id, which) {
  const el = document.getElementById(id);
  if (el) el.onclick = () => openExplainer(which);
}
wireOpen('openRule110', 'rule110');
wireOpen('openScript', 'script');
wireOpen('gotoScript', 'script');
wireOpen('gotoRule110', 'rule110');

// The dialog's own close button is a <form method="dialog">, so closing needs
// no JS — but the hash does have to stop advertising a dialog that is shut.
[dlgRule110, dlgScript].forEach((dlg) => {
  if (!dlg) return;
  dlg.addEventListener('close', () => setHash(null));
  // A native <dialog> counts the backdrop as part of itself, so a click lands
  // on the dialog with coordinates outside its box. That is the only way to
  // tell the two apart.
  dlg.addEventListener('click', (ev) => {
    if (ev.target !== dlg) return;
    const b = dlg.getBoundingClientRect();
    const outside = ev.clientX < b.left || ev.clientX > b.right ||
                    ev.clientY < b.top || ev.clientY > b.bottom;
    if (outside && dlg.close) dlg.close();
  });
});

/* --- deployment facts ----------------------------------------------------- */

/* Fetched on first open rather than at load: nobody who never opens an
 * explainer should pay for it, and /api/state carries the live tail. */
let factsPending = null;
function loadDeploymentFacts() {
  if (ringCells || factsPending) return factsPending || Promise.resolve();
  factsPending = fetch('/api/state')
    .then((r) => r.json())
    .then((s) => {
      applyFacts(s);
    })
    .catch(() => {});
  return factsPending;
}

function applyFacts(s) {
  if (!s || !s.cells) return;
  ringCells = s.cells;
  const hist = s.history || [];
  const newest = hist.length ? hist[hist.length - 1] : null;
  if (newest && newest.row) liveRow = hexToBytes(newest.row);
  fill('js-cells', String(ringCells));
  fill('js-rowbytes', String(ringCells / 8));
  const input = document.getElementById('bitCell');
  if (input) {
    input.max = String(ringCells - 1);
    // Land on a cell whose byte actually has bits in it. Roughly half a Rule
    // 110 row is dead, so the default of cell 3 lands on 0x00 often enough
    // that the worked example would routinely be 0 ÷ 8 = 0 — arithmetic that
    // demonstrates nothing about reading a bit out of a byte.
    input.value = String(interestingCell(ringCells));
  }
  renderBits();
}

// getElementsByClassName rather than querySelectorAll to stay with the
// old-school DOM calls the rest of this application uses.
function fill(cls, text) {
  const els = document.getElementsByClassName(cls);
  for (let i = 0; i < els.length; i++) els[i].textContent = text;
}

function hexToBytes(hex) {
  const out = new Uint8Array(hex.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(hex.substr(i * 2, 2), 16);
  return out;
}

/* --- demo: the eight answers ---------------------------------------------- */

const ruleTable = document.getElementById('ruleTable');
const ruleNum = document.getElementById('ruleNum');
const ruleBits = document.getElementById('ruleBits');

function buildRuleTable() {
  if (!ruleTable) return;
  ruleTable.textContent = '';
  NBHDS.forEach((nbhd) => {
    const wrap = document.createElement('div');
    wrap.className = 'rule-case';

    const nb = document.createElement('div');
    nb.className = 'rule-nbhd';
    // Displayed left to right as (l, c, r), which is bits 2, 1, 0 of nbhd.
    [4, 2, 1].forEach((mask) => {
      const b = document.createElement('i');
      b.className = 'rule-bit' + (nbhd & mask ? ' on' : '');
      nb.appendChild(b);
    });
    wrap.appendChild(nb);

    const arrow = document.createElement('span');
    arrow.className = 'rule-arrow';
    arrow.textContent = '▼';
    wrap.appendChild(arrow);

    const out = document.createElement('button');
    out.type = 'button';
    out.className = 'rule-out';
    out.id = 'ruleOut' + nbhd;
    out.setAttribute('aria-label',
      'answer for neighbourhood ' + nbhd.toString(2).padStart(3, '0'));
    out.onclick = () => setRule(ruleNumber ^ (1 << nbhd));
    wrap.appendChild(out);

    ruleTable.appendChild(wrap);
  });
}

function setRule(n) {
  ruleNumber = n & 0xff;
  renderRule();
  drawPattern();
}

function renderRule() {
  if (ruleNum) ruleNum.textContent = String(ruleNumber);
  if (ruleBits) {
    let s = '';
    for (let k = 7; k >= 0; k--) s += (ruleNumber >> k) & 1;
    ruleBits.textContent = s;
  }
  NBHDS.forEach((nbhd) => {
    const el = document.getElementById('ruleOut' + nbhd);
    if (el) el.className = 'rule-out' + ((ruleNumber >> nbhd) & 1 ? ' on' : '');
  });
}

const ruleReset = document.getElementById('ruleReset');
if (ruleReset) ruleReset.onclick = () => setRule(RULE_DEFAULT);
const ruleRandom = document.getElementById('ruleRandom');
if (ruleRandom) ruleRandom.onchange = () => drawPattern();

/* --- demo: the picture ---------------------------------------------------- */

const PAT_PX = 2;
const PAT_ROWS = 150;
const patternCanvas = document.getElementById('rulePattern');

function drawPattern() {
  if (!patternCanvas || !patternCanvas.getContext) return;
  const ctx = patternCanvas.getContext('2d');
  if (!ctx) return;

  const cssWidth = patternCanvas.clientWidth || 640;
  const cols = Math.max(32, Math.floor(cssWidth / PAT_PX));
  patternCanvas.width = cols * PAT_PX;
  patternCanvas.height = PAT_ROWS * PAT_PX;

  const css = getComputedStyle(document.documentElement);
  const dead = css.getPropertyValue('--dead').trim() || '#161b26';
  const live = css.getPropertyValue('--mined').trim() || '#b9e0ff';

  ctx.fillStyle = dead;
  ctx.fillRect(0, 0, patternCanvas.width, patternCanvas.height);

  let row = new Uint8Array(cols);
  if (ruleRandom && ruleRandom.checked) {
    for (let i = 0; i < cols; i++) row[i] = Math.random() < 0.5 ? 1 : 0;
  } else {
    // One live cell, near the right edge. Under Rule 110 the pattern grows
    // towards higher indices, which this canvas draws leftwards, so seeding at
    // the right is what makes the triangle sweep the full width instead of
    // filling half of it and leaving the rest blank.
    row[2] = 1;
  }

  ctx.fillStyle = live;
  for (let y = 0; y < PAT_ROWS; y++) {
    for (let i = 0; i < cols; i++) {
      // Cell 0 rightmost, exactly as app.js paints the real diagram.
      if (row[i]) ctx.fillRect((cols - 1 - i) * PAT_PX, y * PAT_PX, PAT_PX, PAT_PX);
    }
    row = stepRow(row, ruleNumber);
  }
}

/* --- demo: reading one bit out of a row ----------------------------------- */

const bitCell = document.getElementById('bitCell');
const bitOut = document.getElementById('bitOut');
if (bitCell) bitCell.oninput = () => renderBits();

/* A stable stand-in for a row, used until the deployment tells us a real one.
 * Deterministic so the tests are not at the mercy of what the ring happens to
 * be doing. */
function sampleRow(cells) {
  const b = new Uint8Array(cells / 8);
  for (let i = 0; i < b.length; i++) b[i] = (i * 37 + 11) & 0xff;
  return b;
}

/* The row the widget reads. The live one when the deployment has a usable one,
 * because "this is the row your ring is holding right now" is worth more than
 * an invented example — but an all-dead row is not usable, so fall back. */
function rowForWidget(cells) {
  if (liveRow && liveRow.length === cells / 8 && liveRow.some((b) => b !== 0)) {
    return liveRow;
  }
  return sampleRow(cells);
}

/* A cell sitting in a byte that has something in it. */
function interestingCell(cells) {
  const bytes = rowForWidget(cells);
  let best = 0;
  let bestScore = -Infinity;
  for (let b = 0; b < bytes.length; b++) {
    let pop = 0;
    for (let k = 0; k < 8; k++) pop += (bytes[b] >> k) & 1;
    // Four-ish set bits reads best: enough on and enough off that the shift is
    // visibly doing something. Every score here is negative or zero, so the
    // starting score has to be -Infinity — seeding it at -1 would accept only
    // an exact match and otherwise leave byte 0, which is the empty byte this
    // function exists to avoid.
    const score = -Math.abs(pop - 4);
    if (score > bestScore) { bestScore = score; best = b; }
  }
  return best * 8 + 3;
}

function bin8(n) { return n.toString(2).padStart(8, '0'); }
function hex2(n) { return n.toString(16).padStart(2, '0'); }

function renderBits() {
  if (!bitOut) return;
  const cells = ringCells || 256;
  let i = Number(bitCell && bitCell.value);
  if (!Number.isFinite(i)) i = 0;
  i = ((Math.round(i) % cells) + cells) % cells;

  const k = cellConstants(cells, i);
  const bytes = rowForWidget(cells);
  const byte = bytes[k.byteC];
  const bit = bitOfRow(bytes, i);

  const lines = [];
  const put = (key, val, hi) =>
    lines.push('<div class="bit-row"><span class="bit-k">' + key +
      '</span><span class="' + (hi ? 'bit-hi' : 'bit-v') + '">' + val + '</span></div>');

  put('byteC = ' + i + ' / 8', String(k.byteC));
  put('divC  = 2^(' + i + ' % 8)', String(k.divC));
  put('that byte', '0x' + hex2(byte) + '  =  ' + bin8(byte));
  put('÷ ' + k.divC, String(Math.floor(byte / k.divC)));
  put('% 2', String(bit), true);
  lines.push('<hr>');
  put('left  neighbour', 'cell ' + k.indexL + ' — byte ' + k.byteL + ', ÷ ' + k.divL);
  put('right neighbour', 'cell ' + k.indexR + ' — byte ' + k.byteR + ', ÷ ' + k.divR);
  if (k.indexL < i || k.indexR > i) {
    lines.push('<div class="bit-row"><span class="bit-k"></span>' +
      '<span class="bit-k">↑ the ring wrapped, and it cost nothing to do so</span></div>');
  }

  bitOut.innerHTML = lines.join('');
}

/* --- boot ----------------------------------------------------------------- */

buildRuleTable();
renderRule();
renderBits();

// Deep link: #explain=script or #explain=rule110.
const deep = /(?:^|#|&)explain=(script|rule110)/.exec(currentHash());
if (deep) openExplainer(deep[1]);

// Exposed for the test harness, which drives this file under a DOM stub. Not
// used by the page itself.
if (typeof module !== 'undefined' && module.exports) {
  module.exports = {
    stepRow, bitOfRow, cellConstants, sampleRow,
    setRule, renderBits, applyFacts, openExplainer,
    RULE_DEFAULT,
    ruleNumberNow: () => ruleNumber,
  };
}
