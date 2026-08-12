# Rule 110 on BSV — state of the project

Last updated 2026-08-12, at `v2.0` plus two commits.

## What this is

A Rule 110 cellular automaton whose every cell is proved on chain. The ring is
128 independent UTXO chains; a cell's transition at generation N spends its own
output from N-1, and a covenant script enforces that the cell's new bit is the
correct Rule 110 output of the neighbourhood it claims to have read.

**The caveat is the interesting part, and it is load-bearing: each cell's script
verifies only its OWN bit.** Nothing in Script compares cell 5's asserted
neighbourhood against what cells 4 and 6 actually did — there is no opcode that
can look at another cell's output, which is exactly what lets a generation fan
out across 128 chains instead of serialising. Cross-cell agreement is therefore
an *auditable* property, not a proved one. `rule110 audit` is what audits it.

## Current deployment

Reset from a fresh genesis on 2026-08-12, after the test network's chain was
reset out from under the previous run.

| | |
|---|---|
| Genesis | `06072381e042aa657bd88313f47b6a02aa19dbff258f22ec46d5a857be766cc6` |
| Ring | 128 cells, rule 110, seed `01000000…` (single 1 bit) |
| Funding address | `mpz7rAwYignR5bybEGP4aZQbeikjxiRQ2U` |
| Reserve | ~29.8 BSV |
| Fuel pool | ~13k coins of 1000 sat, keeper maintaining 20k |
| Halted cells | 0 |
| Storage | PostgreSQL, container `rule110-db` on `:5434` |
| UI | http://localhost:8110 (boots **paused**) |

Both the wallet and the automaton's history live in that one PostgreSQL
database. `./data` holds only `keys.json` and `state.json`. This matters more
than it sounds — see *Traps* below.

### Running it

```bash
export RULE110_POSTGRES_DSN="postgres://postgres:postgres@127.0.0.1:5434/rule110?sslmode=disable"
export RULE110_ARCADE_URL="https://arcade-v2.dev-ovh-1.ubsv.dev"
./rule110 run
```

Without those two variables the app silently uses SQLite and does not see this
wallet at all. `scripts/reset.sh` writes `data/env` for exactly this reason.

## What v2.0 fixed

v1.0 destroyed its own cells under load. Every item below was measured on a live
128-cell deployment, not reasoned about.

**The ways it destroyed itself.** The write-ahead record now gates the
broadcast: `persist` halted a cell when its row could not be stored, but halting
stops the *next* turn, not the one already running, so a cell whose intent could
not be recorded broadcast anyway — spending its tip with nothing durable saying
it had tried. An exhausted connection pool halted 70 of 128 cells in three
minutes and every one had broadcast regardless.

A halted laggard no longer freezes the clock. `frontier` is the minimum over
*non-halted* cells, so halting the slowest cell makes the frontier jump past the
target, and `target - frontier` on `uint64` underflowed to ~2^64 — never less
than `maxLag`, so the clock stopped raising the target for ever. Silent, because
`Snapshot.Lag` saturates: the UI read a calm `lag 0` while the clock was dead.
One refused cell could stop all 128.

A local failure no longer halts a cell permanently — a database hiccup before
signing costs a retry, not a cell. A rejected legacy tip is no longer treated as
a migration floor, and that question now goes to the network rather than to our
own mutable record. The history store's connection pool is bounded.

**Recovery.** Five damage classes, dry run by default behind `-apply`:
unresolved write-ahead attempts; a `UTXO_SPENT` rejection naming our own
unrecorded accepted transition; a rejection built against a superseded parent; a
failure that never reached the network; and an opt-in bounded retry of an
unexplained refusal. Recovery examines the record derivation *actually halted
on*, not the one at `tip+1` — which is what let repair converge instead of
reporting success over dead cells. On the previous deployment: halted cells
124 → 11.

**The audit.** `rule110 audit` performs nine checks, including decoding the
neighbourhood the covenant actually read out of the spent output's locking
script and cross-checking it against the BIP-143 preimage. Against real history,
generations 1000–1020: **2,174 cell transactions, 17,411 checks, 0 failures,
cross-cell agreement 2,174/2,174.**

