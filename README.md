# rule-110-arcade

A Rule 110 cellular automaton whose transition rule is enforced in native
Bitcoin Script. Each cell is a UTXO; spending it proves, on chain, that one bit
of the next row is correct. A ring of 128 cells therefore costs 128 transactions
per generation, spread across 128 independent UTXO chains.

It exists to put a workload through [`go-arcade-toolbox`][toolbox] (a BRC-100
wallet) and arcade (broadcast) that neither had seen: not "many payments" but a
few long, unbroken, self-spending chains of large covenant transactions, all
advancing at once. Most of what it found is written up in the package comments —
this file points at them rather than repeating them.

The package docs are the reference. Start with `cmd/rule110/main.go`,
`contracts/Cell.runar.go`, `internal/cellscript/cellscript.go`,
`internal/engine/engine.go`, `internal/history/history.go`,
`internal/chain/config.go`, `internal/chain/fuel.go` and `cmd/rule110/probe.go`.

[toolbox]: https://github.com/galt-tr/go-arcade-toolbox

---

## What is proved on chain, and what is not

Read this before anything else. It is the difference between what this
application demonstrates and what it would be nice to claim it demonstrates.

**Each cell verifies exactly one bit of the next row: its own.**

Cell *i*'s UTXO carries the whole current row. Its script reads bits *i−1*, *i*
and *i+1* out of that row, looks up the rule, and asserts that bit *i* of the
row it is being handed matches. Because the contract embeds
`runar.StatefulSmartContract`, the compiler injects `checkPreimage` and a
state-continuation output, so the successor UTXO is forced to carry the row that
was presented. Per cell, and only per cell, the chain enforces:

1. the successor carries exactly the next row that was presented;
2. that row is the right length;
3. bit *i* of it is the correct Rule 110 image of bits *i−1*, *i*, *i+1*.

**Nothing in Script forces the 128 chains to adopt the *same* next row.**

Cell 3 will happily accept a next row whose other 127 bits are arbitrary, so
long as bit 3 is right. Cell 7 will do the same for bit 7. There is no opcode
anywhere that compares one cell's row to another's, and there is no shared
output that all 128 transactions must agree on. The 128 chains are genuinely
independent — that independence is what lets a generation fan out in parallel,
and it is bought at exactly this price.

So a complete, correct generation is proved by the conjunction of 128
transactions **plus** the claim that all 128 were handed the same row. The first
half is enforced by Script. The second half is not.

**Cross-cell agreement is an auditable invariant, not a script-enforced one.**
Auditable because every cell's row is public: it is a constructor argument
baked into the locking script, so anyone can read all 128 locking scripts at
generation *g* and check they carry identical rows. That check is real and it is
cheap. It is just not something a miner would reject a transaction for failing.

This is asserted deliberately, and executably, by
`TestOtherCellsBitsAreNotChecked` in `internal/cellscript/cellscript_test.go`: it
corrupts a bit belonging to another cell and requires the transaction to
*succeed*.

In practice the engine computes each row locally and hands the same one to all
128 cells, so they do agree. That is a property of this client, not of the
covenant. A different client could diverge and the chain would not object.

The UI says `latest row  N/128 proved`, counting cells of the newest generation
whose transition a node has accepted. It used to say `128/128 agree`, which
named the one property the covenant does not establish. Do not reintroduce that
wording anywhere.

---

## How it works

- **`contracts/Cell.runar.go`** is both the Rúnar contract compiled to Bitcoin
  Script *and* ordinary runnable Go, so `go test ./contracts` executes the same
  logic natively. One source, two compilers.
- **The ring wrap costs nothing.** Each cell is a distinct binding of one
  compiled artifact, differing only in six baked constants (a byte index and a
  divisor per neighbour). Cell 0's right neighbour is cell 127 purely because
  its `DivR` constant says so — no branch, no special case, no extra opcode.
- **The rule table is one number.** `Rule` is a constructor argument, and bit
  `4l + 2c + r` of it *is* the transition function. Passing `-rule 30` deploys a
  different automaton with the same script.
- **Chronicle is required.** Rúnar's injected `checkPreimage` preamble emits
  `OP_2MUL`, re-enabled only in the Chronicle upgrade. A verifier running
  Genesis rules rejects these scripts with "attempt to execute disabled opcode
  OP_2MUL". Hence `-chronicle` (default on) and
  `storage.WithChronicleOpcodes()`.
