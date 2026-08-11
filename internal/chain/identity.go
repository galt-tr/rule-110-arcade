package chain

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/brc29"
)

// keyFile is the on-disk name of the wallet's key material.
const keyFile = "keys.json"

// Identity is the durable key material for a deployment.
//
// An arcade-only wallet has no restore-from-seed: this file plus the wallet
// database ARE the wallet. Losing either loses the coins.
type Identity struct {
	// WalletKeyHex is the wallet's root private key.
	WalletKeyHex string `json:"walletKey"`

	// FunderKeyHex is an ephemeral counterparty key used only to derive a
	// payment address we can hand out.
	//
	// BRC-29 derivation is ECDH between a sender and a recipient, so a payment
	// address normally presumes the payer runs BRC-29 too. To accept a plain
	// transaction from an arbitrary payer we play BOTH roles: we generate this
	// "sender" key ourselves, derive the address from it, and later internalize
	// with it as the SenderIdentityKey. The payer just sends to an address.
	FunderKeyHex string `json:"funderKey"`

	// DerivationPrefix and DerivationSuffix pin the BRC-29 key derivation for
	// the funding address. Stored base64, matching brc29.KeyID.
	DerivationPrefix string `json:"derivationPrefix"`
	DerivationSuffix string `json:"derivationSuffix"`
}

// LoadOrCreateIdentity reads the key file from dir, creating it on first use.
//
// The file is written 0600 and the directory 0700: it is private key material.
func LoadOrCreateIdentity(dir string) (*Identity, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("chain: create data dir %s: %w", dir, err)
	}
	path := filepath.Join(dir, keyFile)

	switch data, err := os.ReadFile(path); {
	case err == nil:
		var id Identity
		if err := json.Unmarshal(data, &id); err != nil {
			return nil, fmt.Errorf("chain: parse %s: %w", path, err)
		}
		if id.WalletKeyHex == "" || id.FunderKeyHex == "" {
			return nil, fmt.Errorf("chain: %s is missing key material", path)
		}
		return &id, nil
	case !os.IsNotExist(err):
		return nil, fmt.Errorf("chain: read %s: %w", path, err)
	}

	walletKey, err := ec.NewPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("chain: generate wallet key: %w", err)
	}
	funderKey, err := ec.NewPrivateKey()
	if err != nil {
		return nil, fmt.Errorf("chain: generate funder key: %w", err)
	}
	prefix, err := randomB64(16)
	if err != nil {
		return nil, err
	}
	suffix, err := randomB64(16)
	if err != nil {
		return nil, err
	}

	id := &Identity{
		WalletKeyHex:     hex.EncodeToString(walletKey.Serialize()),
		FunderKeyHex:     hex.EncodeToString(funderKey.Serialize()),
		DerivationPrefix: prefix,
		DerivationSuffix: suffix,
	}

	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("chain: encode identity: %w", err)
	}
	// Write-then-rename so a crash cannot leave a half-written key file.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return nil, fmt.Errorf("chain: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("chain: install %s: %w", path, err)
	}
	return id, nil
}

// KeyID returns the BRC-29 derivation identifier for the funding address.
func (i *Identity) KeyID() brc29.KeyID {
	return brc29.KeyID{
		DerivationPrefix: i.DerivationPrefix,
		DerivationSuffix: i.DerivationSuffix,
	}
}

// DerivationBytes returns the raw prefix and suffix, as InternalizeAction's
// payment remittance wants them (the KeyID carries the base64 form).
func (i *Identity) DerivationBytes() (prefix, suffix []byte, err error) {
	prefix, err = base64.StdEncoding.DecodeString(i.DerivationPrefix)
	if err != nil {
		return nil, nil, fmt.Errorf("chain: decode derivation prefix: %w", err)
	}
	suffix, err = base64.StdEncoding.DecodeString(i.DerivationSuffix)
	if err != nil {
		return nil, nil, fmt.Errorf("chain: decode derivation suffix: %w", err)
	}
	return prefix, suffix, nil
}

// FunderKey returns the ephemeral counterparty private key.
func (i *Identity) FunderKey() (*ec.PrivateKey, error) {
	k, err := ec.PrivateKeyFromHex(i.FunderKeyHex)
	if err != nil {
		return nil, fmt.Errorf("chain: parse funder key: %w", err)
	}
	return k, nil
}

// WalletPublicKeyHex returns the wallet's identity public key.
func (i *Identity) WalletPublicKeyHex() (string, error) {
	k, err := ec.PrivateKeyFromHex(i.WalletKeyHex)
	if err != nil {
		return "", fmt.Errorf("chain: parse wallet key: %w", err)
	}
	return hex.EncodeToString(k.PubKey().Compressed()), nil
}

func randomB64(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("chain: read randomness: %w", err)
	}
	return base64.StdEncoding.EncodeToString(b), nil
}
