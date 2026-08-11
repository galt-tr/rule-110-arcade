package chain

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	sdk "github.com/bsv-blockchain/go-sdk/wallet"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/arcade"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/defs"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/headers"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/monitor"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/services"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/storage"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/storage/perfprovider"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/wallet"
	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/wdk"
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

	// The callback token is what ties our broadcasts to our event stream.
	// arcade only sends X-CallbackToken when this is non-empty, and only scopes
	// GET /events with ?callbackToken= under the same condition — so leaving it
	// blank means broadcasting untagged and subscribing unscoped, and status
	// updates never come back. It must be stable across restarts, so it is
	// derived from the wallet key rather than generated.
	callbackToken, err := id.CallbackToken()
	if err != nil {
		return nil, err
	}

	oracle := arcade.New(logger, nil, defs.Arcade{
		Enabled:       true,
		URL:           cfg.ArcadeURL,
		EventsURL:     cfg.ArcadeURL,
		CallbackToken: callbackToken,
		// Ask for every transition, not just terminal ones: the diagram is
		// showing the lifecycle, so the intermediate states are the point.
		FullStatusUpdates: true,
	})

	hdrs, err := headers.New(logger, defs.ChainTracks{
		Enabled: true,
		URL:     cfg.chainTracksURL(),
	})
	if err != nil {
		return nil, fmt.Errorf("chain: headers client: %w", err)
	}

	// The covenant reconstructs exactly two outputs — its continuation and one
	// P2PKH change — so every cell transition must have exactly one change
	// output. The default change basket splits change up to 8 ways for privacy,
	// which produces a 9-output transaction the covenant cannot satisfy.
	//
	// This costs parallelism: the change pool grows one coin per transaction
	// rather than eight, so many concurrent cells will contend for coins. The
	// throughput strategy's denominated fuel pool is the real answer at scale.
	changeBasket := defs.DefaultChangeBasket()
	changeBasket.MaxChangeOutputsPerTx = 1

	extra := []storage.Option{
		storage.WithChangeBasket(changeBasket),

		// One change output is not enough — there must also always BE one. The
		// funder otherwise drops it whenever the leftover falls under the dust
		// floor (40 satoshis at 100 sat/kB) and donates it to the miner, which
		// leaves a one-output transaction the covenant cannot spend. That is not
		// rare: every transition returns its change as a smaller coin, so the
		// pool grinds down and the totals land in the sub-dust window more and
		// more often as the automaton runs.
		storage.WithRequiredChangeOutput(),

		// Bound BEEF assembly to the directly-spent coin instead of walking the
		// whole ancestry. Each cell is an unbroken self-spending chain that
		// grows by one transaction per generation, so without this every step
		// costs more than the last and the automaton visibly slows as it runs.
		storage.WithDirectInputBEEF(),

		// The delayed-broadcast drainer is single-threaded by default, which
		// serialises a generation that is otherwise fully parallel.
		storage.WithSendConcurrency(64),
	}
	if cfg.Chronicle {
		// Rúnar covenants contain OP_2MUL, which Genesis-era rules reject. See
		// the cellscript package docs.
		extra = append(extra, storage.WithChronicleOpcodes())
	}

	pcfg := perfprovider.Config{
		Backend:      perfprovider.BackendSQLite,
		SQLitePath:   filepath.Join(cfg.DataDir, "wallet.db"),
		Network:      cfg.Network,
		StorageName:  storageName,
		MaxDBConns:   cfg.MaxDBConns,
		ExtraOptions: extra,
	}
	if cfg.PostgresDSN != "" {
		pcfg.Backend = perfprovider.BackendPostgres
		pcfg.PostgresDSN = cfg.PostgresDSN
		pcfg.SQLitePath = ""
	}

	provider, closeProvider, err := perfprovider.New(ctx, logger, pcfg, oracle, hdrs)
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

	// Start the monitor's SSE apply pipeline.
	//
	// This is NOT optional. Change is minted at TierSending and is promoted to
	// TierUnproven (the first claimable tier) only when the SEEN status is
	// APPLIED — and ApplyStatusBatch is driven exclusively by this daemon.
	// Without it, statuses never land, every change coin stays unspendable, and
	// the funder reports "not enough funds" while the balance looks healthy.
	//
	// The cron tasks matter as much as the stream: the SSE feed only carries
	// events from the moment it connects, so a status that fired before startup
	// is never replayed. CheckForProofs is the poll fallback that recovers it.
	monCfg := defs.DefaultMonitorConfig()
	mon, err := monitor.NewDaemon(logger, provider, hdrs, oracle,
		monCfg, monitor.WithoutDistributedLock())
	if err != nil {
		c.close(ctx)
		return nil, fmt.Errorf("chain: monitor: %w", err)
	}
	if err := mon.Start(ctx, monCfg.Tasks.EnabledTasks()); err != nil {
		c.close(ctx)
		return nil, fmt.Errorf("chain: start monitor: %w", err)
	}
	c.closers = append([]func(context.Context) error{
		func(context.Context) error { return mon.Stop() },
	}, c.closers...)

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

// WaitForClaimableFunds blocks until the change basket holds at least one
// claimable coin, or the deadline passes.
//
// This is needed after any wallet restart: coins become claimable only once the
// monitor has applied their status, and the SSE stream does not replay history,
// so a freshly-started process must wait for the poll fallback to catch up.
func (c *Chain) WaitForClaimableFunds(ctx context.Context, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		n, err := c.Wallet.BasketClaimableCount(ctx, string(wdk.BasketNameForChange))
		if err != nil {
			return fmt.Errorf("chain: count claimable coins: %w", err)
		}
		if n > 0 {
			return nil
		}
		if time.Now().After(deadline) {
			balance, _ := c.Balance(ctx)
			return fmt.Errorf(
				"chain: no claimable coins after %s (balance %d sat)\n"+
					"coins become claimable only once the monitor applies their status; "+
					"if the funding transaction is still unmined, mine a block",
				timeout, balance)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
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
