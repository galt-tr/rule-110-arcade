package chain

import (
	"strings"
	"testing"

	"github.com/bsv-blockchain/go-arcade-toolbox/pkg/wdk"
)

// The cell chain and the fuel pool must live in different baskets, and this is
// load-bearing rather than tidy.
//
// The toolbox has a known, deliberately unfixed gap: funder.FundArgs carries no
// exclusion list, so an input the caller supplies explicitly can ALSO be
// selected by the funder if it happens to be claimable in the funding basket —
// producing a transaction with the same outpoint twice
// (go-arcade-toolbox docs/rejection-hardening-audit.md:344-366, rated High).
//
// We hand every cell transition its previous output as an explicit outpoint, so
// the only thing standing between this deployment and that bug is that cell
// outputs go to CellBasket while fuel comes from the pool basket. That is an
// invisible safety property held up by two string constants, which is exactly
// the kind of thing that gets "tidied" into one.
func TestCellsAndFuelNeverShareABasket(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ArcadeURL = "http://arcade.invalid"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}

	pool, _, _, enabled := cfg.FuelPool()
	if !enabled {
		t.Fatal("the throughput fuel pool is off, so this deployment funds from change — " +
			"re-examine this test rather than deleting it, because the change basket is " +
			"then the one that must not hold cell outputs")
	}

	if pool == CellBasket {
		t.Fatalf("fuel pool and cell outputs share the basket %q. The funder may then "+
			"select a cell's own continuation output to pay that cell's fee, putting the "+
			"same outpoint in the transaction twice — see FundArgs having no exclusion "+
			"list", pool)
	}
	if strings.EqualFold(pool, CellBasket) {
		t.Errorf("fuel pool %q and cell basket %q differ only by case; whether that is "+
			"one basket or two is up to a storage backend's collation, which is not a "+
			"safety argument", pool, CellBasket)
	}

	// The default change basket is the third party here: the keeper draws its
	// reserve from it, so a cell output landing there is reachable by the funder
	// through the same path.
	if CellBasket == string(wdk.BasketNameForChange) {
		t.Errorf("cell outputs go to the wallet's change basket (%q), which the fuel "+
			"keeper draws its reserve from", CellBasket)
	}
}

// CellBasket is written into every cell output at genesis and at every
// transition, so changing it silently strands the whole ring: the outputs the
// engine derives tips from would no longer be found where it looks.
func TestCellBasketIsStable(t *testing.T) {
	if CellBasket != "rule110cells" {
		t.Errorf("CellBasket = %q, want %q. Every cell output ever minted carries the "+
			"old name; changing it does not migrate them, it hides them.",
			CellBasket, "rule110cells")
	}
}
