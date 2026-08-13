# Recovery run, 2026-08-13

The first run of the automaton after the "never lose a cell" work, on a fresh
wallet and a freshly reset chain. It is not the depth ladder — that is still
owed — but it settled several questions, including two where I was wrong.

## Setup

256 cells, rule 110, `-max-unseen 1`, `-full-status` on, 1 gen/s requested.
Genesis `5b17b44c…`, confirmed `SEEN_ON_NETWORK` before the ring was allowed to
build on it. Wallet funded with 30 BSV, fuel pool minted to 20,000 coins.

The chain had been reset earlier that morning: the tip went from 15063 (08:56)
to 15010 (10:28) and arcade answered `transaction not found` for the parents of
every coin the wallet held. An earlier genesis attempt was REJECTED for exactly
that reason — `PROCESSING (4): failed to validate transaction`, which is what
arcade says when a transaction's inputs do not exist. Clearing the dead coin
records and re-funding fixed it, which is the cleanest confirmation available
that the message means "missing inputs" and not anything about this code.

## What held

**Status fidelity was exact.** 69,575 transactions broadcast, 69,575 statuses
applied — one for one, not "eventually converged". At the sampled moment only 8
rows sat in `broadcast` against 25,903 `seen`. The original complaint that
started this work — cells showing `broadcast` while the network had long since
seen or mined them — did not occur.

**Nothing halted and nothing stalled.** `halted=0`, `stalled=0` across the whole
run, including 15,637 failed transitions. Those failures were funding
shortfalls, not refusals: the pool emptied and transitions could not claim a
coin. Under the previous code a fraction of that number cost 248 of 256 cells
permanently.

**Zero arcade rejections.** Not one.

**The acceptance gate never needed its deadline.** `probes=0`,
`unanswered=0` — every status arrived over the stream on its own, so the
mechanism that replaced blind advancing was never called on.