**Throughput and operability.** BEEF accumulation removed (315 MB → 1.05 MB
state; 1,194 kB → 8.1 kB per transaction), per-generation barrier removed, fuel
pool ends coin contention (4 → 267 tx/s), single-writer lease, graceful funding
starvation with unattended resume, bounded reconciler, wallet-DB pruner,
Dockerfile, Kubernetes manifests, CI, README.

## The status pipeline, after v2.0

The UI lagged arcade, and the cause was that this process held **two** SSE
connections on the same callback token: the wallet monitor's, and one the engine
opened for itself. The toolbox documents that as pathological — arcade's
`GET /events` has no per-client filter, so the second connection received a full
duplicate of every event and doubled arcade's per-event fan-out cost, against a
fan-out already measured as the bottleneck.

The engine's copy was also the slow-consumer case that behaviour punishes. It
applied each event synchronously on the SSE reader goroutine, under the engine's
global write lock, across a Postgres round trip — so a reader that should have
been draining its socket was instead parked on the database, once per event. The
toolbox's own playbook names the mistake: *"Do not hold a global lock across a
synchronous database write."*

What changed:

- **One connection.** `monitor.WithStatusObserver` (added to the toolbox) hands
  each applied batch to the engine off the connection that already exists. The
  engine's own subscription is gone, and with it an in-memory replay cursor that
  reset to `""` on every restart — arcade replays only NON-terminal statuses to a
  cold connection, so terminal statuses that landed while we were down were being
  lost. The monitor's cursor is durable.
- **No database write under the lock.** Ownership is decided under a read lock,
  the whole batch applies under one write-lock acquisition, and persistence goes
  to a batching writer that coalesces on a linger into one multi-row `UPDATE`.
- **Snapshots cost what they emit.** `SnapshotTail` built the full 2048-generation
  copy and discarded 97.7% of it, per push, per client. It now copies only the
  tail, and a publisher marshals that tail once for every subscriber.
- **Pushes are leading-edge.** The stream used to wait for a change and *then*
  sleep 100 ms, taxing every update to coalesce the few that needed it. The floor
  is now applied between publishes, not before one, and the handler subscribes
  before it reads — closing a race that left a stale final frame after pause.
- **The diagram is painted incrementally.** The browser used to clear the canvas
  and repaint every generation on every message. The canvas is now a durable
  surface: rows are painted once, scrolled with the bitmap when the window
  slides, and repainted only when their generation's pixels actually change.
  Measured in a real browser with 2,072 rows on screen: **0** `fillRect` calls
  for ten unchanged pushes, **65** for a push that changes one cell, against
  ~134,000 per push before.

  This also removes a slow fuse. The client kept every generation it was ever
  sent while the server capped at 2048; canvas height is generations × cellPx,
  browsers refuse a dimension over ~32767px, and past that the diagram did not
  degrade — it went blank. The client now holds the same bound as the server and
  caps what it renders, so maximum zoom shows fewer rows instead of none.

Measured on the live deployment, one generation stepped from paused: 128 cells
broadcast, first status through the observer ~1.9 s later, all 128 proved within
~4 s, 0 failed, 0 halted, and the UI's state agreed with arcade's `/tx/` for a
sampled transaction.

## A refusal no longer costs a cell

Cell 106 went red at generation 249 on 2026-08-12 and took 250 with it: 250 was
already broadcast when 249's rejection landed, 1.16s before its own. The cell then
halted and stayed halted, because one refusal used to kill a cell until an operator
ran `rule110 recover`.

Nothing failed there — the halt fired promptly and correctly. The problem is what
came next, and since refusals are intermittent (~2 per 16,000 transactions) it is
monotonic erosion: the ring loses cells one at a time to a fault that usually goes
away by itself.

A network refusal now schedules a rebuild instead of a halt. The rejection arrives
on the monitor's applier goroutine, which must never block, so it only records the
refusal and flags the cell; the repair itself runs on the cell's own worker — the
only goroutine that can be sure no transition for that cell is in flight.

