# Arcade SSE delivery at 256 tx/s — findings for the arcade team

Gathered 2026-08-12 against `arcade-v2` on `dev-ovh-1` (arcade **v0.12.1**), from
arcade's own pod logs plus `GET /tx` verdicts. The consumer is the Rule 110
automaton: one `go-arcade-toolbox` wallet, **one** SSE subscription, 256
independent transaction chains at 1 generation/second.

Nothing here needs action from us to keep running — we have worked around all of
it. It is written up because the workarounds are ours and the causes are not.

---

## 1. A block's MINED burst is ~10x the per-client SSE buffer, and the overflow is lost

This is the one that matters.

Steady-state event volume is modest and well inside budget. The problem is not a
rate, it is a **burst**: a block sweeps `rate x cells x block-interval`
transactions and arcade emits MINED for all of them at once. At 256 tx/s with
~300 s blocks that is **~77,000 events in one burst**, against a per-client send
buffer of **8,192** (`buffer_cap` below; configurable since arcade #277, default
8192 — this deployment is at the default).

Arcade drops the overflow and schedules a mid-stream catch-up. Observed on
`sse-bc97cb47b-mxzsp` and `-fxgns`:

```
sse fan-out could not enqueue events: client send buffer full; will catch up mid-stream
  client_id:40 dropped:36496 buffer_cap:8192 clients:1 fanout_avg:0.001296582

sse mid-stream catchup round
  client_id:40 frames:122687 round_cap:4096 live_frames:8569 capped:true
```

Note `clients:1` — that is a single subscriber, not contention between many.

The catch-up then hits its own ceiling, and **this is where delayed delivery
becomes permanent loss**:

```
sse catchup truncated at frame cap  cap:10000 fresh_connect:false
```

Past 10,000 frames the remaining events are simply not replayed. They are not
late; they are gone from the stream. A consumer's only remaining route to them is
`GET /tx`, one transaction per HTTP call.

We also see the catch-up itself abort under its own weight:

```
sse mid-stream catchup aborted  client_id:50 frames:96768
  error: write tcp 10.224.26.68:8082->10.224.2.193:60958: write: connection reset by peer
```

**Suggested direction.** Three things would each help independently: size the
per-client buffer to a block's worth of events rather than a fixed 8192; make the
catch-up frame cap large enough that truncation is not silent data loss (or log
it at ERROR with the count, so a consumer can tell); and consider coalescing a
block's MINED events into one framed batch, since they share a block hash and
height and are the entire burst.

## 2. The fan-out is a single goroutine doing one synchronous Postgres query per event, per client

Already known and documented in the toolbox benchmarks
(`docs/benchmarks/20260811-1000tps-45min-transition-timing.md`), which located it
at `services/sse/manager.go:212` -> `store/postgres/postgres.go:1363` and timed
the token->txid membership probe at **0.583 ms**, i.e. a ceiling of **~1,500-1,700
events/s**.

Still true in v0.12.1, and arcade now reports it itself: the `fanout_avg` field
above sits at **0.3-0.6 ms** in the steady state and spikes to **11-44 ms** under
load — 23 to 90 events/s at the bad end.

**The log wording is actively misleading.** `client send buffer full` /
`dropped events for slow SSE client` names the consumer as the slow party. When
we first hit this, a goroutine dump showed our SSE reader parked in `netpoll`
waiting for data and our applier idle in `collectBatch` waiting for its first
event — the client was **starved, not saturated**. The toolbox benchmark records
the same signature independently ("the client was idle... the name is wrong").
That message cost us a full misdiagnosis cycle, and it will cost the next
consumer one too.

**Suggested direction.** Rename the message, or qualify it with `fanout_avg` so
the reader can see which side is behind. Longer term the probe is the fix: it is
a membership test that could be cached per token, or resolved once per fan-out
batch rather than once per event per client.

## 3. `GET /events` has no filter, so a consumer cannot shard its own subscription

`GET /events` accepts exactly one query parameter (`callbackToken`) and one header
(`Last-Event-ID`). There is no status filter, no txid subscription, no partition
or shard key.

The consequence is that a consumer with a genuine throughput problem has **no
horizontal option**. A second connection on the same token receives a full
duplicate of every event *and doubles arcade's per-event cost*, because the
membership probe of §2 runs once per event **per client** inside the one fan-out
goroutine. Parallelism makes it strictly worse for both sides.

**Suggested direction.** Even a coarse `?status=` filter would help: this
application discards `SEEN_MULTIPLE_NODES` entirely and treats
`ACCEPTED_BY_NETWORK` and `SEEN_ON_NETWORK` as the same state, so more than half
the events we are sent are thrown away on arrival — and the fan-out paid full
price for each. A shard key (`?shard=i/n`) would additionally let a consumer
scale out at all.

## 4. `X-FullStatusUpdates` bundles a latency signal with a volume cost

Not a defect, but worth knowing how it is being used.

`ACCEPTED_BY_NETWORK` is only sent with full status updates on, and it is the
first status that tells a consumer its transaction is safe to build on. From the
toolbox's instrumented run: `create -> ACCEPTED_BY_NETWORK` p50 **154 ms**, and
`ACCEPTED -> SEEN_ON_NETWORK` a further p50 **8,513 ms**.

For any consumer that chains transactions, that 55x gap is the whole ballgame —
so full updates cannot be turned off to relieve §1 and §2, even though for our
*display* purposes the extra transitions are redundant. We are obliged to take
double the event volume to get one latency signal.

**Suggested direction.** If the milestone set included `ACCEPTED_BY_NETWORK`, or
if it were separately selectable, chained-transaction consumers could halve their
event volume without giving up their build-on signal. This is probably the
cheapest single change on this page for the traffic arcade actually carries.

## 5. `api-server` is OOMKilling repeatedly

Verified live, not inferred:

```
api-server-54bc59fb7f-dq4t4  restarts=15  lastReason=OOMKilled
api-server-54bc59fb7f-rqpj4  restarts=15  lastReason=OOMKilled
api-server-54bc59fb7f-xpz2h  restarts=17  lastReason=OOMKilled
```

Limit `2Gi`, no `GOMEMLIMIT` set on this deployment (the `sse` deployment does set
`GOMEMLIMIT=1600MiB` against its 2Gi limit; `api-server` sets none). Most recent
kill 09:19 local on 2026-08-12. We have not traced a specific consumer symptom to
these restarts, so this is reported as an observation rather than a diagnosis —
but 47 OOM kills across three replicas in ~25 hours seems worth someone's
attention, and setting `GOMEMLIMIT` would at least convert it from a kill into
back-pressure.

---

## What we did on our side

For completeness, so nobody spends time on our behalf:

- **The repair poll is our convergence guarantee**, not the stream — as the
  toolbox playbook says it should be. Ours is now ordered un-acknowledged-first,
  sized adaptively to the backlog, and stamps every row it picks up so a
  transaction arcade cannot answer for cannot block the head of every pass.
- **Every gate has a deadline.** A status that never arrives now costs one slow
  cell rather than stopping the automaton.
- We keep `X-FullStatusUpdates` on, for the §4 reason.

The measurement that started this: of 11 cells our diagram had stuck in
`broadcast`, **11 of 11** returned `ACCEPTED_BY_NETWORK` from `GET /tx`. Arcade
had every verdict. None of them ever reached us over the stream.
