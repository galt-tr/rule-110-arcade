package cellscript

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// verdictCacheSize bounds the memo.
//
// The two calls it exists to collapse happen microseconds apart inside one
// SignAction, so almost any size works; this one holds over a second of
// history at the highest rate this application has reached, which is enough
// that an entry is never evicted before its second read. It is a bound against
// the leak, not a working set to tune.
const verdictCacheSize = 1024

// ScriptsVerifier is the toolbox's script verifier, with the covenant executed
// once per transaction instead of twice.
//
// # Why this exists
//
// Broadcasting one cell transition runs the ~2.6 kB covenant through the script
// interpreter three times. Ours, before broadcasting, is one — see
// AdvanceCell, and it has to stay: it is what distinguishes a covenant we built
// wrong from a transaction the network refused, which decides whether the cell
// retries or halts. The other two are both inside SignAction, in
// storage.processNewTx and again in storage.broadcastOne, and they judge THE
// SAME FULLY-SIGNED TRANSACTION. The second is pure repetition.
//
// Measured at ~0.9 ms per execution, against ~2.7 ms of build for the whole
// rest of a transition, so this is the largest single piece of CPU on the path
// and the only one duplicated outright.
//
// It cannot collapse to one. Our pre-broadcast check runs before the wallet has
// signed its own funding input, so that transaction is not the one that gets
// broadcast — it hashes differently, and it would not even pass a whole-
// transaction verification, because input 1 has no signature yet.
//
// # Why it is safe to memoise on the txid
//
// A txid commits to every byte of the transaction including its unlocking
// scripts, so two calls that agree on the txid are judging identical scripts
// against identical inputs, and the verdict cannot differ. The source outputs
// are hydrated from the same stored BEEF both times.
//
// # The era flags are a copy, and that is the risk here
//
// storage's own verifier picks interpreter flags per input, and supplying a
// custom verifier turns that logic off — WithChronicleOpcodes documents that it
// "has no effect when WithScriptsVerifier supplies a custom verifier". So the
// flags below are a deliberate reproduction of what the default verifier
// produces for THIS deployment's configuration, and they are only faithful
// because this application never calls WithGenesisActivationHeight: with no
// activation height, the default's per-input pre-Genesis selection is disabled
// and every input gets the same era. If that ever changes, this type has to
// learn the same rule or it will judge a pre-Genesis input by post-Genesis
// limits. TestVerifierAppliesTheConfiguredEra is the reminder that the era
// reaches the interpreter at all; nothing can test a rule this does not have.
type ScriptsVerifier struct {
	// chronicle selects Chronicle-era rules, and must track chain.Config's own
	// setting rather than being assumed: Rúnar covenants contain OP_2MUL and do
	// not verify without it, but a Genesis-rules network is a legitimate, if
	// doomed, thing to point this at.
	chronicle bool

	mu   sync.Mutex
	seen map[chainhash.Hash]error
	// fifo is the eviction order. A plain queue rather than an LRU: entries are
	// read once, moments after they are written, so recency carries no
	// information worth the bookkeeping.
	fifo []chainhash.Hash

	// executions counts interpreter runs, which is the only way to observe from
	// the outside that the memo did anything: the verdict is identical either
	// way, so a test asserting only on verdicts would pass against a cache that
	// never hits.
	executions atomic.Uint64
}

// Executions is how many times the interpreter has actually run. It exists for
// the tests, and for anyone wanting to confirm on a live process that the memo
// is being hit — it should sit at about half the transactions broadcast.
func (v *ScriptsVerifier) Executions() uint64 { return v.executions.Load() }

// NewScriptsVerifier returns a verifier for the given script era.
func NewScriptsVerifier(chronicle bool) *ScriptsVerifier {
	return &ScriptsVerifier{
		chronicle: chronicle,
		seen:      make(map[chainhash.Hash]error, verdictCacheSize),
	}
}

// VerifyScripts runs every input's script pair, or returns the verdict already
// reached for this transaction.
//
// The signature is storage's wdk.ScriptsVerifier, satisfied structurally so
// this package needs no dependency on the toolbox.
func (v *ScriptsVerifier) VerifyScripts(_ context.Context, tx *transaction.Transaction) (bool, error) {
	if tx == nil {
		return false, fmt.Errorf("cellscript: nil transaction")
	}
	txid := *tx.TxID()

	if err, ok := v.lookup(txid); ok {
		return err == nil, err
	}
	err := v.execute(tx)
	v.remember(txid, err)
	return err == nil, err
}

// execute runs the interpreter over every input, which is what storage's
// default verifier does and what its callers rely on: the funding input's
// signature is checked here too, not only the covenant.
func (v *ScriptsVerifier) execute(tx *transaction.Transaction) error {
	v.executions.Add(1)
	engine := interpreter.NewEngine()
	for i := range tx.Inputs {
		src := tx.Inputs[i].SourceTxOutput()
		if src == nil {
			return fmt.Errorf("cellscript: input %d has no source output to verify against", i)
		}
		if err := engine.Execute(v.options(tx, i, src)...); err != nil {
			return fmt.Errorf("cellscript: script verification failed for input %d: %w", i, err)
		}
	}
	return nil
}

// options are the interpreter flags for one input. See the type comment for why
// there is no per-input era selection here.
func (v *ScriptsVerifier) options(
	tx *transaction.Transaction, i int, src *transaction.TransactionOutput,
) []interpreter.ExecutionOptionFunc {
	opts := []interpreter.ExecutionOptionFunc{
		interpreter.WithTx(tx, i, src),
		interpreter.WithForkID(),
	}
	if v.chronicle {
		// Chronicle implies after-genesis, so the two are exclusive rather than
		// additive.
		return append(opts, interpreter.WithAfterChronicle())
	}
	return append(opts, interpreter.WithAfterGenesis())
}

func (v *ScriptsVerifier) lookup(txid chainhash.Hash) (error, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	err, ok := v.seen[txid]
	return err, ok
}

func (v *ScriptsVerifier) remember(txid chainhash.Hash, err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.seen[txid]; ok {
		return // another goroutine got there first; do not double-queue the key
	}
	if len(v.fifo) >= verdictCacheSize {
		delete(v.seen, v.fifo[0])
		v.fifo = v.fifo[1:]
	}
	v.seen[txid] = err
	v.fifo = append(v.fifo, txid)
}
