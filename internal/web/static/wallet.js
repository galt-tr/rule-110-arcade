// A minimal BRC-100 wallet client.
//
// Hand-rolled rather than pulling in @bsv/sdk, because this page is served by a
// Go binary with no JavaScript toolchain: the alternative is either a committed
// several-hundred-kilobyte bundle or an npm build step in a distroless CGO
// Dockerfile, and what we actually need is one call.
//
// BRC-100 defines several "substrates" — transports by which a page reaches the
// user's wallet. Two are live in practice and both are covered here:
//
//   window.CWI  an extension injects an object into the page.
//   HTTP        a desktop wallet listens on loopback and speaks JSON.
//
// This is a CLASSIC script, not an ES module, and exposes one global. app.js is
// loaded by a Node harness with require() for the renderer tests, so introducing
// `import` into that file would break a test suite to save a global.
//
// The HTTP one is the fragile part and it is worth knowing why before debugging
// it. The page is served over HTTPS and loopback is plain HTTP; that specific
// combination is allowed (http://localhost is "potentially trustworthy" in the
// Secure Contexts spec, so it is not blocked as mixed content), but Chrome's
// Private Network Access additionally requires the wallet to answer a CORS
// preflight with Access-Control-Allow-Private-Network, and browsers disagree on
// the details. When the substrate cannot be reached the UI must fall back to
// showing an address to pay by hand — see the manual panel in index.html.

const Wallet = (() => {

/** The loopback port BRC-100 desktop wallets listen on. */
const HTTP_SUBSTRATE = 'http://localhost:3321';

/** How long to wait for the wallet to answer a probe.
 *
 * Short, because this runs on page load against a port that is usually closed
 * and the answer "no wallet" has to arrive quickly enough to render the
 * fallback. A real call gets no timeout: it is waiting for a human to approve
 * a payment. */
const PROBE_MS = 1500;

class WalletError extends Error {
  constructor(kind, message) {
    super(message);
    this.kind = kind; // 'none' | 'rejected' | 'funds' | 'network' | 'failed'
  }
}

/** The substrate discovered by probe(), or null before it has run. */
let substrate = null;

/** Call the wallet, whichever substrate answered.
 *
 * The two transports carry the same method names and the same argument shape,
 * which is the whole reason this is short. */
async function call(method, args, { timeoutMs } = {}) {
  if (!substrate) throw new WalletError('none', 'no BRC-100 wallet is available');

  if (substrate === 'cwi') {
    return window.CWI[method](args);
  }

  const ctl = new AbortController();
  const timer = timeoutMs ? setTimeout(() => ctl.abort(), timeoutMs) : null;
  try {
    const res = await fetch(`${HTTP_SUBSTRATE}/${method}`, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        // BRC-100 identifies the calling page to the wallet so the user can see
        // who is asking. The wallet decides what to do with it; we must send it.
        'Originator': location.host,
      },
      body: JSON.stringify(args ?? {}),
      signal: ctl.signal,
    });
    if (!res.ok) {
      throw new WalletError('failed', `wallet returned ${res.status}`);
    }
    return res.json();
  } finally {
    if (timer) clearTimeout(timer);
  }
}

/** Find a wallet. Resolves to the substrate name, or null if there is none.
 *
 * The extension is tried first because it needs no network round trip and
 * cannot be blocked by a private-network policy. */
async function probe() {
  if (window.CWI && typeof window.CWI.createAction === 'function') {
    substrate = 'cwi';
    return substrate;
  }
  substrate = 'http';
  try {
    await call('getVersion', {}, { timeoutMs: PROBE_MS });
    return substrate;
  } catch {
    substrate = null;
    return null;
  }
}

