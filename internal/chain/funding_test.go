package chain

import (
	"encoding/hex"
	"testing"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/defs"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

// fundingTarget builds a real identity in a temp dir and derives its funding
// target, so these tests exercise the same derivation the deployment uses
// rather than a hand-written script.
func fundingTarget(t *testing.T) *FundingTarget {
	t.Helper()
	id, err := LoadOrCreateIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	target, err := FundingAddress(id, defs.NetworkTTN)
	if err != nil {
		t.Fatalf("FundingAddress: %v", err)
	}
	return target
}

// payTo appends an output paying the given script hex.
func payTo(t *testing.T, tx *transaction.Transaction, scriptHex string, sats uint64) {
	t.Helper()
	s, err := script.NewFromHex(scriptHex)
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	tx.AddOutput(&transaction.TransactionOutput{LockingScript: s, Satoshis: sats})
}

// decoyScript is a locking script that is emphatically not ours.
const decoyScript = "76a914000000000000000000000000000000000000000088ac"

// TestMatchIgnoresTheClaimedIndex is the guard for the one thing a public
// funding endpoint must never do: believe the payer about which output pays it.
//
// BRC-100's createAction randomises output order by default, so the payer's own
// wallet decides where the payment lands and does not tell them. An index in
// the request is therefore either redundant or a lie, and the derived script is
// the only identification we can compute ourselves.
func TestMatchIgnoresTheClaimedIndex(t *testing.T) {
	target := fundingTarget(t)

	tx := transaction.NewTransaction()
	payTo(t, tx, decoyScript, 1000)
	payTo(t, tx, decoyScript, 2000)
	payTo(t, tx, decoyScript, 3000)
	payTo(t, tx, target.LockingScriptHex, 50_000)

	indices, sats := MatchFundingOutputs(tx, target)
	if len(indices) != 1 || indices[0] != 3 {
		t.Errorf("matched %v, want just index 3", indices)
	}
	if sats != 50_000 {
		t.Errorf("matched %d satoshis, want 50000", sats)
	}
}

// A transaction may legitimately pay the funding script more than once — the
// address is fixed, so nothing stops a wallet splitting a payment. All of them
// are ours and all of them must be credited, or the payer is short-changed.
func TestMatchFindsEveryPayingOutput(t *testing.T) {
	target := fundingTarget(t)

	tx := transaction.NewTransaction()
	payTo(t, tx, target.LockingScriptHex, 10_000)
	payTo(t, tx, decoyScript, 999)
	payTo(t, tx, target.LockingScriptHex, 25_000)

	indices, sats := MatchFundingOutputs(tx, target)
	if len(indices) != 2 || indices[0] != 0 || indices[1] != 2 {
		t.Errorf("matched %v, want indices 0 and 2", indices)
	}
	if sats != 35_000 {
		t.Errorf("matched %d satoshis, want 35000 (both outputs)", sats)
	}
}

// A transaction that pays somebody else must match nothing at all, rather than
// falling back to "the first output" or "the largest".
func TestMatchRefusesATransactionThatPaysSomebodyElse(t *testing.T) {
	target := fundingTarget(t)

	tx := transaction.NewTransaction()
	payTo(t, tx, decoyScript, 1_000_000)

	if indices, sats := MatchFundingOutputs(tx, target); len(indices) != 0 || sats != 0 {
		t.Errorf("matched %v / %d satoshis on a transaction that does not pay us", indices, sats)
	}
}

// Two deployments have different identities and therefore different funding
// scripts. Neither may credit the other's payment.
func TestMatchIsScopedToThisDeployment(t *testing.T) {
	mine := fundingTarget(t)
	theirs := fundingTarget(t)

	if mine.LockingScriptHex == theirs.LockingScriptHex {
		t.Fatal("two fresh identities derived the same funding script")
	}

	tx := transaction.NewTransaction()
	payTo(t, tx, theirs.LockingScriptHex, 100_000)

	if indices, _ := MatchFundingOutputs(tx, mine); len(indices) != 0 {
		t.Errorf("matched %v against another deployment's funding output", indices)
	}
}

// TestAtomicBEEFRoundTrips pins the exact conversion InternalizeAction demands.
//
// The wallet hands the browser a BEEF; InternalizeAction parses ATOMIC BEEF and
// nothing else — NewBeefFromTransaction's V2 output fails validation with
// "version 4022206466 is not atomic BEEF". This is the one line of the funding
// path where getting the container format wrong produces an error message that
// points nowhere near the cause.
func TestAtomicBEEFRoundTrips(t *testing.T) {
	target := fundingTarget(t)

	tx := transaction.NewTransaction()
	payTo(t, tx, target.LockingScriptHex, 42_000)

	atomic, err := tx.AtomicBEEF(true)
	if err != nil {
		t.Fatalf("AtomicBEEF: %v", err)
	}

	beef, subject, err := transaction.NewBeefFromAtomicBytes(atomic)
	if err != nil {
		t.Fatalf("InternalizeAction would reject these bytes: %v", err)
	}
	if beef == nil || subject == nil {
		t.Fatal("atomic BEEF parsed to no subject transaction")
	}
	if subject.String() != tx.TxID().String() {
		t.Errorf("round trip named %s, want %s", subject, tx.TxID())
	}
}

// ParseBeef is what accepts whatever shape a wallet hands us. It must find the
// same subject transaction in the atomic form the CLI path builds.
func TestParseBeefFindsTheSubjectTransaction(t *testing.T) {
	target := fundingTarget(t)

	tx := transaction.NewTransaction()
	payTo(t, tx, target.LockingScriptHex, 7_000)

	atomic, err := tx.AtomicBEEF(true)
	if err != nil {
		t.Fatalf("AtomicBEEF: %v", err)
	}

	_, got, hash, err := transaction.ParseBeef(atomic)
	if err != nil {
		t.Fatalf("ParseBeef: %v", err)
	}
	if got == nil || hash == nil {
		t.Fatal("ParseBeef found no subject transaction")
	}
	if hash.String() != tx.TxID().String() {
		t.Errorf("subject = %s, want %s", hash, tx.TxID())
	}

	// And the scan still finds our output through that round trip, which is the
	// property AcceptPayment actually relies on.
	if indices, sats := MatchFundingOutputs(got, target); len(indices) != 1 || sats != 7_000 {
		t.Errorf("after a BEEF round trip the scan matched %v / %d satoshis", indices, sats)
	}
}

// The funding script must be a real, parseable locking script — it is handed to
// a browser to put in a transaction, so a malformed one fails in somebody
// else's wallet where nothing here can explain it.
func TestFundingScriptIsWellFormed(t *testing.T) {
	target := fundingTarget(t)

	raw, err := hex.DecodeString(target.LockingScriptHex)
	if err != nil {
		t.Fatalf("funding script is not hex: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("funding script is empty")
	}
	if _, err := script.NewFromHex(target.LockingScriptHex); err != nil {
		t.Errorf("funding script does not parse: %v", err)
	}
	if target.Address == "" {
		t.Error("no funding address derived")
	}
}
