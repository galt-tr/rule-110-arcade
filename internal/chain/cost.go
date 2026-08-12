package chain

import (
	"fmt"

	"github.com/dymurray/rule-110-arcade/internal/ca"
	"github.com/dymurray/rule-110-arcade/internal/cellscript"
)

// genesisEnvelopeBytes is everything in the genesis transaction that is not a
// cell output: 4 version + 4 locktime + 1 input count + 148 for one P2PKH input
// + 3 output count (a varint wide enough for a ring of any size we will build)
// + 34 for the change output WithRequiredChangeOutput insists on.
const genesisEnvelopeBytes = 4 + 4 + 1 + 148 + 3 + 34

// GenesisBytes is the serialized size of the genesis transaction, measured from
// the REAL compiled locking scripts.
//
// Measured rather than estimated, because a cell's locking script is a Rúnar
// covenant of well over a kilobyte and nothing about that number is guessable
// from the outside — the obvious per-output guess is a 34-byte P2PKH output,
// which is wrong by a factor of thirty-odd. Everything downstream of this
// (BootstrapMinimum, and therefore the amount a stranger is told to send)
// inherits the error, and the failure it produces is a deployment that accepts
// a payment, mints fuel, and then cannot afford the one transaction it exists
// to make.
func GenesisBytes(compiled *cellscript.Compiled, seed ca.Row) (uint64, error) {
	n := compiled.Cells()
	if seed.Cells() != n {
		return 0, fmt.Errorf("chain: seed has %d cells, contract expects %d", seed.Cells(), n)
	}

	total := uint64(genesisEnvelopeBytes)
	for cell := range n {
		lock, err := compiled.LockingScript(cell, seed)
		if err != nil {
			return 0, err
		}
		// 8 satoshis + the script's own length prefix + the script.
		total += 8 + uint64(varIntLen(uint64(len(lock)))) + uint64(len(lock))
	}
	return total, nil
}

// varIntLen is the encoded width of a Bitcoin variable-length integer.
func varIntLen(n uint64) int {
	switch {
	case n < 0xfd:
		return 1
	case n <= 0xffff:
		return 3
	case n <= 0xffffffff:
		return 5
	default:
		return 9
	}
}

// GenesisCost prices genesis: the satoshis locked into the cell outputs, the
// fee for the transaction that creates them, and a change output left above the
// dust floor.
//
// The change term is not padding. WithRequiredChangeOutput means a transaction
// whose change would fall below the floor does not quietly donate it to the
// miner — it fails, and genesis failing after the fuel has been minted is the
// worst place in the cold start to run out.
func (c Config) GenesisCost(sizeBytes uint64) uint64 {
	return uint64(c.Cells)*c.CellSatoshis +
		feeForBytes(sizeBytes, c.FeeSatPerKB) +
		dustFloor(c.FeeSatPerKB)
}

// fanOutCost prices minting `coins` fuel coins: the coins themselves, plus one
// leaf fan-out's fee and dust floor per fanoutPerTx-sized round.
func (c Config) fanOutCost(coins uint64) uint64 {
	rounds := (coins + fanoutPerTx - 1) / fanoutPerTx
	perRound := feeForBytes(leafFanOutBytes, c.FeeSatPerKB) + dustFloor(c.FeeSatPerKB)
	return coins*c.FuelDenomination + rounds*perRound
}

// bootstrapHeadroomPercent is the margin over the measured cold-start cost.
//
// The measurement is good but not exact: the funder's input selection decides
// how many inputs the genesis transaction really carries, and every input it
// adds beyond the one assumed above is another ~148 bytes of fee. 25% covers
// that comfortably at any ring size we would build, and being over is cheap
// while being under strands the deployment mid-bootstrap.
const bootstrapHeadroomPercent = 25

// BootstrapMinimum is the smallest payment that turns a cold deployment into a
// RUNNING one, rather than a stuck one.
//
// It has to cover both halves of the cold start, because they are not
// independent: the fuel fan-out has to happen first (genesis is funded from the
// pool it fills), so a payment large enough for genesis alone buys a pool and
// then cannot afford the transaction it was minted for. Quoting a number that
// only covers one half is how a stranger's payment produces a deployment that
// looks funded and does nothing.
//
// This is a floor on STARTING, not a floor on being useful. Once generation 0
// exists the automaton runs until the pool drains and then enters ModeStarved,
// whose UI points at the same address — so any later payment of any size helps,
// and there is no cliff.
func (c Config) BootstrapMinimum(genesisSize uint64) uint64 {
	total := c.fanOutCost(c.FirstFuelCoins) + c.GenesisCost(genesisSize)
	return total * (100 + bootstrapHeadroomPercent) / 100
}
