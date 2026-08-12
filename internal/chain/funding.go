package chain

import (
	"context"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdk "github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/brc29"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/defs"
)

// FundingTarget is where to send coins to fund a deployment, together with the
// material needed to internalize the payment afterwards.
type FundingTarget struct {
	// Address is an ordinary P2PKH address. A payer needs nothing else — they
	// do not have to speak BRC-29.
	Address string

	// LockingScriptHex is the script that address encodes, for a payer who
	// would rather construct the output directly.
	LockingScriptHex string

	// SenderPublicKeyHex is the counterparty key the payment is derived
	// against. It is OUR ephemeral key, not the payer's.
	SenderPublicKeyHex string

	// DerivationPrefix and DerivationSuffix are the base64 BRC-29 key
	// identifiers pinning this address.
	DerivationPrefix string
	DerivationSuffix string
}

// FundingAddress derives the address that funds a deployment.
//
// This is deliberately offline: it needs no arcade connection, so an address
// can be handed out before any service is reachable.
//
// The derivation plays both BRC-29 roles locally (see Identity.FunderKeyHex),
// which is what lets an arbitrary payer fund the wallet with a plain payment
// while the wallet still holds the derivation material required to spend it.
func FundingAddress(id *Identity, network defs.BSVNetwork) (*FundingTarget, error) {
	walletPub, err := id.WalletPublicKeyHex()
	if err != nil {
		return nil, err
	}
	funderKey, err := id.FunderKey()
	if err != nil {
		return nil, err
	}

	// brc29's option type is unexported, so the network choice has to branch at
	// the call site rather than build a slice. Only mainnet uses the mainnet
	// address version byte; testnet, teratestnet and regtest share the other.
	var addr *script.Address
	if network == defs.NetworkMainnet {
		addr, err = brc29.AddressForCounterparty(
			brc29.PrivHex(id.FunderKeyHex), id.KeyID(), brc29.PubHex(walletPub), brc29.WithMainNet(),
		)
	} else {
		addr, err = brc29.AddressForCounterparty(
			brc29.PrivHex(id.FunderKeyHex), id.KeyID(), brc29.PubHex(walletPub), brc29.WithTestNet(),
		)
	}
	if err != nil {
		return nil, fmt.Errorf("chain: derive funding address: %w", err)
	}

	lockingScript, err := brc29.LockForCounterparty(
		brc29.PrivHex(id.FunderKeyHex), id.KeyID(), brc29.PubHex(walletPub),
	)
	if err != nil {
		return nil, fmt.Errorf("chain: derive funding locking script: %w", err)
	}

	return &FundingTarget{
		Address:            addr.AddressString,
		LockingScriptHex:   lockingScript.String(),
		SenderPublicKeyHex: hex.EncodeToString(funderKey.PubKey().Compressed()),
		DerivationPrefix:   id.DerivationPrefix,
		DerivationSuffix:   id.DerivationSuffix,
	}, nil
}

// FundingTarget derives this deployment's funding target.
//
// A method as well as the package function because the two callers that need it
// most — the HTTP funding endpoint and the cold start — hold a Chain and should
// not have to know that the derivation takes an identity and a network, nor be
// able to pass the wrong pair.
func (c *Chain) FundingTarget() (*FundingTarget, error) {
	return FundingAddress(c.Identity, c.Config.Network)
}

// InternalizeResult reports what a payment credited.
type InternalizeResult struct {
	TxID string
	// Outputs are every index that paid this deployment. A transaction may pay
	// the funding script more than once; all of them are credited.
	Outputs  []uint32
	Satoshis uint64
	// Mined reports that the BEEF carried a merkle path for this transaction,
	// so the coin was credited at the mined tier rather than the unproven one.
	Mined bool
}

// MatchFundingOutputs returns every output index whose locking script is the
// one this deployment's funding address encodes, and their total value.
//
// It SCANS rather than trusting an index, because a BRC-100 wallet's
// createAction randomises output order by default: the payer's wallet decides
// where the payment lands and does not tell the payer. The derived script is
// the only thing that identifies our output, and it is the one thing we can
// derive ourselves — so a caller-supplied index is at best redundant and at
// worst a way to ask us to credit an output that pays someone else.
func MatchFundingOutputs(tx *transaction.Transaction, target *FundingTarget) (indices []uint32, satoshis uint64) {
	for i, out := range tx.Outputs {
		if out.LockingScript == nil || out.LockingScript.String() != target.LockingScriptHex {
			continue
		}
		indices = append(indices, uint32(i))
		satoshis += out.Satoshis
	}
	return indices, satoshis
}

