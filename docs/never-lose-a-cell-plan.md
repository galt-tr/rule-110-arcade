# Never lose a cell to a rejection

A plan to make the automaton structurally incapable of stopping, and to prove it
with tests that run offline. **No step in this plan requires a live arcade.**

## Context

On 2026-08-13 a run ended with **248 of 256 cells permanently halted**. The
trigger was a burst of 3,962 asynchronous `REJECTED` statuses, almost all
`PROCESSING (4): failed to validate`. The burst passed. The halts did not: they
are terminal, and clear only when an operator runs `rule110 recover`.

Two things were established first, and they shape everything below.

**The toolbox's UTXO management is not the problem.** Its basket claim is a
genuine atomic operation, not a read-then-write race — Postgres does candidate
selection and reservation in one statement
(`pkg/utxostore/sqlstore/ops_claim.go:21-28`):

```sql
WITH candidate AS (
  SELECT txid, vout FROM utxos
  WHERE ... reserved_by IS NULL AND spent_by IS NULL AND NOT frozen
  ORDER BY satoshis, seq LIMIT 1 FOR UPDATE SKIP LOCKED)
UPDATE utxos u SET reserved_by = $5, reserved_at = $6 FROM candidate c ...
```

Aerospike does the same job with a single-record CAS. There is no global
coin-selection mutex. Measured across ~90,000 transactions from 256 concurrent
workers: **zero** double-spends, **zero** `UTXO_SPENT`, **zero** funding
shortfalls, **zero** broadcast errors.

**But the cells are not wallet-managed coins.** Each cell's continuation output
is passed to `CreateAction` as a caller-supplied explicit outpoint, and for those
`resolveProvidedInputs` (`pkg/storage/create_inputs.go:26-68`) does pure
resolution — no claim, no `reserved_by`, no uniqueness check, no `utxostore`
touch. The toolbox's own audit states the consequence:

> "For an input the wallet does not own — a caller-supplied outpoint — the
> toolbox has no reservation and no spend state, so **only the caller can know**
> that an attempt was in flight when the process died."

So the safety net covers fuel and, by construction, not the 256 covenant chains.
Their integrity is entirely this application's code. That is where we broke, and
that is what this plan hardens.

### The two one-way doors

| Door | Where | Effect |
|---|---|---|
| `maxRetries = 3` within `minHaltWindow = 60s` | `internal/engine/status.go:344` | cell halts; only `rule110 recover` clears it |
| `maxWreckage = 3` stacked rejections | `internal/engine/derive.go:54` | derivation *withholds* the cell — no repair is even offered |

Only 46 of the 248 halts came from the first door. The rest came from the
second. Both were written for a cell that had drifted alone for hundreds of
generations — "a decision about the automaton's history". Neither was written for
a burst that hits every cell at once and then clears.

### One suspect is our own code

`gate_timeouts=119` in that run: 119 times a cell advanced **without** an
acknowledgement, because the acceptance-gate deadline added the previous day
fires unconditionally. If the parent genuinely was not there, every child after
it is invalid and cascades until the pile trips `maxWreckage`. That is the right
order of magnitude for 3,962. Unproven, but it is the first thing to fix.

---

## Part 1 — Application changes

### 1.1 Make the gate deadline *checked*, not blind

`unseenGateExpired` (`internal/engine/worker.go`) currently releases a cell on a
timer alone. Change it to ask before it acts: on expiry, call
`TxStatus.GetTx(parent)`.

| arcade says | action |
|---|---|
| `ACCEPTED_BY_NETWORK` / `SEEN_*` / `MINED` | advance — the status was only lost in delivery |
| `REJECTED` / `DOUBLE_SPEND_ATTEMPTED` | do **not** advance; mark the cell for repair |
| `ErrTxNotFound` / transport error | do **not** advance; re-arm the deadline |

This is the case we actually measured — 11 of 11 and then 242 of 242 stuck rows
came back `ACCEPTED_BY_NETWORK`, meaning the transaction was fine and only the
event was lost. The check costs one HTTP call on a path that fires rarely by
construction, and it converts a bet into a decision.

### 1.2 Halt stops being terminal

Replace permanent halt with **unbounded exponential backoff**. A cell that cannot
advance retries at an ever slower cadence (cap ~10 minutes), stays visible as
`rule110_cells_backing_off` and in the UI as needing attention, and **never**
requires an operator to resume.

Keep a genuinely terminal state for exactly one thing: a cell whose covenant no
longer verifies locally, which is a code bug rather than a network condition.
Everything a network can do to a cell must be recoverable without a human.

### 1.3 Distinguish environmental from structural

Add a shared refusal window. If more than a threshold fraction of live cells
refuse inside the same short window, classify it as environmental:

- do not charge it against any cell's `maxRetries` budget;
- do not let it push a cell past `maxWreckage`;
- log once for the ring rather than once per cell;
- count it as `rule110_refusal_bursts_total`.

A ring-wide burst must never be able to halt the ring. This is the single change
that would have prevented the 248.

### 1.4 Size the fuel pool for quarantine

