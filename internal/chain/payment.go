package chain

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bsv-blockchain/go-sdk/transaction"
)

// The failure modes a public payer can reach. They are sentinels because the
// HTTP layer maps them onto status codes and, more importantly, onto messages a
// stranger can act on: "your wallet is on the wrong chain" and "we already have
// this one" are different situations with different remedies, and collapsing
// them into a 500 wastes the payer's time and possibly their coins.
var (
	ErrNoPaymentOutput = errors.New("chain: no output in this transaction pays this deployment")
	ErrPaymentTooSmall = errors.New("chain: payment below the minimum")
	ErrPaymentKnown    = errors.New("chain: this payment has already been credited")
	ErrPaymentRefused  = errors.New("chain: the network refused this payment")
	ErrBadPayment      = errors.New("chain: the payment could not be parsed")
)

// acceptMu serialises AcceptPayment.
//
// Not for throughput — funding is rare — but because the duplicate check and
// the credit have to be one step. Two concurrent posts of the same transaction
// would both see it as unknown and both internalize it, and a second
// internalize of the same transaction inserts a SECOND outputs row for the same
// outpoint: storage's mint returns early on the outpoint conflict without
// crediting anything, while the metastore insert has no conflict clause at all.
// The result is a reported balance that grew and a wallet that gained nothing.
var acceptMu sync.Mutex

// KnowsTx reports whether the wallet already holds this transaction.
//
// Asked BEFORE internalizing rather than relying on the wallet to reject a
// duplicate, because it does not reject one — see acceptMu.
func (c *Chain) KnowsTx(ctx context.Context, txid string) (bool, error) {
	raw, err := c.RawTx(ctx, txid)
	if err != nil {
		// Not found, by any route, is the answer we want and not an error.
		return false, nil
	}
	return len(raw) > 0, nil
}

// BroadcastForeign submits a transaction this wallet did not build, through
// this deployment's own arcade.
//
// FindTransactionForSigning wires each direct input's SourceTransaction, which
// is what lets EF() succeed; this mirrors services.PostFromBEEF, which is how
// the toolbox broadcasts everything else.
func (c *Chain) BroadcastForeign(ctx context.Context, beef *transaction.Beef, txid string) error {
	tx := beef.FindTransactionForSigning(txid)
	if tx == nil {
		return fmt.Errorf("%w: transaction %s is not in the supplied BEEF", ErrBadPayment, txid)
	}
	if tx.MerklePath != nil {
		return nil // already mined; there is nothing to broadcast.
	}

	ef, err := tx.EF()
	if err != nil {
		return fmt.Errorf("%w: your wallet returned a transaction without its input history, "+
			"so it cannot be broadcast: %v", ErrBadPayment, err)
	}

	res, err := c.Oracle.Broadcast(ctx, txid, ef)
	if err != nil {
		// Transport, backpressure or an open circuit: the transaction's fate is
		// unknown and it may never have been queued. Nothing has been credited,
		// so the honest thing is to refuse and let the payer retry.
		return fmt.Errorf("%w: %v", ErrPaymentRefused, err)
	}
	if res != nil && res.Rejected {
		// A verdict, and a durable one. On this deployment the overwhelmingly
		// likely cause is a payer whose coins are on a different chain: a
		// BRC-100 wallet reports only "testnet", which does not distinguish
		// teratestnet from any other test network, and the address version byte
		// is shared. Their inputs do not exist in our UTXO set.
		return fmt.Errorf("%w: %s", ErrPaymentRefused, res.ExtraInfo)
	}
	return nil
}

