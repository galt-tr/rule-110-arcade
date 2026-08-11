package chain

import (
	"context"
	"encoding/hex"
	"fmt"
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

// Internalize imports an externally-funded payment into the wallet.
//
// rawTxHex may be a plain raw transaction or Extended Format; bumpHex is the
// BUMP (merkle path) proving it was mined. Both are required: InternalizeAction
// calls VerifyBeef with allowTxidOnly=false, so an unproven transaction is
// rejected outright — there is no path for importing an unconfirmed payment.
//
// outputIndex selects which output pays this wallet. It is matched against the
// derived BRC-29 locking script before anything is submitted, so a wrong index
// fails locally with a clear message instead of an opaque wallet error.
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
	got := tx.Outputs[outputIndex].LockingScript.String()
	if got != target.LockingScriptHex {
		return fmt.Errorf("chain: output %d does not pay this wallet\n  expected: %s\n  got:      %s",
			outputIndex, target.LockingScriptHex, got)
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

	prefix, suffix, err := c.Identity.DerivationBytes()
	if err != nil {
		return err
	}
	funder, err := c.Identity.FunderKey()
	if err != nil {
		return err
	}

	res, err := c.Wallet.InternalizeAction(ctx, sdk.InternalizeActionArgs{
		Tx: beefBytes,
		Outputs: []sdk.InternalizeOutput{{
			OutputIndex: outputIndex,
			Protocol:    sdk.InternalizeProtocolWalletPayment,
			PaymentRemittance: &sdk.Payment{
				DerivationPrefix:  prefix,
				DerivationSuffix:  suffix,
				SenderIdentityKey: funder.PubKey(),
			},
		}},
		Description: description,
		Labels:      []string{"funding"},
	}, c.Config.Originator)
	if err != nil {
		return fmt.Errorf("chain: internalize: %w", err)
	}
	if !res.Accepted {
		return fmt.Errorf("chain: wallet did not accept the funding payment")
	}
	return nil
}