- **The clock is decoupled from confirmation.** Rows are computed locally and
  immediately; the chain then proves them, and confirmation status washes down
  the diagram behind the leading edge. See the `engine` package doc.

---

## Building

```sh
git clone <this repo> && cd rule-110-arcade
go build ./...
```

That is the whole story now, and it was not always: `go.mod` used to carry three
`replace` directives to absolute paths under `/git`, so the repository compiled
on exactly one machine. Every dependency now resolves from a public git host.
Verified from a clean clone in a temporary directory, with an empty
`GOMODCACHE`, `GOWORK=off` and no sibling checkouts anywhere on disk.

Three `replace` directives remain, and they are pinning, not local paths. The
reasons are in `go.mod`; the short version:

- **`github.com/bsv-blockchain/go-arcade-toolbox` is not a public repository.**
  The module declares that path, but it 404s. The work this application depends
  on — `WithRequiredChangeOutput`, `WithGenesisActivationHeight`,
  `WithMinBroadcastFeeRate`, `WithChronicleOpcodes` — is unreleased on the
  `galt-tr` fork, so there is no tag to pin to instead. The replace redirects the
  unreachable path at a specific fork commit.
- **Runar is pinned to a commit for correctness, not convenience.**
  `packages/runar-go/v0.4.5` predates `e7221a7b`, which fixed two
  miscompilations that produce *unspendable contracts*. Pinning to the published
  tag would silently emit scripts that cannot be spent — the coins would be gone
  and the compiler would not have complained.
- **Both Runar modules are pinned to the same commit**, because they import each
  other (`packages/runar-go` needs `compilers/go/codegen`;
  `compilers/go/compiler` needs `packages/runar-go/bn254witness`). Upstream
  resolves that with a `go.work`, and the published `go.mod` points
  `compilers/go` at `v1.0.0-rc.1`, a tag that was never pushed — minimal version
  selection picks it and then cannot download it.

### The build needs cgo

Not for the databases. `modernc.org/sqlite` is transpiled C with no libc
dependency and `pgx` never had one, so neither driver needs it. The requirement
is that this application compiles the contract to Script **at runtime**, out of
the source embedded by `contracts/embed.go`, so the binary carries the Rúnar
compiler — whose frontend parses Go with tree-sitter, a C library.
`CGO_ENABLED=0 go build ./...` fails with `undefined: Node`.

The `Dockerfile` deals with this by linking statically against musl. Compiling
the contract at build time and embedding the artifact instead of the source
would remove the compiler and the C toolchain with it; that is a change to the
application, not to its packaging, and has not been made.

---

## Runbook

**These steps are ordered.** Nothing works out of sequence, and two of the
orderings are not obvious: the funding payment must be *mined* before `fund`
will take it, and `fuel` should come before `genesis` so the ring has coin to
move on from the moment it exists.

`rule110 help` prints the same sequence.

### 1. Get the funding address

```sh
rule110 address -data-dir ./data -network tstn
```

Offline on purpose — no arcade instance is needed, so the address can be handed
out before any service is reachable. It prints the address, and creates
`./data/keys.json` if it is not already there.

### 2. Send coin to it, from any wallet

The payer does not need to speak BRC-29. The wallet plays both sides of the
derivation itself (see `Identity.FunderKeyHex`), so an ordinary payment to an
ordinary address is enough.

### 3. Wait for that payment to be mined

Not optional, and not merely a good idea: step 4 verifies a merkle proof against
the headers service. A payment sitting in the mempool cannot be internalized.

### 4. Internalize it

```sh
rule110 fund -tx <tx-hex> -bump <bump-hex> -arcade-url https://... -data-dir ./data
```

**Both `-tx` and `-bump` are required.** `-bump` is the BUMP merkle path that
proves the transaction was mined; without it the command fails. (The next-step
line printed by `address` omitted `-bump` until recently and therefore could not
work as printed.)

`-tx` takes raw or Extended Format hex. `-vout` selects the output paying this
wallet, defaulting to 0.

### 5. Mint the fuel pool

```sh
rule110 fuel -arcade-url https://... -data-dir ./data
```

