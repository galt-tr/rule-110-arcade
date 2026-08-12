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

	// provider is the wallet's storage, kept rather than discarded because it is
	// the only supported way to get a transaction's bytes back from a txid. See
	// Chain.RawTx: every cell tip is now a txid plus a position, and the bytes
	// behind that txid have to come from somewhere that cannot go stale.
	provider *storage.Provider

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
		// Every transition, not just terminal ones: the diagram is showing the
		// lifecycle, so the intermediate states are the point.
		//
		// This is roughly a 4x multiplier on event volume, and arcade's SSE
		// fan-out is measured at ~1,500-1,700 events/s, so above about three
		// generations a second it is the wrong trade and should be turned off.
		FullStatusUpdates: cfg.FullStatusUpdates,
	})

	hdrs, err := headers.New(logger, defs.ChainTracks{
		Enabled: true,
		URL:     cfg.chainTracksURL(),
	},
		// The ~1000 proofs that share a block then cost one header fetch between
		// them rather than one each — the lever that keeps the status-apply
		// pipeline ahead of mining at a sustained high rate.
		headers.WithCacheDepth(0),
	)
	if err != nil {
		return nil, fmt.Errorf("chain: headers client: %w", err)
	}

	// The covenant reconstructs exactly two outputs — its continuation and one
	// P2PKH change — so every cell transition must have exactly one change
	// output. The default change basket splits change up to 8 ways for privacy,
	// which produces a 9-output transaction the covenant cannot satisfy.
	//
	// This costs parallelism: the change pool grows one coin per transaction
	// rather than eight, so many concurrent cells contend for coins.
	changeBasket := defs.DefaultChangeBasket()
	changeBasket.MaxChangeOutputsPerTx = 1

	um, err := cfg.utxoManagement()
	if err != nil {
		return nil, err
	}

	extra := []storage.Option{
		storage.WithChangeBasket(changeBasket),

		// Priced above arcade's floor for headroom, not because the floor is
		// applied to a larger size than we price — it is not. See
		// Config.FeeSatPerKB.
		storage.WithFeeModel(defs.FeeModel{Type: defs.SatPerKB, Value: cfg.FeeSatPerKB}),

		// The fee is committed from a size ESTIMATE made before the unlocking
		// scripts exist, and ours are ~2.6 kB of covenant, so an estimate that
		// comes out short is the one remaining way to emit an underpriced
		// transaction. This measures the finished article instead of the plan,
		// before anything is persisted or sent.
		storage.WithMinBroadcastFeeRate(cfg.MinBroadcastFeeRate),

		// Coin selection is the difference between 128 cells running in parallel
		// and 128 cells queueing. Under the privacy strategy they contend for the
		// unreserved change set: we measured the whole automaton down to 4 tx/s
		// with 981 coins in the wallet and exactly ONE of them claimable. The
		// denominated pool makes every coin interchangeable, so ClaimExact's
		// SKIP LOCKED claim never collides.
		storage.WithUTXOManagement(um),

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
		provider: provider,
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
	mon, err := monitor.NewDaemon(logger, provider, hdrs, oracle, monCfg,
		monitor.WithoutDistributedLock(),
		// The default of 8 appliers is documented as too low for a sustained
		// high rate, and the failure is not a slowdown: when the appliers cannot
		// drain the hand-off queue the SSE reader blocks, arcade drops events
		// for a slow client, and those transactions end up with no status at
		// all. Statuses are what release the depth gate here, so losing them
		// would wedge every cell.
		monitor.WithApplyConcurrency(cfg.ApplyConcurrency),
	)
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

// Funds is what the deployment can actually spend, and what it has left to
// convert into spendable coin.
//
// The distinction is the whole point. Wallet.Balance reports the CHANGE basket,
// but under the throughput strategy the funder claims from the FUEL basket, so
// the headline balance measures a pot the automaton never spends from. We
// watched that happen: 2,955,934,265 satoshis reported, zero claimable fuel
// coins, and an automaton that could not build a single transaction. A number
// that healthy while nothing works is worse than no number at all.
type Funds struct {
	// Spendable is the value the funder can claim right now.
	Spendable uint64
	// Reserve is what is left to mint more fuel from once Spendable runs down.
	Reserve uint64
	// PoolCoins is how many claimable coins back Spendable. This is the figure
	// that predicts starvation: one coin funds one transition, so the pool
	// draining is visible long before the satoshis are gone.
	//
	// Indicative rather than authoritative — it is counted from the metastore
	// while the funder claims from the utxostore, so the two can briefly
	// disagree under load.
	PoolCoins int
}

// Funds reports spendable and reserve balances for whichever funding strategy
// is configured.
func (c *Chain) Funds(ctx context.Context) (Funds, error) {
	pool, _, _, throughput := c.Config.FuelPool()
	if !throughput {
		// Privacy strategy: the change basket IS the spendable pot.
		spendable, err := c.Wallet.Balance(ctx)
		if err != nil {
			return Funds{}, err
		}
		coins, err := c.Wallet.BasketClaimableCount(ctx, string(wdk.BasketNameForChange))
		if err != nil {
			return Funds{}, fmt.Errorf("chain: count claimable coins: %w", err)
		}
		return Funds{Spendable: spendable, PoolCoins: coins}, nil
	}

	spendable, err := c.Wallet.BasketBalance(ctx, pool)
	if err != nil {
		return Funds{}, fmt.Errorf("chain: fuel pool balance: %w", err)
	}
	coins, err := c.Wallet.BasketClaimableCount(ctx, pool)
	if err != nil {
		return Funds{}, fmt.Errorf("chain: count claimable fuel: %w", err)
	}
	// Everything the keeper can still turn into fuel: unspent change plus
	// whatever is already staged in the reserve basket.
	change, err := c.Wallet.Balance(ctx)
	if err != nil {
		return Funds{}, err
	}
	um, err := c.Config.utxoManagement()
	if err != nil {
		return Funds{}, err
	}
	reserve, err := c.Wallet.BasketBalance(ctx, um.Throughput.ReserveBasket)
	if err != nil {
		return Funds{}, fmt.Errorf("chain: reserve balance: %w", err)
	}
	return Funds{Spendable: spendable, Reserve: change + reserve, PoolCoins: coins}, nil
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
