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

**This fake already exists — in a package we cannot import.**
`internal/testenv/mockarcade` (`arcade.go:37`) is an `httptest.Server` speaking
real Arcade HTTP + SSE, driving the *real* `arcade.Client`, with exactly the
controls this test needs: `SetBroadcastResponder`, `SetTxRecord`, `EmitStatus`.
There is a matching `mockarcade.ChainTracks`. But it lives under the toolbox's
`internal/`, so Go's visibility rule puts it out of reach of this module.

Two options, and the first is better for everyone:

1. **Ask the toolbox to promote it** to `pkg/arcadetest` (or export it from
   `pkg/arcade`). Every application built on the toolbox needs precisely this to
   test its own recovery behaviour offline, and the toolbox already maintains it.
   This is the single most useful thing the toolbox could give downstream users.
2. Failing that, write our own against the same four routes — `POST /tx`,
   `GET /tx/{txid}`, `GET /events` (SSE with `Last-Event-ID`), health — plus the
   ChainTracks routes (`/height`, `/tip`, `/header/height/{n}`). The shapes are
   pinned by `pkg/arcade/client_test.go` and `sse_test.go`, so it is transcription
   rather than invention.

Start with option 2 so the plan is not blocked on another team, and raise option 1
in parallel.

The fake is *scriptable*: reject a named fraction of submissions, stall the event
stream, drop events outright, then return to normal. Run the real engine, the
real toolbox and a real SQLite wallet against it and assert:

1. the ring never reaches a state requiring `rule110 recover`;
2. after the fake returns to healthy, **every** cell advances again;
3. the diagram converges to the fake's own view of every transaction.

This is the closest thing to the live failure that can run in CI, and it is what
would have caught the 248 before it happened.

### 2.7 The basket separation we depend on without saying so

```
TestCellOutputsAreNeverInTheFuelBasket
```

The toolbox has a known-unfixed gap: `funder.FundArgs` carries no exclusion list,
so a caller-provided input that is *also* claimable in the funding basket can be
selected a second time, producing duplicate inputs
(`docs/rejection-hardening-audit.md:344-366`, rated High and deliberately not
implemented). We are safe from it for one reason only — cell outputs go to
`CellBasket` and fuel comes from `"fuel"`.

That is load-bearing and currently unwritten anywhere. A cheap assertion that no
cell output is ever minted into the fuel basket turns an implicit safety
property into a checked one.

---

## Part 3 — Toolbox tests

**Read this section against what already exists.** A survey of the toolbox's test
tree found that the headline reservation property is *already covered*, and that
the real gaps are somewhere else entirely. Proposing duplicates would have been
the wrong deliverable.

### 3.0 What is already proven — do not rebuild it

| Property | Test | Runs |
|---|---|---|
| Concurrent claims never issue the same coin twice, at the store layer | `utxostoretest` `ClaimExclusivityConcurrent` (`pkg/utxostore/utxostoretest/suite.go:446`) — 3 rounds × 90 coins × 3 workers × all 3 claim shapes | untagged: memstore + SQLite; `-tags integration`: Postgres + Aerospike |
| **N concurrent `CreateAction` against one shared basket never double-claim an input** | `conformance.contentionClaim` (`pkg/storage/conformance/contention.go:25`), n=12, asserts `claimed[key] == 1` and that failures are only `ErrNotEnoughFunds` / `ErrUTXOContention` | untagged: memstore+SQLite and over REST; tagged: PG Mode A, Aerospike hybrid |
| Funder-layer concurrent allocation | `testConcurrentFunding` (`pkg/storage/internal/funder/funder_integration_test.go:117`), 16 goroutines | `-tags integration` |
| Reconciler double-release race | `TestReconciler_ConcurrentDoublePass_NoDoubleRelease` (`pkg/storage/reconciler_test.go:740`) | untagged |