This is the difference between an automaton and a queue. The covenant forces
exactly one change output per transaction, so under the default privacy
strategy 128 cells contend for the same change set: measured at **4
transactions per second, with 981 coins in the wallet and exactly one of them
claimable**. A pool of identical, interchangeable coins lets the funder's
`ClaimExact` issue a single `SKIP LOCKED` claim that cannot collide. One coin
funds one transition.

`-sats` must equal the pool denomination `-fuel-sats`, and now defaults to it.
The funder claims coins of *exactly* the denomination (`AND satoshis = ?`), so
a coin of any other value is invisible to it: the mint succeeds, the balance
looks healthy, and every transition then reports "not enough funds" forever.
`rule110 fuel` refuses a mismatch rather than letting that happen.

### 6. Create generation 0

```sh
rule110 genesis -arcade-url https://... -data-dir ./data
```

One transaction with one output per cell. **`-cells` belongs here and only
here** — the ring size is fixed for the life of the deployment, and every later
subcommand reads it back out of the recorded state. `-seed` sets the initial
row; empty seeds a single live cell at index 0. `-rule` picks the Wolfram rule.

### 7. Run it

```sh
rule110 run -arcade-url https://... -data-dir ./data -start -rate 2
```

Serves the UI on `-addr` (default `:8110`), with `/healthz`, `/readyz` and
`/metrics` alongside it. Without `-start` the automaton comes up paused and
waits for the UI.

### Two other subcommands

- `rule110 step -cell N` advances a single cell by hand. Useful for looking at
  one transition in isolation.
- `rule110 depth-probe` measures how deep an unbroken chain of unconfirmed
  transactions the network will accept. **It destroys the cell it probes if the
  limit is real**, which is why it probes one. Run it against any new network
  before choosing `-max-depth`, and stop the engine first — it refuses to run
  while the writer lease is held.

---

## Configuration

Six environment variables, and they are the entire Kubernetes configuration
surface. Each is only the *default* for the matching flag; the flag wins.

| Environment | Flag | Default | Notes |
|---|---|---|---|
| `RULE110_ARCADE_URL` | `-arcade-url` | — | Required. The only endpoint that must be set; the toolbox derives its companions. |
| `RULE110_CHAINTRACKS_URL` | `-chaintracks-url` | derived | Headers service. Empty derives it from the arcade URL. |
| `RULE110_NETWORK` | `-network` | `tstn` | `main` \| `test` \| `ttn` \| `tstn`. Not `mainnet`/`testnet`. Defaults to the private scaling test net, not to real money. |
| `RULE110_DATA_DIR` | `-data-dir` | `./data` | **This IS the wallet.** See below. |
| `RULE110_POSTGRES_DSN` | `-postgres-dsn` | — | Empty uses SQLite. |
| `RULE110_ADDR` | `-addr` | `:8110` | `run` only. |

Flags accepted by every subcommand, beyond those five:

| Flag | Default | What it is for |
|---|---|---|
| `-originator` | `rule110.arcade.local` | BRC-100 originator; must be FQDN-shaped. |
| `-cell-sats` | `1` | Value each cell UTXO carries. Constant across generations — fees come from the fuel pool, not from the cells. |
| `-concurrency` | `32` | How many cells advance at once. Unbounded fan-out makes SQLite worse, not better. |
| `-max-db-conns` | `72` | Storage pool size. Pair with worker count. |
| `-apply-concurrency` | `32` | Monitor workers applying arcade status batches. The toolbox default of 8 is documented as too low: when appliers cannot drain the hand-off queue, the SSE reader blocks and arcade drops events, and transactions end up with no status at all. |
| `-full-status` | `true` | Subscribe to every status transition rather than terminal ones. ~4× the events; turn off above ~3 gen/s. |
| `-chronicle` | `true` | Chronicle-era script rules for local pre-broadcast verification. Required — the covenant contains `OP_2MUL`. |
| `-fee-sat-per-kb` | `125` | Above arcade's 100 sat/kB floor. The margin is headroom for a fee committed from a size *estimate* made before the ~2.6 kB unlocking scripts exist, not a correction for the floor being applied to a larger size. See `Config.FeeSatPerKB`. |
| `-throughput` | `true` | Fund from the denominated fuel pool instead of contending for change. |
| `-fuel-sats` | `1000` | Value of one fuel coin. Must clear one transition's fee plus the dust floor. |
| `-fuel-pool` | `20000` | Coins the keeper maintains. |
| `-max-depth` | `200` | How far a cell may run ahead of its newest mined transaction; 0 is unbounded. A deliberate margin, not a proven boundary — see below. |
| `-max-lag` | `32` | How far the clock may run ahead of the slowest cell. |

