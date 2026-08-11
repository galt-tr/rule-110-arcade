package chain

import (
	"encoding/hex"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script"

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