So the property you would most want — *"two workers cannot claim the same
UTXO"* — is tested today at both the store and the `CreateAction` layer. What
follows is only what is genuinely missing.

### 3.1 The single highest-value missing test

```
TestRejectedActionReleasesItsInputsAndTheCoinCanBeSpentAgain
```

**The two halves of this loop are each tested, and nothing joins them.**
`process_test.go:74` asserts inputs are *not* released on rejection (correct —
release is the reconciler's job). `reconciler_test.go:305` asserts the coin
becomes claimable. But the hermetic reconciler harness **never calls
`CreateAction`** — it hand-seeds `known_txs` rows via `seedKnownTx` — and the one
test that does drive the real path (`reconciler_integration_test.go:37`, PG-only)
stops at "claimable" and never spends the coin again.

So write the join, untagged and hermetic:

1. real `CreateAction` → `ProcessAction`, coin claimed;
2. oracle scripted to return `REJECTED`; deliver the status;
3. assert inputs still held — phase 1 is deliberate;
4. advance the injected clock past `grace`, run `VerifyAndReleaseSuspects`;
5. **`CreateAction` again and assert it succeeds, spending that same coin.**

Step 5 is the assertion nothing currently makes, and it is precisely the loop the
application depends on to keep building.

*Blocker to clear first:* `conformance.FakeOracle.GetTx` hardcodes
`ErrTxNotFound` (`pkg/storage/conformance/fakes.go:48`), so the exported harness
cannot script a `REJECTED` verdict at all. Either give `FakeOracle` a scriptable
`GetTxFunc` — worth doing, it is the exported seam other applications get — or
use the package-local `fakeOracle` (`status_updates_test.go:43`), which already
has the hook that `reconStack` drives via `scriptTx`.

### 3.2 Claim racing release — nothing races heterogeneous operations

Every concurrency test in the module races *homogeneous* operations: many
claimers, or many releasers. Nothing races a claimer against a releaser.

```
TestSweepDoesNotReclaimAReservationACreateActionIsMidFlightOn
TestAbortDoesNotRaceAConcurrentClaimOfTheSameCoin
TestReconcilerReleaseRacingAFreshClaimNeverDoubleAllocates
```

This is a real safety question rather than a hypothetical: `SweepStaleReservations`
guards it with `reservationResendable` (`pkg/storage/status_updates.go:1083`), and
that guard has no concurrent test. Sweeping a reservation that `SendWaiting` is
about to re-drive would manufacture the exact double-spend the sweep exists to
prevent.

### 3.3 Leak recovery is barely tested, and the existing tests sidestep the clock

`SweepStaleReservations` has exactly one test
(`pkg/storage/status_updates_test.go:756`) and `AbortAbandoned` exactly one
(`:540`). Both are single-threaded, and both cheat by passing
`time.Now().Add(time.Hour)` as the cutoff rather than driving a fake clock.

```
TestReservationLeakedByANeverSignedActionIsSwept   // real clock injection
TestSweptCoinIsImmediatelyClaimableAgain
```

**Trap to avoid:** all three hermetic provider harnesses build the utxostore as
bare `memstore.New()` with **no clock injected** (`reconciler_test.go:110`,
`status_updates_test.go:147`, `provider_test.go:114`). The provider and metastore
clocks are fakeable; reservation timestamps are not. A test asserting on
reservation *age* must construct `memstore.New(memstore.WithClock(clock.Now))`
itself — copy the pattern at `pkg/utxostore/memstore/memstore_test.go:28-44`.

### 3.4 Dimensions the existing concurrency tests do not reach

Cheap to add on top of `contentionClaim` rather than beside it:

- **an under-supplied pool** — demand deliberately exceeding supply, on an
  exact-selection backend (today only the approximate-selection path sees this);
- **N ≫ 12** — the current test is n=12, which is not 256;
- **more than one basket and tier at once** — the automaton uses two (`fuel` and
  the cell basket), and their separation is load-bearing (see 3.5);
- **`-race`** — see below.

### 3.5 Two defects in the toolbox's own test plumbing

Found while surveying, and both worth a fix regardless of this plan:

- **`-race` is never actually run.** `docs/storage.md:213` states the conformance
  suites "run under `-race`" and `utxostoretest/suite.go:69` says "Run it under
  -race" — but no Makefile target, CI workflow or lint config passes the flag
  anywhere in the module. Every concurrency property above is currently asserted
  without the detector that would catch the memory races underneath it. Add
  `test-race` and wire it into CI.
- **`make test-conformance` selects almost nothing.** It filters
  `-run 'TestConformance'`, but the tests are named `TestProviderConformance_*`,
  `TestMemStoreConformance` and `TestAerostore_Conformance`. Go's `-run` is an
  unanchored per-segment regex, so only the Aerospike one matches; the provider
  and memstore conformance suites are silently skipped by the target that exists
  to run them.

### 3.6 Worth raising with the toolbox team

Not blockers, but they force applications to infer things they should be told:

- there is **no** typed spend-conflict error and **no** "inputs released, safe to
  retry" signal — an application can only notice that `CreateAction` stopped
  returning `ErrNotEnoughFunds`;
- `staleReservationTTL` (15 min, `pkg/monitor/monitor.go:26-29`) has no config key;
- after `DefaultMaxQuarantine` = 24 h a suspect becomes terminal `stuck` and is
  never auto-released — defensible, but it must be loud;
- the known-unfixed funder gap: `FundArgs` has no exclusion list, so a
  caller-provided input that is also claimable in the funding basket can be
  selected twice (`docs/rejection-hardening-audit.md:344-366`). We are safe only
  because cell outputs live in a different basket from fuel — which earns a test
  of its own on our side (§2.7).

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

This application:

```
go vet ./...
go test ./... -race                 # CI already runs exactly this
go test ./... -race -tags smoke     # adds the fake-arcade smoke test
```

The toolbox:

```
make test                                   # untagged; includes the conformance
                                            # contention test that already exists
go test -race ./pkg/storage/... ./pkg/utxostore/...   # by hand today — see 3.5
make test-integration                       # testcontainers; Postgres + Aerospike
```

Nothing above contacts a live arcade, a live node, or the network. The toolbox
gates its Postgres and Aerospike tests on `//go:build integration` plus runtime
detection of a container engine (`internal/testenv/testenv.go:73` skips rather
than fails), so a machine without podman/docker simply runs the hermetic subset.

Our CI workflow (`.github/workflows/ci.yml:52`) picks up every new unit test with
no change; the smoke test is added as a separate tagged job so its runtime is
visible rather than hidden inside the unit suite.

**Acceptance for the whole plan, stated as one property:** there is no sequence
of rejections, dropped events, or restarts that leaves a cell needing a human.
2.1, 2.2 and 2.6 are the three tests that assert it; everything else exists to
make them pass for the right reasons.

## Order of work

1. 1.1 (checked gate release) + 2.3 — smallest change, most likely cause.
2. 2.1 and 2.2 written **first and failing**, so the fix is measured against them.
3. 1.2 and 1.3, until 2.1 and 2.2 pass; Part 4 rewrites land with them.
4. 2.7 — one cheap assertion, no dependencies.
5. 3.1 — the reject→release→**respend** join, the toolbox's biggest gap. Needs the
   `FakeOracle` scripting hook first.
6. 3.5 — add a `-race` target and fix the conformance `-run` filter. Small, and
   everything else in Part 3 is worth less without it.
7. 2.6, the smoke harness (option 2), with the request to promote `mockarcade`
   raised in parallel.
8. 3.2 and 3.3 — claim-vs-release racing and leak recovery.
9. 1.4, then re-run the depth ladder on a fresh wallet.

Performance work — Aerospike versus Postgres, and the storage-provider question —
comes after this, and by then it will have contention numbers to justify it
rather than a hunch.
