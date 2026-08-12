#!/usr/bin/env bash
#
# Reset the automaton back to a fresh genesis.
#
# READ THIS FIRST, because the obvious mental model is wrong.
#
# The wallet is NOT in ./data. It is in PostgreSQL, in the same database as the
# automaton's history — `outputs`, `utxos`, `transactions`, `known_txs` and
# `output_baskets` are the wallet's own tables, sitting alongside `cell_txs` and
# `generations`. ./data holds only keys.json (the private keys), state.json (the
# deployment facts) and some stale SQLite WAL files from before the move to
# PostgreSQL.
#
# So "drop the database container" spends nothing and destroys everything: the
# coin is in that container, and dropping it is indistinguishable from burning
# the wallet. Controlling the funds needs BOTH keys.json and the utxos table,
# and this wallet has no chain rescan to rebuild the latter.
#
# Hence two modes, and the safe one is the default:
#
#   ./scripts/reset.sh                 automaton only. Truncates the history
#                                      tables and removes state.json. The wallet,
#                                      its coin and the funding address all
#                                      survive, so there is nothing to re-fund
#                                      and the bootstrap is two commands.
#
#   ./scripts/reset.sh --wipe-wallet   everything, including the money. Destroys
#                                      the container and the keys. Requires
#                                      typing the balance to confirm.
#
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

