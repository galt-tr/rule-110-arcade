package chain

import (
	"testing"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
)

// compileRing compiles a small ring and its seed. Small because Compile runs
// the whole Rúnar frontend and these tests are about arithmetic, not scale.
func compileRing(t *testing.T, cells int) (*cellscript.Compiled, ca.Row) {
	t.Helper()
	compiled, err := cellscript.Compile(cells, ca.Rule110)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	seed, err := ca.NewRow(cells)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	seed.Set(cells-1, true)
	return compiled, seed
}

// TestGenesisBytesMeasuresTheRealScripts is the guard against pricing genesis
// off a guess.
//
// A cell's locking script is a Rúnar covenant of well over a kilobyte. The
// intuitive per-output figure is a 34-byte P2PKH output, and everything
// downstream — the minimum payment a stranger is asked for — inherits whatever
// error is made here. So the assertion is that the measurement tracks the
// scripts that will actually be broadcast, not a constant.
func TestGenesisBytesMeasuresTheRealScripts(t *testing.T) {
	compiled, seed := compileRing(t, 8)

	size, err := GenesisBytes(compiled, seed)
	if err != nil {
		t.Fatalf("GenesisBytes: %v", err)
	}

	lock, err := compiled.LockingScript(0, seed)
	if err != nil {
		t.Fatal(err)
	}
	scripts := uint64(len(lock)) * 8
	if size <= scripts {
		t.Errorf("genesis measured %d bytes, but its 8 locking scripts alone are ~%d", size, scripts)
	}
	// A P2PKH-shaped guess would be about 34 bytes per output. Anything near
	// that means the covenant is not being measured at all.
	if size < 8*500 {
		t.Errorf("genesis measured %d bytes for 8 covenant outputs; that is a P2PKH-shaped "+
			"guess, not a measurement of the real scripts", size)
	}
}

// The size must scale with the ring, since that is the whole reason it cannot
// be a constant.
func TestGenesisBytesScalesWithTheRing(t *testing.T) {
	small, smallSeed := compileRing(t, 8)
	large, largeSeed := compileRing(t, 16)

	a, err := GenesisBytes(small, smallSeed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenesisBytes(large, largeSeed)
	if err != nil {
		t.Fatal(err)
	}
	if b <= a {
		t.Errorf("16 cells measured %d bytes against %d for 8; genesis is one transaction "+
			"carrying one output per cell, so it must grow with the ring", b, a)
	}
}

// A seed of the wrong width is caught here rather than producing a plausible
// number for a transaction that could never be built.
func TestGenesisBytesRefusesAMismatchedSeed(t *testing.T) {
	compiled, _ := compileRing(t, 8)
	_, wrong := compileRing(t, 16)

	if _, err := GenesisBytes(compiled, wrong); err == nil {
		t.Error("GenesisBytes priced a genesis whose seed does not fit the contract")
	}
}

// TestBootstrapMinimumBuysBothHalvesOfTheColdStart pins the property that makes
// the number useful.
//
// The two halves are not independent: the fan-out has to run first, because
// genesis is funded from the pool it fills. A minimum that covers only genesis
// therefore buys a pool and then cannot afford the one transaction it was
// minted for — a deployment that looks funded and does nothing.
func TestBootstrapMinimumBuysBothHalvesOfTheColdStart(t *testing.T) {
	compiled, seed := compileRing(t, 8)
	cfg := DefaultConfig()
	cfg.Cells = 8
	cfg.ArcadeURL = "https://arcade.example"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	size, err := GenesisBytes(compiled, seed)
	if err != nil {
		t.Fatal(err)
	}

	fuel := cfg.fanOutCost(cfg.FirstFuelCoins)
	genesis := cfg.GenesisCost(size)
	got := cfg.BootstrapMinimum(size)

	if got < fuel+genesis {
		t.Errorf("bootstrap minimum %d does not cover fuel (%d) plus genesis (%d)", got, fuel, genesis)
	}
	if got <= genesis {
		t.Errorf("bootstrap minimum %d covers genesis (%d) but buys no fuel to fund it with", got, genesis)
	}
	// Headroom, not a multiple: being over is cheap, being wildly over asks
	// strangers for money the deployment does not need.
	if got > 2*(fuel+genesis) {
		t.Errorf("bootstrap minimum %d is more than twice the measured cost %d", got, fuel+genesis)
	}
}

// Genesis locks real satoshis into every cell output and pays a fee sized by
// the covenant, so its cost must exceed the trivial cells x CellSatoshis figure
// that an eyeball estimate would land on.
func TestGenesisCostExceedsTheValueItLocksUp(t *testing.T) {
	compiled, seed := compileRing(t, 8)
	cfg := DefaultConfig()
	cfg.Cells = 8

	size, err := GenesisBytes(compiled, seed)
	if err != nil {
		t.Fatal(err)
	}
	locked := uint64(cfg.Cells) * cfg.CellSatoshis
	if got := cfg.GenesisCost(size); got <= locked {
		t.Errorf("genesis cost %d does not exceed the %d satoshis it locks into cells; "+
			"the covenant's fee is not being counted", got, locked)
	}
}