Per-subcommand flags:

| Subcommand | Flags |
|---|---|
| `address` | — |
| `fund` | `-tx`, `-bump` (**both required**), `-vout`, `-description` |
| `fuel` | `-count`, `-sats` |
| `genesis` | `-cells`, `-rule`, `-seed` |
| `step` | `-cell` |
| `run` | `-addr`, `-rate`, `-start` |
| `depth-probe` | `-cell`, `-max` |

`Config.MinBroadcastFeeRate` (default 100) has no flag. It is the floor a
finished transaction must clear locally before it is allowed to exist, measured
against the real bytes rather than the plan, and it is the check that catches a
fee committed against a bad size estimate. Set it to the floor the receiving
arcade enforces if you need to change it — it is the network's policy, not a
target rate.

---

## `data/` is the wallet

There is no restore-from-seed in an arcade-only wallet. `data/keys.json` plus
the wallet database *are* the wallet. Delete the directory and the coins are
gone; no phrase, no backup key, no recovery.

Two failure modes deserve naming, because only one of them is loud:

- **Not writable.** `LoadOrCreateIdentity` cannot write `keys.json`, startup
  fails, you find out immediately.
- **Writable but empty.** `LoadOrCreateIdentity` does `os.MkdirAll(dir, 0700)`
  and then, finding no `keys.json`, *generates a new wallet and carries on*. The
  process starts, reports a funding address, and looks entirely healthy — as a
  different wallet, with no access to the previous one's coins. This was
  confirmed by running the container against an empty writable mount: exit 0, a
  fresh `keys.json`, a new address. Under Kubernetes the way to hit it is a
  replaced or unmounted PVC.

So: back it up, mount the same volume every time, and set a retain policy on the
StorageClass.

### If this repository is ever published, rotate the keys

Live private keys were committed to this repository once and had to be purged
from its history. `.gitignore` and `.dockerignore` were hardened afterwards —
`data/`, `data-*/`, `*-backup/`, `*.key`, `keys.json`, `*.db`, `*.tmp`, anywhere
in the tree — because the copy that leaked was named `data-sqlite-backup/` and
slipped past a rule that only matched `/data/`. Do not weaken either file.

Purging history does not un-publish anything that was ever fetched. Any key that
existed in this repository must be treated as compromised: generate a new
`data/` directory, move the coins, and never restore an old one.

---

## Deployment

`Dockerfile` builds a static binary on a `distroless/static:nonroot` base
(~31 MB, runs as UID 65532, no shell). `deploy/k8s/` holds the manifests.

```sh
docker build -t rule110:dev .
kubectl apply -f deploy/k8s/
```

The manifests carry their reasoning in comments. The decisions worth knowing
before reading them:

- **A single-replica StatefulSet, not a Deployment.** This process owns 128 live
  UTXO chains; two instances advancing the same chain double-spend all of them,
  and the resulting rejection is indistinguishable from a genuine failure — that
  is how cells 34 and 51 were lost. A Deployment's RollingUpdate runs the new
  pod *alongside* the old one by design. A StatefulSet terminates the old one
  first. The single-writer lease (`history.AcquireLease`, `engine.holdLease`) is
  the safety net for an overlap, not the strategy. Do not raise `replicas`.
- **`terminationGracePeriodSeconds: 60`**, and 45 is the floor.
  `cmd/rule110/main.go` returns from `web.Serve` and only *then* waits, in a
  deferred function, up to 20 seconds for the engine to drain. The Kubernetes
  default of 30 s can therefore land the SIGKILL inside exactly the window
  `history.StatusAttempting` exists to survive: a transition broadcast but not
  recorded leaves that cell's tip *unknown*, and the engine refuses to advance
  it on the next start rather than risk re-spending a consumed output.
- **`fsGroup: 65532`** is what makes the PVC writable by the image's UID. See
  the two failure modes above for why that matters more than it looks.