/** What network the wallet's coins are on: 'mainnet' or 'testnet'.
 *
 * COARSE, and deliberately labelled so. BRC-100 has no way to say which test
 * network — teratestnet, classic testnet and any other all answer "testnet",
 * and they share an address version byte, so a wallet on the wrong one will
 * build a perfectly valid-looking payment to our address. This check catches
 * only the obvious mainnet-versus-testnet mistake. The authoritative check is
 * our own arcade refusing to broadcast, which is safe to rely on ONLY because
 * we ask the wallet not to send — see fundWith. */
async function network() {
  try {
    const res = await call('getNetwork', {});
    return res?.network ?? null;
  } catch {
    return null;
  }
}

/** Build a payment to `lockingScript` and return its BEEF as base64.
 *
 * noSend is the load-bearing option. It means the wallet signs the transaction
 * and hands it back WITHOUT broadcasting, so the payer's coins do not move
 * until our own arcade has validated the inputs against the chain this
 * deployment actually runs on. A wallet backed by the wrong test network fails
 * at our broadcast with nothing spent, instead of spending real coins onto a
 * chain we will never see.
 *
 * randomizeOutputs:false is a courtesy and not a dependency — the server scans
 * every output for the script it derived and never trusts an index. */
async function fundWith(lockingScript, satoshis) {
  const res = await call('createAction', {
    description: 'Fund the Rule 110 automaton',
    outputs: [{
      lockingScript,
      satoshis,
      outputDescription: 'rule110 funding',
    }],
    labels: ['rule110'],
    options: {
      noSend: true,
      randomizeOutputs: false,
      acceptDelayedBroadcast: false,
    },
  });

  const tx = res?.tx ?? res?.beef;
  if (!tx) {
    throw new WalletError('failed',
      'the wallet returned no transaction; it may not support noSend');
  }
  return { beef: toBase64(tx), txid: res?.txid ?? null };
}

/** Tell the wallet a noSend action has been sent, so its own records settle.
 *
 * Best effort by design. We already hold the coin; a wallet that never learns
 * the transaction went out has a cosmetic inconsistency in its history, and
 * failing the funding flow over that would be worse than the inconsistency. */
async function settle(txid) {
  if (!txid) return;
  try {
    await call('createAction', {
      description: 'Release the Rule 110 funding payment',
      outputs: [],
      options: { sendWith: [txid] },
    });
  } catch {
    /* see above */
  }
}

/** Map a wallet failure onto something a person can act on.
 *
 * The wallet's own message is not shown directly: it is written for a developer
 * and often names internal state. What a payer needs is which of a handful of
 * situations they are in, because each has a different next step. */
function explain(err) {
  if (err instanceof WalletError && err.kind === 'none') {
    return 'No BRC-100 wallet found. Install one, or pay the address below by hand.';
  }
  const text = String(err?.message ?? err).toLowerCase();
  if (text.includes('reject') || text.includes('denied') || text.includes('cancel')) {
    return 'Payment cancelled.';
  }
  if (text.includes('insufficient') || text.includes('not enough')) {
    return 'Your wallet does not have enough coin on this network.';
  }
  return `The wallet could not build the payment: ${err?.message ?? err}`;
}

/** Encode the wallet's byte array as base64.
 *
 * Chunked, and that is not premature. A payment BEEF carries the transaction
 * and its ancestry — hundreds of kilobytes is ordinary — and
 * String.fromCharCode.apply on an array that size exceeds the argument limit
 * and throws a RangeError that reads like a wallet fault. */
function toBase64(bytes) {
  const arr = bytes instanceof Uint8Array ? bytes : Uint8Array.from(bytes);
  const CHUNK = 0x8000;
  let s = '';
  for (let i = 0; i < arr.length; i += CHUNK) {
    s += String.fromCharCode.apply(null, arr.subarray(i, i + CHUNK));
  }
  return btoa(s);
}

return { probe, network, fundWith, settle, explain, WalletError };
})();

// The renderer test harness loads these files with require(); nothing else does.
if (typeof module !== 'undefined' && module.exports) module.exports = Wallet;