// internalizeOutputs is the ONE call site of InternalizeAction.
//
// Both funding paths — the CLI's raw transaction plus a BUMP, and a browser
// wallet's BEEF — reduce to atomic BEEF bytes plus a set of output indices, so
// the remittance material lives here once. That material is the part which is
// easy to get subtly wrong and impossible to diagnose afterwards: the wrong
// derivation prefix or sender key produces a credited output the wallet can
// never derive a key for, which reads as a healthy balance and an unspendable
// coin.
func (c *Chain) internalizeOutputs(
	ctx context.Context, atomicBEEF []byte, tx *transaction.Transaction,
	indices []uint32, satoshis uint64, description string,
) (*InternalizeResult, error) {
	if len(indices) == 0 {
		return nil, ErrNoPaymentOutput
	}

	prefix, suffix, err := c.Identity.DerivationBytes()
	if err != nil {
		return nil, err
	}
	funder, err := c.Identity.FunderKey()
	if err != nil {
		return nil, err
	}

	outputs := make([]sdk.InternalizeOutput, 0, len(indices))
	for _, vout := range indices {
		outputs = append(outputs, sdk.InternalizeOutput{
			OutputIndex: vout,
			Protocol:    sdk.InternalizeProtocolWalletPayment,
			PaymentRemittance: &sdk.Payment{
				DerivationPrefix:  prefix,
				DerivationSuffix:  suffix,
				SenderIdentityKey: funder.PubKey(),
			},
		})
	}

	res, err := c.Wallet.InternalizeAction(ctx, sdk.InternalizeActionArgs{
		Tx:          atomicBEEF,
		Outputs:     outputs,
		Description: description,
		Labels:      []string{"funding"},
	}, c.Config.Originator)
	if err != nil {
		return nil, fmt.Errorf("chain: internalize: %w", err)
	}
	if !res.Accepted {
		return nil, fmt.Errorf("chain: wallet did not accept the funding payment")
	}
	return &InternalizeResult{
		TxID:     tx.TxID().String(),
		Outputs:  indices,
		Satoshis: satoshis,
		Mined:    tx.MerklePath != nil,
	}, nil
}

// Internalize imports a MINED payment from a raw transaction plus its BUMP.
//
// This is the operator path — `rule110 fund` — and it stays as it was: a raw
// transaction carries no ancestry, so nothing but a merkle proof can establish
// that it is real, and both flags remain required here.
//
// That is a property of THIS path, not of InternalizeAction. An earlier comment
// on this function claimed the wallet rejects any unproven transaction outright
// and that no path could import an unconfirmed payment. It does not: storage's
// InternalizeAction checks whether the BEEF carries a merkle path for the
// subject transaction and, when it does not, credits the output at the unproven
// tier — which is spendable, and which is what makes a one-click Fund button
// possible. See AcceptPayment.
//
// outputIndex selects which output pays this wallet. It is checked against the
// scan rather than trusted, so a wrong index fails locally with a clear message.
func (c *Chain) Internalize(ctx context.Context, rawTxHex, bumpHex string, outputIndex uint32, description string) error {
	tx, err := transaction.NewTransactionFromHex(strings.TrimSpace(rawTxHex))
	if err != nil {
		return fmt.Errorf("chain: parse funding transaction: %w", err)
	}
	if int(outputIndex) >= len(tx.Outputs) {
		return fmt.Errorf("chain: output index %d out of range (%d outputs)", outputIndex, len(tx.Outputs))
	}

	target, err := FundingAddress(c.Identity, c.Config.Network)
	if err != nil {
		return err
	}
	indices, _ := MatchFundingOutputs(tx, target)
	if !slices.Contains(indices, outputIndex) {
		return fmt.Errorf("chain: output %d does not pay this wallet\n  expected: %s\n  got:      %s",
			outputIndex, target.LockingScriptHex, tx.Outputs[outputIndex].LockingScript.String())
	}

	mp, err := transaction.NewMerklePathFromHex(strings.TrimSpace(bumpHex))
	if err != nil {
		return fmt.Errorf("chain: parse BUMP: %w", err)
	}
	if err := tx.AddMerkleProof(mp); err != nil {
		return fmt.Errorf("chain: attach merkle proof: %w", err)
	}

	// InternalizeAction wants ATOMIC BEEF, not the V2 BEEF that
	// NewBeefFromTransaction produces — the latter fails validation with
	// "version 4022206466 is not atomic BEEF" (0xEFBE0002, the V2 magic).
	// allowPartial=false: the proof is present, so nothing may be a stub.
	beefBytes, err := tx.AtomicBEEF(false)
	if err != nil {
		return fmt.Errorf("chain: build atomic BEEF: %w", err)
	}

	// Credit only the output the operator named, not every match. This path is
	// someone typing a vout on a command line; doing more than they asked with
	// their transaction is not a kindness.
	sats := tx.Outputs[outputIndex].Satoshis
	_, err = c.internalizeOutputs(ctx, beefBytes, tx, []uint32{outputIndex}, sats, description)
	return err
}