**A block avalanche was absorbed.** One block swept the backlog and 69,576 MINED
events were delivered and applied without loss. That burst is what overran
arcade's per-client buffer on 2026-08-12 and produced `catchup truncated at
frame cap` — permanent loss. Worth tempering: this ring had run twenty minutes,
not hours, on a quieter chain, so it is one clean observation rather than a
disproof of the earlier failure.

## Where I was wrong

**I called a freeze that was not one.** Two consecutive identical samples showed
`gen=30, tx=9676` and I reported the automaton frozen. It was not: it was at
gen=90 with 25,655 transactions, and the sampler was lagging because fetching
`/api/state` over a 25k-row window is slow. The arcade check I ran off that
snapshot — "49 of 49 stuck rows have verdicts" — was measuring transactions in
flight, not lost. That is the same error as reading a wedged system's goroutine
dump and concluding arcade was healthy: over-trusting one snapshot.

**I predicted the stranded transactions were unrecoverable.** 114 wallet
transactions had `was_broadcast = true` and no arcade status, with `GET /tx`
answering `transaction not found`; their change held 29.7 BSV at `TierSending`,
the keeper could not refill, and the automaton starved. I reasoned that the
status poll could not apply a status arcade does not have, and that
`SendWaiting` skips already-broadcast rows, so nothing would ever re-drive them
— and was about to report it upstream as a toolbox gap.

It healed by itself in under three minutes:

```
11:04:58  no_status=98   claimable=64,052 sat         starved
11:05:19  no_status=2    claimable=2,969,535,977 sat  starved
11:05:39  no_status=134  claimable=2,968,742,292 sat  running
```

The repair poll cleared the backlog, the change promoted in one step, the keeper
refilled and the automaton returned to running with no operator action. The
recovery guarantee holds at the wallet layer as well as the cell layer.

## What starvation actually costs

The run reached `starved` because the fuel pool drained from 20,000 to 160 while
the keeper's own change sat unpromoted. This is the promote-on-SEEN coupling:
change is minted at `TierSending` and becomes claimable only when its SEEN
status is APPLIED, so a keeper minting hard cannot fund its next chunk until the
status pipeline catches up. It logs `not enough funds` while holding 29.7 BSV.

That is the arithmetic already written into the README's fuel-sizing section,
observed live. The pool must carry the transactions in flight *plus* everything
awaiting acknowledgement, and 20,000 coins is not enough for 256 cells at this
rate.

## The depth ladder

Each rung measured over a clean window from the monotonic counters, at 1 gen/s
requested, 256 cells.

| `-max-unseen` | gen/s | tx/s | statuses/s | rejections | halted | stalled |
|---|---|---|---|---|---|---|
| 1 | 0.342 | 95.1 | 114.5 | **0** | 0 | 0 |
| 8 | 0.613 | 161.9 | 178.7 | **0** | 0 | 0 |
| 32 | 0.787 | 195.0 | 195.4 | **0** | 0 | 0 |
| 0 (off) | *not measurable* | — | 241.1 | — | 0 | 0 |

**Zero ordering rejections at every rung that ran.** Depth 1 cost 2.3× the
throughput against depth 32 to prevent something that did not happen even with
the gate 32 deep. On this network, at this rate, the shallow gate was not
buying safety.

**Returns diminish sharply.** 1 → 8 gave 1.8×; 8 → 32 gave 1.28×. Working the
model backwards, `rate = depth ÷ ack-latency` implies ~2.9 s acknowledgement
latency at rung 1 and ~13 s at rung 3 — so latency is not a constant, it grows
with the number of transactions in flight. Depth lets more cells run ahead,
which loads arcade, which slows acknowledgement, which eats most of the gain.
The `ackLatency*` constants in engine.go are a starting estimate, not a law, and
the comment there should not be read as one.

**Rung 4 is not a result.** With the gate off entirely the automaton made zero
transitions in 150 seconds — not because of the gate but because the fuel pool
was exhausted (2,983 coins and falling at ~200 tx/s). The number to take from
that row is the 241 statuses/s, which is the pipeline draining a mined backlog,
not a throughput figure.

Statuses tracked broadcasts one-for-one at every measurable rung: 114.5 vs 95.1,
178.7 vs 161.9, 195.4 vs 195.0.

## Restarting a running ring costs cells

Found by doing it. A SIGTERM at 162 tx/s stranded **179 of 256 cells** with
`tip is UNKNOWN: a transition may have been broadcast without being recorded` —
the write-ahead record catching a stop that landed between the intent row and
the broadcast row. `rule110 recover` resolved all 179: **141 adopt** (signed, so
it reached the network — move the tip forward, re-spend nothing) and **38
resume** (created but never signed, so nothing went out — roll back). That
distinction is the entire reason the intent row exists.

A later restart with `-auto-recover` healed 101 stranded cells by itself, with
no operator action, which is the flag working as designed.

But that is mitigation. **The defect is that shutdown does not drain**: the
workers stop wherever they stand, so a stop reliably strands about a second's
worth of cells. Stopping the clock, waiting for in-flight transitions to record,
then exiting would make a restart cost nothing. That is a fourth halt path,
distinct from the three this plan closed, and it is now the highest-value item
left.

## The burst classifier did not engage on the one burst that occurred

After the depth-0 restart, 233 distinct transactions came back REJECTED in a
flood — the closest thing this run produced to the 2026-08-13 shape. Throughout
it: `stalled=0`, `burst_refusals=0`, `halted=0`.

Nothing broke, but nothing engaged either. Those rejections are catch-up
statuses for transactions the previous process broadcast, and they apply to
generations that have already scrolled out of the live window, so they land as
`failed` display rows without reaching `noteRefusalLocked` at all. The classifier
was designed for refusals arriving against LIVE cells; a flood of stale
rejections for departed generations is a different shape and it has nothing to
say about them.

That is a gap in the work, not a success: the counter that would demonstrate the
classifier working stayed at zero through the event that most resembled what it
was built for. Whether it needs to engage there is a real question — the cells
those rejections name have already moved on, so charging them budget would be
wrong — but the current behaviour is accidental rather than designed, and the
tests do not cover it.

## Still owed

- **Drain on shutdown** (above) — the real fix for restart-stranded cells.
- **A fuel pool sized for the rate.** 20,000 coins ran dry at 95 tx/s and 200,000
  was never reached because the keeper cannot mint faster than promote-on-SEEN
  allows. Every rung after the first ended in starvation.
- **Rung 4, measured properly**, once fuel is not the binding constraint.
- **What actually caps this deployment near 200 tx/s**, now that depth
  demonstrably is not it past rung 2.