Newly established, and it changes sizing: a rejection does **not** immediately
free its inputs. `applyRejected` only marks the transaction suspect
(`pkg/storage/status_updates.go:285`); release requires
`VerifyAndReleaseSuspects` (60 s interval) plus `DefaultSuspectGrace` = 90 s —
realistically ~150 s — and the change minted by that transaction is **frozen**
meanwhile. After `DefaultMaxQuarantine` = 24 h a suspect becomes terminal `stuck`
and is never auto-released.

The toolbox says it plainly: *"Size fuel pools for worst-case quarantine if
rejections are common."* Document the arithmetic in the README next to
`-fuel-pool`, and surface locked capital as a metric so starvation from
quarantine is diagnosable rather than mysterious.

---

## Part 2 — Application tests, all offline

The seams already exist and are documented as existing for this purpose.
`chain.TxStatus` (`internal/chain/recover.go:200`) carries the comment that
"both branches — including the ones reached only when arcade is unreachable —
have to be testable without a network". Build on:

- `newFixture(t)` (`internal/engine/derive_test.go:51`) — real compiled covenant
  scripts, a real genesis transaction, a real SQLite store, a `countingLedger`;
- `engineOn(t, f)` — a real engine over that fixture;
- `f.advance` / `f.build` / `refuse(...)` — drive a cell and refuse a generation;
- `fakeOracle` (`internal/chain/recover_test.go:365`) and `fakeLedger` (`:24`).

### 2.1 The headline property

```
TestARingWideRejectionBurstNeverHaltsTheRing
```

Drive all 256 cells to a tip. Refuse **every** cell's newest generation several
times inside one short window — more refusals per cell than `maxRetries`, and
more stacked than `maxWreckage`. Then stop refusing.

Assert: **zero cells halted**, every cell holds a repair schedule, and after the
backoff each cell advances again. This test fails today, and it is the one that
matters most.

### 2.2 Recovery is unconditional

```
TestNoRejectionSequenceCanPermanentlyHaltACell
```

A table over the rejection shapes we have actually seen from arcade — retryable,
`PROCESSING (4): failed to validate`, `TX_LOCKED (37)`,
`parent rejected (ancestor …)`, `UTXO_SPENT (70)`, and a doubled-prefix live
message — each repeated well past every budget. For every shape, assert the cell
ends in a state from which it advances once refusals stop. No shape, and no
repetition count, may produce a state needing an operator.

### 2.3 The gate cannot advance onto a dead parent

```
TestExpiredGateAsksArcadeBeforeAdvancing        // ACCEPTED → advances
TestExpiredGateRepairsInsteadOfAdvancingOnAReject // REJECTED → repairs, does not advance
TestExpiredGateDoesNotAdvanceWhenArcadeIsUnreachable
```

A `fakeOracle` supplies each answer; no network. The third is the important one:
"we could not ask" must never be read as "yes".

### 2.4 Backoff is bounded and progressing

```
TestBackoffGrowsAndIsCapped
TestABackingOffCellStillAdvancesOnceRefusalsStop
```

Guards the obvious failure of item 1.2 — a backoff that grows without bound is a
halt wearing a different name.

### 2.5 Burst classification

```
TestABurstDoesNotSpendPerCellRetryBudget
TestAnIsolatedRefusalStillCountsAgainstItsCell
```

The second matters as much as the first: one genuinely broken cell must still be
caught, or item 1.3 has traded one failure for another.

### 2.6 Smoke test — the whole application against a fake arcade

```
TestSmokeRingSurvivesAScriptedRejectionStorm   (build tag: smoke)
```

Stand up an `httptest.Server` implementing the four routes the toolbox actually
uses — `POST /tx`, `GET /tx/{txid}`, `GET /events` (SSE with `Last-Event-ID`),
health — plus the ChainTracks routes (`/height`, `/tip`,
`/header/height/{n}`). The toolbox already fakes this surface for its own tests
(`pkg/arcade/client_test.go`, `pkg/arcade/sse_test.go`), so the shapes are known
and can be followed rather than invented.

The fake is *scriptable*: reject a named fraction of submissions, stall the event
stream, drop events outright, then return to normal. Run the real engine, the
real toolbox and a real SQLite wallet against it and assert:

1. the ring never reaches a state requiring `rule110 recover`;
2. after the fake returns to healthy, **every** cell advances again;
3. the diagram converges to the fake's own view of every transaction.

This is the closest thing to the live failure that can run in CI, and it is what
would have caught the 248 before it happened.

---

## Part 3 — Toolbox tests

Two properties, both offline. Where a real database is needed, SQLite is enough
for correctness and Postgres adds the concurrency dimension; gate the Postgres
runs behind the harness the repo already uses so CI stays hermetic.

### 3.1 Reservation is safe under concurrency

```
TestConcurrentClaimsNeverIssueTheSameCoinTwice
```

Mint N coins into one basket. Launch W ≫ N goroutines all calling the claim path
simultaneously. Assert:

- **no outpoint is ever returned to two claimants** — the core invariant;
- exactly `min(N, W)` claims succeed and the rest get "none" (or, on Aerospike,
  `ErrContention` after its CAS budget), never a phantom coin;
- every claimed row carries a `reserved_by` matching its claimant;
- run under `-race`.

Repeat for each backend, and for the exact-match, smallest-sufficient and
largest-insufficient variants, since each has its own statement.

```
TestClaimUnderContentionNeverBlocksIndefinitely
```

`SKIP LOCKED` should mean a loser moves to another coin rather than queueing.
Assert bounded wall-clock for W goroutines against N coins.

### 3.2 A rejection always returns its inputs

```
TestRejectedTransactionReleasesItsInputsAndTheyCanBeClaimedAgain
```

The loop the application depends on, end to end and offline:

1. claim a coin, build and "broadcast" a transaction through a stub oracle;
2. deliver `REJECTED`;
3. assert inputs are **still held** — the deliberate phase-1 behaviour
   (`pkg/storage/status_updates.go:98-105`), so the test documents it rather than
   being surprised by it;
4. advance the injected clock past `DefaultSuspectGrace`, run
   `VerifyAndReleaseSuspects`;
5. assert the coin is claimable again **and** a fresh claim succeeds.

Step 4 must use the existing clock seam, not `time.Sleep` — a test that waits 90
seconds will be deleted by the first person in a hurry.

```
TestRecoveredRejectionReleasesNothing
TestSpendConflictReleasesOnlyTheLosingInputs
TestQuarantinedSuspectIsReportedRatherThanSilentlyStuck
```

The third is a gap worth closing: after 24 h a suspect becomes terminal `stuck`
and never auto-releases. That is defensible, but it must be *loud*.

### 3.3 Leaked reservations are reclaimed

```
TestReservationLeakedByANeverSignedActionIsSwept
TestSweepDoesNotYankInputsFromARedrivableTransaction
```

`SweepStaleReservations` rides the `fail_abandoned` task at a **15-minute**
TTL (`pkg/monitor/monitor.go:26-29`) with no config key. Both tests use the
injected clock. The second guards `reservationResendable`
(`pkg/storage/status_updates.go:1083`) — sweeping a transaction that
`SendWaiting` is about to re-drive would manufacture the double-spend the sweep
exists to prevent.

### 3.4 Worth raising with the toolbox team

Not blockers, but they force applications to infer things they should be told:

- there is **no** typed spend-conflict error and **no** "inputs released, safe to
  retry" signal — an application can only notice that `CreateAction` stopped
  returning `ErrNotEnoughFunds`;
- `staleReservationTTL` has no config key;
- the known-unfixed funder gap: `FundArgs` has no exclusion list, so a
  caller-provided input that is also claimable in the funding basket can be
  selected twice (`docs/rejection-hardening-audit.md:344-366`). We are safe only
  because cell outputs live in a different basket from fuel — which is a fact
  worth a test of its own on our side.

---

## Part 4 — Tests that must be consciously replaced

These currently *defend* the behaviour this plan removes. They are not to be
quietly deleted; each is rewritten to assert the new property, with the reason in
the test comment.

| Test | Today | Becomes |
|---|---|---|
| `TestRepairStopsAfterMaxRetries` (`repair_test.go:151`) | halts after 3 refusals past the window | enters backoff, stays recoverable, still advances later |
| `TestRetryNeverReachesACascade` (`recovery_test.go:912`) | cascade is withheld from retry | cascade is withheld from *automatic* retry but remains recoverable |
| `TestRecoverIgnoresACascadeRejection` (`recovery_test.go:953`) | as above | as above |

`maxWreckage`'s reasoning stays valid for what it was written for — a single cell
that built on phantom outputs for hundreds of generations really is a judgement
call. What changes is that the judgement no longer has to be a human's, and never
applies to a whole ring at once.

---

## Verification

```
go vet ./...
go test ./... -race                 # CI already runs exactly this
go test ./... -race -tags smoke     # adds the fake-arcade smoke test
```

Nothing above contacts a live arcade, a live node, or the network. The existing
CI workflow (`.github/workflows/ci.yml:52`) picks up every unit test with no
change; the smoke test is added as a separate tagged job so its runtime is
visible rather than hidden inside the unit suite.

**Acceptance for the whole plan, stated as one property:** there is no sequence
of rejections, dropped events, or restarts that leaves a cell needing a human.
2.1, 2.2 and 2.6 are the three tests that assert it; everything else exists to
make them pass for the right reasons.

## Order of work

1. 1.1 (checked gate release) + 2.3 — smallest change, most likely cause.
2. 2.1 and 2.2 written **first and failing**, so the fix is measured against them.
3. 1.2 and 1.3, until 2.1 and 2.2 pass; Part 4 rewrites land with them.
4. 3.1 and 3.2 — the toolbox properties, independent of the above.
5. 2.6, the smoke harness.
6. 1.4, then re-run the depth ladder on a fresh wallet.

Performance work — Aerospike versus Postgres, and the storage-provider question —
comes after this, and by then it will have contention numbers to justify it
rather than a hunch.
