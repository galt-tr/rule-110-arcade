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

## Still owed

- A clean rung-1 rate. Both windows so far were contaminated: the first by
  startup, the second by fuel drain.
- The depth ladder itself: 1 → 8 → 32 → 0, recording achieved gen/s and
  rejections at each rung. Zero ordering rejections is the acceptance test for
  whether depth 1 was ever buying anything.
- A larger fuel pool before the next attempt, sized per the README rather than
  the 20,000 that ran out here.