// AcceptPayment verifies, broadcasts and credits a payment built by an
// arbitrary BRC-100 wallet.
//
// THE ORDER IS THE SAFETY ARGUMENT. Match the script, check the floor, refuse a
// duplicate, BROADCAST, and only then credit. Crediting first would mint a coin
// against a transaction the network may never have seen, and the fuel keeper
// would then spend a phantom — every transition built on it failing with a
// missing input, one per cell, with nothing pointing back here.
//
// It also depends on the browser having asked its wallet NOT to send. That is
// what makes the broadcast ours, which matters twice over: our arcade is the
// only thing that can tell a teratestnet coin from a testnet one before the
// payer's money moves, and a transaction we broadcast carries our callback
// token, so its mined verdict arrives on the monitor's existing stream. A
// payment somebody else broadcast is invisible to our arcade and would sit
// unproven forever. A wallet that ignores noSend still works — arcade's
// re-submit is idempotent — so this is a safety property, not a requirement.
func (c *Chain) AcceptPayment(ctx context.Context, beefBytes []byte, minSatoshis uint64) (*InternalizeResult, error) {
	target, err := FundingAddress(c.Identity, c.Config.Network)
	if err != nil {
		return nil, err
	}

	beef, tx, txidHash, err := transaction.ParseBeef(beefBytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadPayment, err)
	}
	if tx == nil || txidHash == nil {
		return nil, fmt.Errorf("%w: the BEEF names no subject transaction", ErrBadPayment)
	}

	indices, satoshis := MatchFundingOutputs(tx, target)
	if len(indices) == 0 {
		return nil, ErrNoPaymentOutput
	}
	if satoshis < minSatoshis {
		return nil, fmt.Errorf("%w: %d satoshis, minimum is %d", ErrPaymentTooSmall, satoshis, minSatoshis)
	}

	// Atomic BEEF, keeping the ancestry: VerifyBeef inside InternalizeAction
	// needs every transaction without a merkle path to trace back to one that
	// has it, and that ancestry is exactly what the wallet supplied.
	atomic, err := beef.AtomicBytes(txidHash)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadPayment, err)
	}

	txid := txidHash.String()

	acceptMu.Lock()
	defer acceptMu.Unlock()

	known, err := c.KnowsTx(ctx, txid)
	if err != nil {
		return nil, err
	}
	if known {
		return nil, fmt.Errorf("%w: %s", ErrPaymentKnown, txid)
	}

	if err := c.BroadcastForeign(ctx, beef, txid); err != nil {
		return nil, err
	}
	return c.internalizeOutputs(ctx, atomic, tx, indices, satoshis,
		"public funding via "+txid[:16])
}

// AcceptPaymentByTxID credits a payment somebody made without the button.
//
// Mined payments only, and that is a limit of the evidence rather than a
// policy: fetched by txid there is no ancestry, so a merkle proof is the only
// thing that can establish the transaction is real. An unmined one has to come
// back later.
func (c *Chain) AcceptPaymentByTxID(ctx context.Context, txid string) (*InternalizeResult, error) {
	txid = strings.ToLower(strings.TrimSpace(txid))
	if raw, err := hex.DecodeString(txid); err != nil || len(raw) != 32 {
		return nil, fmt.Errorf("%w: %q is not a transaction id", ErrBadPayment, txid)
	}

	acceptMu.Lock()
	defer acceptMu.Unlock()

	known, err := c.KnowsTx(ctx, txid)
	if err != nil {
		return nil, err
	}
	if known {
		return nil, fmt.Errorf("%w: %s", ErrPaymentKnown, txid)
	}

	rec, err := c.Oracle.GetTx(ctx, txid)
	if err != nil {
		return nil, fmt.Errorf("%w: arcade has no record of %s: %v", ErrBadPayment, txid, err)
	}
	if rec == nil || len(rec.RawTx) == 0 {
		return nil, fmt.Errorf("%w: arcade has no bytes for %s", ErrBadPayment, txid)
	}
	if len(rec.MerklePath) == 0 {
		return nil, fmt.Errorf("%w: %s is not mined yet; a transaction fetched by id carries no "+
			"ancestry, so nothing but a merkle proof can prove it is real. Try again once it is mined",
			ErrBadPayment, txid)
	}

	tx, err := transaction.NewTransactionFromBytes(rec.RawTx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadPayment, err)
	}
	mp, err := transaction.NewMerklePathFromBinary(rec.MerklePath)
	if err != nil {
		return nil, fmt.Errorf("%w: parse BUMP: %v", ErrBadPayment, err)
	}
	if err := tx.AddMerkleProof(mp); err != nil {
		return nil, fmt.Errorf("%w: attach merkle proof: %v", ErrBadPayment, err)
	}

	target, err := FundingAddress(c.Identity, c.Config.Network)
	if err != nil {
		return nil, err
	}
	indices, satoshis := MatchFundingOutputs(tx, target)
	if len(indices) == 0 {
		return nil, ErrNoPaymentOutput
	}
	if satoshis < c.Config.MinPaymentSatoshis {
		return nil, fmt.Errorf("%w: %d satoshis, minimum is %d",
			ErrPaymentTooSmall, satoshis, c.Config.MinPaymentSatoshis)
	}

	atomic, err := tx.AtomicBEEF(false)
	if err != nil {
		return nil, fmt.Errorf("%w: build atomic BEEF: %v", ErrBadPayment, err)
	}
	return c.internalizeOutputs(ctx, atomic, tx, indices, satoshis, "public funding via "+txid[:16])
}