- **`POST /api/control` is unauthenticated.** There is no credential check on
  it at all: anyone who can reach the pod can pause the automaton, resume it,
  single-step it, or change its rate. The manifests state the exposure and do
  not invent an auth scheme for it. The Service is a headless ClusterIP; guard
  it above the pod with an ingress auth filter or a NetworkPolicy before
  exposing it.
- **Bootstrap runs as a Job.** Steps 1 and 4–6 of the runbook are one-shot
  commands against the same PVC. `deploy/k8s/bootstrap-job.yaml` is how; scale
  the StatefulSet to 0 first, because the writer lease protects the cell chains
  and nothing else — `fuel` in particular draws on the same pool the running
  engine's keeper is minting into. The image has no shell, so there is nothing
  to `kubectl exec -- sh` into; run the binary directly.

`/metrics` is Prometheus text format, hand-rolled off the engine snapshot
(`web.handleMetrics`). Every gauge there is one this project actually needed to
diagnose something: a stalled automaton with no errors turned out to be 127
cells queueing for one coin (`rule110_waiting_on_coin`), and a frontier that
would not move turned out to be the depth governor working
(`rule110_unconfirmed_depth`).

---

## What was measured

Numbers from this deployment, against the `dev-ovh-1` scaling test network.
They are observations, not benchmarks, and the caveats are part of them.

**No mempool ancestor limit was found.** `rule110 depth-probe` built 600
consecutive transactions on a single chain and reached at least 250 unconfirmed
ancestors with zero rejections. That is consistent with arcade enforcing no
ancestor limit — its `LimitAncestorCount` is a dead value with no setter — and
with teranode's documentation saying ancestor tracking is not enforced. The
cascade recorded in the toolbox's own benchmarks may well have had another
cause. `-max-depth` is kept at 200 as a deliberate margin, not a proven
boundary: other networks may differ, and the bound also caps the in-flight set
the status pipeline has to carry. **Measure it on the network you are actually
running against.**

**Fuel-pool funding took the application from 4 to 267 transactions per
second.** The 4 was not a slow network. It was 128 cells contending for the
unreserved change set, with 981 coins in the wallet and exactly one of them
claimable at any moment. Denominating the pool makes every coin interchangeable,
so the claim never collides. See `Config.utxoManagement`.

**Per-transaction storage fell from ~1.5 MB to ~12 kB.** The cause was handing
the wallet an ever-growing BEEF. A cell is an unbroken self-spending chain, so
its atomic BEEF carries every generation back to genesis: it grew ~11 kB per
generation per cell, reached 694 kB per cell by generation 63, and was passed
straight back to `CreateAction` as `InputBEEF`, which storage then persisted
twice per transaction. The state file reached 175 MB and the wallet database
12 GB after 8,000 transactions. Storing only the immediately-spent transaction
and rebuilding a one-transaction BEEF on demand (`CellChain.RawTxHex`,
`tipBEEF`) is sufficient, because `hydrateInputs` only looks the source up by
hash and arcade validates from each input's inline prevout.

**PostgreSQL over SQLite is the single biggest throughput lever**, on the
toolbox's own numbers: ~57–108 TPS against ~575. SQLite serialises every write,
so 128 cells advancing at once thrash one writer lock. Empty
`RULE110_POSTGRES_DSN` is fine for a small ring and wrong for this one.

Not measured, and worth being explicit about: nothing here establishes an upper
bound on generation rate, and the 267 tx/s figure is one run on one network with
one set of tuning. Treat it as evidence that the bottleneck was coin selection,
not as a throughput specification.

---

## Development

```sh
go build ./...
go vet ./...
go test ./... -race
gofmt -l .
```

All four are clean and CI (`.github/workflows/ci.yml`) runs all four. `-race`
rather than plain `go test` because the engine is a clock, 128 cell workers, a
status pipeline and an HTTP server sharing one snapshot; a real bug lived there
(`Snapshot` shared the per-generation `Cells` backing arrays with the live
engine while they were being marshalled).

The tests are offline. `go test ./contracts` runs the automaton as native Go,
and `internal/cellscript` executes the compiled Script through the go-sdk
interpreter with Chronicle enabled — the same check the toolbox performs before
broadcasting, except the toolbox's default verifier does not enable Chronicle
and so rejects these scripts on `OP_2MUL`.

If you touch `internal/web/static/app.js`, check it parses: `node --check`.