The repair does not invent a second decision path. It re-derives from the store and
calls the same `recoverRejected` the operator's `rule110 recover` calls, with the
same `RetryRefusal` at the end of it, and applies through the same `applyRecovery`.
Re-deriving is the bug 9a guard: the tip comes from the newest row the network
actually accepted, verified against the covenant locking script, and never from the
refused transaction's own output.

It loops, because a refusal usually leaves more than one dead row and the reviewed
repair retracts exactly one per pass so that each row gets its own evidence. Cell
106 needed two passes, and only the first needed the unexplained-retry rule — the
second was decided by `RecoverStaleRejection`, which proved generation 250 was built
on a parent the cell no longer had. Clearing the whole pile in one repair also keeps
the cell away from `maxWreckage`, past which derivation stops offering it to
recovery at all.

Bounded: `maxRetries` **consecutive** refusals of the same generation and the cell
halts for a human, keeping arcade's own words as the reason. Consecutive is the
point — a cell that refuses once, recovers and meets an unrelated fault two hundred
generations later has not failed twice at the same thing.

**Startup stays conservative.** `-auto-recover` still never retries an unexplained
refusal: it runs over all 128 cells, unattended, right after a crash, which is
exactly when the program knows least. So damage already in the store when the
process starts — cell 106's was — still needs `rule110 recover -retry-refused`.

## Measured, not assumed

- **No mempool ancestor limit on this network.** 600 transactions, ≥250 deep,
  zero rejections.
- **`PROCESSING (4)` is intermittent, not deterministic.** A refused generation
  was retracted, rebuilt and *accepted*, then ran 58 further generations before
  hitting a fresh one. Roughly 2 refusals per 16,000 transactions. This is the
  single most important finding for the open work: it means a halted cell is
  almost always recoverable by simply trying again, and that a reset does not
  help, because a fresh ring erodes at the same rate for the same reason.
- **The fuel keeper bootstraps a cold pool by itself.** From one 30 BSV coin it
  built 20,048 fuel coins with no manual step.

## Open work

Everything below is now tracked as a GitHub issue in `galt-tr/rule-110-arcade`,
along with the performance defects found while fixing the status pipeline
(issues 8–13). This section stays as the narrative; the issues carry the same
detail item by item.

### 1. ~~Retry an intermittent refusal instead of halting for ever~~ — DONE — [#1](https://github.com/galt-tr/rule-110-arcade/issues/1)

Implemented; see *A refusal no longer costs a cell* above. The rest of this entry
is kept as the reasoning that led to it.

Today one refusal kills a cell until an operator runs `rule110 recover`. Since
refusals are intermittent, that is monotonic erosion: the ring loses cells one
at a time, and — before the clock fix — a single loss could stop the whole
automaton.

Treat a network refusal as retryable: retract the record and let the cell
rebuild that generation, bounded, halting only after N consecutive refusals of
the *same* generation. The safety argument is already proven — a refused
transaction spends nothing, so the tip is unspent and rebuilding cannot double
spend; and if the tip *has* been spent, arcade answers `UTXO_SPENT` and
`RecoverSpentTip` adopts the real spender. This is the automatic form of what
`recover -retry-refused` already does by hand.

Guard against reintroducing the bug 9a cascade: the retry must rebuild from the
cell's real derived tip, never from the rejected transaction's output.

### 2. Pause coasts up to 32 generations — [#2](https://github.com/galt-tr/rule-110-arcade/issues/2)

Pressing pause at generation 20 left it running to 52 before settling — exactly
`maxLag`, about 100 seconds, with `lag` draining 25 → 22 → 15 → 10 → 0.
`SetMode` freezes `e.target` where the clock left it rather than pulling it
back, so every already-commissioned generation still gets built.