CONTAINER=${RULE110_DB_CONTAINER:-rule110-db}
PORT=${RULE110_DB_PORT:-5434}
DB=${RULE110_DB_NAME:-rule110}
DSN="postgres://postgres:postgres@127.0.0.1:${PORT}/${DB}?sslmode=disable"
ARCADE=${RULE110_ARCADE_URL:-https://arcade-v2.dev-ovh-1.ubsv.dev}
DATA=${RULE110_DATA_DIR:-./data}
UI=${RULE110_ADDR:-localhost:8110}
BACKUPS=${RULE110_BACKUP_DIR:-./.reset-backups}

WIPE_WALLET=0
[[ ${1:-} == "--wipe-wallet" ]] && WIPE_WALLET=1

say()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
warn() { printf '\033[31m%s\033[0m\n' "$*"; }

psql_q() { PGPASSWORD=postgres psql -h 127.0.0.1 -p "$PORT" -U postgres -d "$DB" -tAc "$1"; }

# ---------------------------------------------------------------- what is at stake
# Read the balance from the database rather than the running engine, so this
# works whether or not the automaton is up — and so a reset can never be talked
# into proceeding by an API that happens to be down.
#
# The column is `spent_by IS NULL`, and getting that wrong is not a cosmetic
# bug: the first version asked `spent_at`, which does not exist, swallowed the
# error and reported 0 satoshis — so --wipe-wallet would have said nothing was
# at stake and asked you to confirm by typing "0" while sitting on 28 BSV.
# Hence the explicit failure below rather than `|| echo 0`.
RESERVE=0
DB_UP=0
if psql_q "SELECT 1" >/dev/null 2>&1; then
  DB_UP=1
  if ! RESERVE=$(psql_q "SELECT coalesce(sum(satoshis),0) FROM utxos WHERE spent_by IS NULL"); then
    warn "Could not read the wallet balance from ${DB}."
    warn "Refusing to continue: this script must not offer to destroy an amount it cannot state."
    exit 1
  fi
fi
BSV=$(awk -v s="$RESERVE" 'BEGIN{printf "%.8f", s/1e8}')

say "rule110 reset"
echo "  container    ${CONTAINER} (port ${PORT}, database ${DB})"
echo "  data dir     ${DATA}"
echo "  arcade       ${ARCADE}"
echo "  unspent coin ${RESERVE} sat  (~${BSV} BSV)"
if (( DB_UP )); then
  psql_q "SELECT '    ' || basket || ': ' || count(*) || ' coins, ' || sum(satoshis) || ' sat'
          FROM utxos WHERE spent_by IS NULL GROUP BY basket ORDER BY sum(satoshis) DESC" 2>/dev/null || true
fi

if (( WIPE_WALLET )); then
  warn ""
  warn "  --wipe-wallet DESTROYS THE WALLET."
  warn ""
  warn "  The ${RESERVE} satoshis above are held in this container's utxos table and"
  warn "  spent with the key in ${DATA}/keys.json. This removes both. There is no"
  warn "  seed phrase and no chain rescan, so the coin is not recoverable — not by"
  warn "  restoring a backup of one half, not by re-running genesis."
  warn ""
  warn "  If you want a fresh automaton, you almost certainly want the default mode"
  warn "  instead: it keeps the coin and the funding address, so there is nothing"
  warn "  to re-fund."
  warn ""
  echo  "  keys.json and a full dump will be copied to ${BACKUPS}/ first, but treat"
  echo  "  that as a courtesy and not a plan."
  echo
  read -r -p "  Type the satoshi figure above to confirm: " CONFIRM
  if [[ "$CONFIRM" != "$RESERVE" ]]; then
    echo "  Mismatch — nothing was touched."
    exit 1
  fi
else
  echo
  echo "  Automaton only: the wallet, its coin and the funding address survive."
  echo "  Pass --wipe-wallet to destroy those too."
  echo
  read -r -p "  Reset the automaton? [y/N] " CONFIRM
  [[ ${CONFIRM,,} == y ]] || { echo "  Nothing was touched."; exit 1; }
fi

# ---------------------------------------------------------------- stop the engine
# Match the binary, not the string "rule110 run" — a pattern that loose also
# matches this script's own shell, which is a good way to kill the reset midway
# through and leave the deployment in a state nobody planned.
say "Stopping the engine"
if pgrep -f '[.]/rule110 run' >/dev/null 2>&1; then
  pkill -f '[.]/rule110 run' || true
  for _ in $(seq 1 30); do
    pgrep -f '[.]/rule110 run' >/dev/null 2>&1 || break
    sleep 1
  done
  echo "  stopped"
else
  echo "  not running"
fi

mkdir -p "$BACKUPS"
STAMP=$(date +%Y%m%d-%H%M%S)

if (( WIPE_WALLET )); then
  # ------------------------------------------------------------- full wipe
  say "Backing up before destroying"
  if [[ -f "${DATA}/keys.json" ]]; then
    cp "${DATA}/keys.json" "${BACKUPS}/keys-${STAMP}.json"
    chmod 600 "${BACKUPS}/keys-${STAMP}.json"
    echo "  keys  -> ${BACKUPS}/keys-${STAMP}.json"
  fi
  if psql_q "SELECT 1" >/dev/null 2>&1; then
    PGPASSWORD=postgres pg_dump -h 127.0.0.1 -p "$PORT" -U postgres -d "$DB" \
      --no-owner --no-acl -Fc -f "${BACKUPS}/db-${STAMP}.dump"
    echo "  db    -> ${BACKUPS}/db-${STAMP}.dump"
  fi

  say "Destroying the database container"
  podman rm -f "$CONTAINER" >/dev/null 2>&1 || true
  echo "  removed ${CONTAINER}"

  say "Deleting ${DATA}"
  rm -rf "${DATA:?}"
  mkdir -p "$DATA"
  echo "  gone (a new identity is created on the next command)"
else
  # ------------------------------------------------------------- automaton only
  say "Clearing the automaton's history"
  if ! psql_q "SELECT 1" >/dev/null 2>&1; then
    echo "  database is not up; it will start empty of history below"
  else
    # Only the automaton's own tables. Everything else in this database is the
    # wallet, and truncating it is the same as burning the coin.
    psql_q "TRUNCATE cell_txs, generations, leases" >/dev/null
    echo "  truncated cell_txs, generations, leases (wallet tables untouched)"
  fi

  say "Removing the deployment facts"
  rm -f "${DATA}/state.json"
  echo "  ${DATA}/state.json removed — genesis will write a new one"
fi

# Stale SQLite WAL from before the wallet moved into PostgreSQL. Large, unused,
# and confusing to anyone trying to work out where the wallet lives.
rm -f "${DATA}"/wallet.db "${DATA}"/wallet.db-shm "${DATA}"/wallet.db-wal 2>/dev/null || true

# ---------------------------------------------------------------- bring it back up
say "Starting the database container"
if podman inspect "$CONTAINER" >/dev/null 2>&1; then
  podman start "$CONTAINER" >/dev/null
  echo "  started existing ${CONTAINER}"
else
  podman run -d --name "$CONTAINER" -p "127.0.0.1:${PORT}:5432" \
    -e POSTGRES_PASSWORD=postgres -e POSTGRES_USER=postgres -e POSTGRES_DB="$DB" \
    -e POSTGRES_INITDB_ARGS="-c max_connections=300" \
    docker.io/library/postgres:16 >/dev/null
  echo "  created ${CONTAINER}"
fi
printf '  waiting for postgres'
until psql_q "SELECT 1" >/dev/null 2>&1; do printf '.'; sleep 1; done
echo " ready"

# NOT config. The app takes the DSN from -postgres-dsn or RULE110_POSTGRES_DSN
# and defaults to SQLite in the data dir; nothing reads a file. The old
# deployment had a data/postgres.dsn sitting next to a wallet that really was in
# PostgreSQL, which reads exactly like configuration and is not — the flag was
# being passed on every command instead.
#
# That misreading has teeth. Bootstrapping without the DSN puts the wallet in
# SQLite, silently, and it works: `fund` reports the balance and `fuel` starts
# minting. You find out at 128-cell fan-out, which is where SQLite is far too
# slow, by which point the coin is in the wrong store and the funding output is
# spent.
#
# So write an env file to SOURCE, which cannot be mistaken for something the app
# picks up on its own.
rm -f "${DATA}/postgres.dsn"
cat > "${DATA}/env" <<ENVEOF
# source this before running rule110, or the wallet goes to SQLite:
#   source ${DATA}/env
export RULE110_POSTGRES_DSN='${DSN}'
export RULE110_ARCADE_URL='${ARCADE}'
ENVEOF
echo "  wrote ${DATA}/env — source it, nothing reads it automatically"

go build -o rule110 ./cmd/rule110
echo "  rebuilt ./rule110"

# ---------------------------------------------------------------- what to do next
# Print a `source` line rather than repeating flags on every command: a reader
# who copies six commands will drop the flag on one of them, and dropping it is
# silent — the wallet just goes to SQLite instead.
SRC="source ${DATA}/env"

if (( WIPE_WALLET )); then
  cat <<EOF

$(printf '\033[1m%s\033[0m' "Next: fund the new wallet, then create genesis")

The identity is gone, so this is a new deployment with a new funding address and
no coin. The subcommands are a sequence, not a menu.

  0. FIRST, in every shell you use:

       ${SRC}

     Without RULE110_POSTGRES_DSN the wallet silently goes to SQLite, which is
     far too slow for a 128-cell ring — and you find out after the coin is in it.

  1. Print the new funding address (offline, needs no arcade):

       ./rule110 address

  2. Send coin to it from any wallet. Budget for the automaton you want:
     one transition costs ~1000 sat of fuel, so 128 cells x N generations is
     roughly 128 x N x 1000 sat, plus the fuel pool below.

  3. WAIT for that payment to be MINED. Not seen — mined. The next step
     verifies a merkle proof and cannot succeed before there is one.

  4. Internalize it. BOTH flags are required:

       ./rule110 fund -tx <raw-tx-hex> -bump <merkle-proof-hex>

  5. Mint the fuel pool. Each cell transition spends exactly one coin, so this
     is what stops 128 cells contending for change:

       ./rule110 fuel -count 20000

  6. Create generation 0 — one covenant UTXO per cell:

       ./rule110 genesis -cells 128 -rule 110

  7. Start it:

       ./rule110 run

     then open http://${UI} and press play. The engine boots PAUSED by design.
EOF
else
  cat <<EOF

$(printf '\033[1m%s\033[0m' "Next: create genesis. No funding needed")

The wallet survived, so steps 1-4 of the usual bootstrap do not apply: the coin
is already there and the funding address is unchanged.

  0. FIRST, in every shell you use:

       ${SRC}

     Without RULE110_POSTGRES_DSN the app defaults to SQLite and will not see
     this wallet at all.

  1. Top the fuel pool back up if it is low (genesis and the keeper both draw on
     it; skip if the pool is already stocked):

       ./rule110 fuel -count 20000

  2. Create generation 0 — one covenant UTXO per cell:

       ./rule110 genesis -cells 128 -rule 110

  3. Start it:

       ./rule110 run

     then open http://${UI} and press play. The engine boots PAUSED by design.

Worth knowing on a fresh ring: a cell halted by an arcade refusal stays halted
until you run \`./rule110 recover\` (dry run unless -apply). Those refusals are
intermittent rather than deterministic, so a halted cell is usually recoverable
by retrying it — see task #37.
EOF
fi

echo
