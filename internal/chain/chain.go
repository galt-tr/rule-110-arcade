package chain

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"

	sdk "github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/defs"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/headers"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/services"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/storage"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/storage/perfprovider"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/wallet"
)

// storageName identifies this deployment's storage to the wallet.
const storageName = "rule110-storage"

// Chain is a connected wallet: storage, arcade and headers, wired together.
type Chain struct {
	Config   Config
	Identity *Identity
	Wallet   *wallet.Wallet
	Oracle   arcade.TxOracle
	Headers  headers.Headers

	// IdentityKey is the wallet's identity public key, as the wallet reports it.
	IdentityKey string

	logger  *slog.Logger
	closers []func(context.Context) error
}

// Open builds a Chain from cfg, creating the wallet on first use.
func Open(ctx context.Context, cfg Config, logger *slog.Logger) (*Chain, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	id, err := LoadOrCreateIdentity(cfg.DataDir)
	if err != nil {
		return nil, err
	}

	oracle := arcade.New(logger, nil, defs.Arcade{
		Enabled:   true,
		URL:       cfg.ArcadeURL,
		EventsURL: cfg.ArcadeURL,
	})

	hdrs, err := headers.New(logger, defs.ChainTracks{
		Enabled: true,
		URL:     cfg.chainTracksURL(),
	})
	if err != nil {
		return nil, fmt.Errorf("chain: headers client: %w", err)
	}

	extra := []storage.Option{
		// Rúnar covenants contain OP_2MUL, which Genesis-era rules reject. See
		// the cellscript package docs.
		storage.WithChronicleOpcodes(),
	}
	if !cfg.Chronicle {
		extra = nil
	}

	provider, closeProvider, err := perfprovider.New(ctx, logger, perfprovider.Config{
		Backend:      perfprovider.BackendSQLite,
		SQLitePath:   filepath.Join(cfg.DataDir, "wallet.db"),
		Network:      cfg.Network,
		StorageName:  storageName,
		ExtraOptions: extra,
	}, oracle, hdrs)
	if err != nil {
		return nil, fmt.Errorf("chain: storage provider: %w", err)
	}

	c := &Chain{
		Config:   cfg,
		Identity: id,
		Oracle:   oracle,
		Headers:  hdrs,
		logger:   logger,
		closers:  []func(context.Context) error{closeProvider},
	}

	walletPub, err := id.WalletPublicKeyHex()
	if err != nil {
		c.close(ctx)
		return nil, err
	}
	if _, err := provider.Migrate(ctx, storageName, walletPub); err != nil {
		c.close(ctx)
		return nil, fmt.Errorf("chain: migrate storage: %w", err)
	}

	svc := services.New(logger, oracle, hdrs, defs.DefaultServicesConfig(cfg.Network))
	w, err := wallet.New(cfg.Network, id.WalletKeyHex, provider,
		wallet.WithServices(svc),
		wallet.WithLogger(logger),
		// Skips the shared BeefParty mutex, which is otherwise a hard serial
		// cap once many cells are advancing at once.
		wallet.WithThroughputMode(true),
	)
	if err != nil {
		c.close(ctx)
		return nil, fmt.Errorf("chain: wallet: %w", err)
	}
	c.Wallet = w

	// Storage binds lazily; this forces the bind and confirms the identity key.
	pub, err := w.GetPublicKey(ctx, sdk.GetPublicKeyArgs{IdentityKey: true}, cfg.Originator)
	if err != nil {
		c.close(ctx)
		return nil, fmt.Errorf("chain: resolve identity key: %w", err)
	}
	c.IdentityKey = pub.PublicKey.ToDERHex()

	return c, nil
}

// Close releases the wallet and its storage.
func (c *Chain) Close(ctx context.Context) error { return c.close(ctx) }

func (c *Chain) close(ctx context.Context) error {
	if c.Wallet != nil {
		c.Wallet.Close()
	}
	var firstErr error
	for _, fn := range c.closers {
		if err := fn(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Balance returns the wallet's spendable change balance.
func (c *Chain) Balance(ctx context.Context) (uint64, error) {
	return c.Wallet.Balance(ctx)
}

// chainTracksURL returns the configured headers endpoint, deriving it from the
// arcade base when unset. A standard arcade deployment serves ChainTracks under
// /chaintracks/v2 on the same host.
func (c Config) chainTracksURL() string {
	if c.ChainTracksURL != "" {
		return c.ChainTracksURL
	}
	return c.ArcadeURL + "/chaintracks/v2"
}