The current behaviour is deliberate (don't strand a half-finished generation)
but that argument covers the *current* generation, not 32 of them. Fix: on
pause, set `e.target = frontierLocked()`. Verify `TestPausedStillFinishesAStep`
still passes rather than assuming it.

### 3. `rule110 fuel` and `genesis` could not bootstrap in throughput mode — FIXED

Both failed with `not enough funds` on a fresh wallet holding 30 BSV, after a
4-minute retry, with an error that misled on both counts it raised (the funding
transaction *was* mined; the monitor was *not* the problem). The workaround on
record was `-throughput=false`.

**The cause was one unset field, and it is invisible from this repository.**
storage's `fanOutSourceBasket` routes a fan-out by its DESTINATION: a shape whose
`Basket` is the pool draws its funding from the RESERVE basket unless
`SourceBasket` says otherwise. The reserve is the fuel keeper's own staging area —
filled by aggregating change crumbs — so on a fresh wallet it is empty, while the
deposit `internalize` just credited sits in change. The one command whose job is
to create the pool could not fund itself.

The keeper never had the bug only because its `RecycleBasket` setting becomes
that same field.

`Config.fuelShape` now names the source, and `TestFanOutDrawsFromTheChangeBasket`
is the guard. Cold start runs fund → fuel → genesis with throughput left on, and
`internal/boot` does exactly that unattended.

### 4. Replay the cascade cells (previous deployment only) — [#4](https://github.com/galt-tr/rule-110-arcade/issues/4)

Four or five cells on the *old* deployment carried ~170 stacked rejections each
over tips near generation 300, needing ~1,380 replayed transitions apiece to
rejoin the ring. Recovery deliberately refuses them (`maxWreckage = 3`). Not
applicable to the current fresh ring; keep the design note in case it recurs.

### 5. Fuel keeper soak — [#5](https://github.com/galt-tr/rule-110-arcade/issues/5)

The keeper looked healthy on the previous deployment — the pool gained while
running at 4 gen/s, where it had earlier drained toward starvation. That
improvement was never soaked long enough to close honestly, and the earlier
flapping was entangled with connection exhaustion and a saturated co-tenant
database. Re-measure on this clean deployment before declaring it fixed.

### 6. Append-only history log — deferred by choice — [#6](https://github.com/galt-tr/rule-110-arcade/issues/6)

Would replace `cell_txs` as the system of record, touching the engine, the store
and the web layer. Deliberately not in v2.0: shipping a system-of-record rewrite
with no soak time into a release whose whole point is stability is how records
get lost.

### 7. Unexplained rejection class — [#7](https://github.com/galt-tr/rule-110-arcade/issues/7)

`PROCESSING (4): failed to validate transaction` still has no root cause. Ruled
out by direct comparison of a mined and a rejected transaction from the same
cell: identical fee (410 sat), identical size class, identical unlocking-script
push structure. Arcade's `extraInfo` is generic and our txids do not appear in
teranode's propagation logs, so the node-side reason is unreachable from either
end. Teranode logs show `tx meta maybe too big for txmeta cache` with parent
hash counts up to 9,953 in the same window — plausible, unproven.

## Traps

Things that cost real time here, recorded so they cost less next time.

**The wallet is in PostgreSQL, not `./data`.** `outputs`, `utxos`,
`transactions`, `known_txs` and `output_baskets` sit alongside `cell_txs` and
`generations`. Dropping the database container destroys the coin as surely as
deleting the keys, and spending needs *both* halves — there is no chain rescan.
`scripts/reset.sh` defaults to resetting the automaton only, and requires typing
the satoshi balance to confirm `--wipe-wallet`.

**`data/postgres.dsn` is not configuration.** Nothing reads it. The DSN comes
from `-postgres-dsn` or `RULE110_POSTGRES_DSN` and defaults to SQLite. A
bootstrap without it *appears* to work — `fund` reports the balance, `fuel`
starts minting — and you discover the problem at 128-cell fan-out, after the
coin is in the wrong store.

**Arcade's `/tx/` returning `transaction not found` does not mean the
transaction is absent from the chain.** Arcade only knows transactions it
processed. Ask the node's `getrawtransaction` before concluding anything.

**`UTXO_SPENT` naming a mined transaction absent from `cell_txs` is our own lost
transition**, not a foreign double spend — that is the signature recovery keys
on.

**The engine boots paused.** Drive it with
`POST /api/control {"action":"play"}` and `{"action":"rate","rate":N}`. The
endpoints are `/api/state`, `/api/events`, `/api/control`, `/healthz`,
`/readyz`, `/metrics` — there is no `/api/status`.

## Verification

`gofmt`, `go vet` and `go test ./... -race` are clean across all seven packages.
Every fix listed above has a test verified to fail against the previous code.
