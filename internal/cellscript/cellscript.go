// Package cellscript compiles the Cell contract to Bitcoin Script and binds it
// per cell.
//
// The contract is compiled ONCE. Each of the N cells is then a distinct binding
// of the same artifact: the six neighbourhood constants differ per cell, and it
// is those constants alone that encode the ring wrap. Cell 0's right neighbour
// is cell N-1 purely because its DivR constant says so — no branch, no extra
// opcode, no special-case script.
//
// # Chronicle is required
//
// Rúnar's injected checkPreimage preamble emits OP_2MUL, which BSV re-enabled
// only in the Chronicle upgrade. Any verifier for these scripts must run with
// interpreter.WithAfterChronicle(); WithAfterGenesis() alone rejects them with
// "attempt to execute disabled opcode OP_2MUL". See VerifyInput.
package cellscript

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/script/interpreter"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/icellan/runar/compilers/go/compiler"
	runar "github.com/icellan/runar/packages/runar-go"

	contractsrc "github.com/dymurray/rule-110-arcade/contracts"
	"github.com/dymurray/rule-110-arcade/internal/ca"
)

// opReturn separates a stateful contract's code from its serialized state.
const opReturn = "6a"

// ctorParams is the constructor signature this package binds against, in order.
// It is checked against the compiled ABI so that renaming or reordering a field
// in Cell.runar.go fails loudly here instead of silently mis-binding constants.
var ctorParams = []string{
	"row",
	"byteL", "divL",
	"byteC", "divC",
	"byteR", "divR",
	"rowBytes",
	"rule",
}

// stepMethod is the contract's single public method. Being the only one means
// the compiler emits no method selector, so unlocking scripts are just pushes.
const stepMethod = "step"

// Compiled is the compiled Cell contract, ready to bind per cell.
type Compiled struct {
	artifact *runar.RunarArtifact
	cells    int
	rule     ca.Rule

	// codeSepOffset is the byte offset of the OP_CODESEPARATOR that scopes the
	// BIP-143 scriptCode for step.
	codeSepOffset int
}

// Compile compiles the embedded Cell contract for a ring of the given size.
func Compile(cells int, rule ca.Rule) (*Compiled, error) {
	if cells <= 0 || cells%8 != 0 {
		return nil, fmt.Errorf("cellscript: cells must be a positive multiple of 8, got %d", cells)
	}

	res := compiler.CompileFromSourceStrWithResult(contractsrc.Source, contractsrc.SourceName)
	if !res.Success || res.Artifact == nil {
		return nil, fmt.Errorf("cellscript: compile %s: %s", contractsrc.SourceName, diagnostics(res))
	}

	art, err := toSDKArtifact(res.Artifact)
	if err != nil {
		return nil, err
	}
	if err := checkABI(art); err != nil {
		return nil, err
	}

	c := &Compiled{artifact: art, cells: cells, rule: rule}

	// Resolve the code-separator offset against a real binding rather than the
	// template, then confirm it really points at OP_CODESEPARATOR.
	probe, err := c.LockingScript(0, mustRow(cells))
	if err != nil {
		return nil, err
	}
	off, err := codeSepOffset(art, probe)
	if err != nil {
		return nil, err
	}
	c.codeSepOffset = off

	return c, nil
}

// Cells returns the ring size.
func (c *Compiled) Cells() int { return c.cells }

// Rule returns the automaton rule baked into every cell.
func (c *Compiled) Rule() ca.Rule { return c.rule }

// ctorArgs builds the constructor arguments for cell i carrying row.
func (c *Compiled) ctorArgs(i int, row ca.Row) ([]interface{}, error) {
	if i < 0 || i >= c.cells {
		return nil, fmt.Errorf("cellscript: cell index %d out of range [0,%d)", i, c.cells)
	}
	if row.Cells() != c.cells {
		return nil, fmt.Errorf("cellscript: row has %d cells, contract expects %d", row.Cells(), c.cells)
	}

	l := ca.LeftIndex(c.cells, i)
	r := ca.RightIndex(c.cells, i)

	// byte index and divisor that shift the wanted bit down to position 0.
	return []interface{}{
		row.Hex(),
		int64(l / 8), int64(1) << (l % 8),
		int64(i / 8), int64(1) << (i % 8),
		int64(r / 8), int64(1) << (r % 8),
		int64(c.cells / 8),
		int64(c.rule),
	}, nil
}

// LockingScript returns the locking script for cell i carrying row.
func (c *Compiled) LockingScript(i int, row ca.Row) ([]byte, error) {
	args, err := c.ctorArgs(i, row)
	if err != nil {
		return nil, err
	}
	h := runar.NewRunarContract(c.artifact, args).GetLockingScript()
	b, err := hex.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("cellscript: decode locking script for cell %d: %w", i, err)
	}
	return b, nil
}

// CodePart returns the code portion of cell i's locking script: everything
// before the OP_RETURN that introduces the serialized state. It is constant
// across generations for a given cell, and is pushed as the contract's
// _codePart argument so the covenant can rebuild its own continuation output.
func (c *Compiled) CodePart(i int, row ca.Row) ([]byte, error) {
	lockHex, err := c.lockingScriptHex(i, row)
	if err != nil {
		return nil, err
	}
	stateHex := runar.SerializeState(c.artifact.StateFields, map[string]interface{}{"row": row.Hex()})
	suffix := opReturn + stateHex
	if !strings.HasSuffix(lockHex, suffix) {
		return nil, fmt.Errorf("cellscript: cell %d locking script does not end with OP_RETURN|state", i)
	}
	b, err := hex.DecodeString(lockHex[:len(lockHex)-len(suffix)])
	if err != nil {
		return nil, fmt.Errorf("cellscript: decode code part for cell %d: %w", i, err)
	}
	return b, nil
}

func (c *Compiled) lockingScriptHex(i int, row ca.Row) (string, error) {
	args, err := c.ctorArgs(i, row)
	if err != nil {
		return "", err
	}
	return runar.NewRunarContract(c.artifact, args).GetLockingScript(), nil
}

// Spend describes one cell transition to be unlocked.
type Spend struct {
	// CellIndex is the cell whose UTXO is being spent.
	CellIndex int
	// CurrentRow is the row the UTXO currently carries.
	CurrentRow ca.Row
	// NextRow is the claimed next generation. The script verifies only this
	// cell's bit of it.
	NextRow ca.Row

	// Tx is the fully-built spending transaction. Its inputs and outputs must
	// be final: the covenant binds all of them through the sighash preimage.
	Tx *transaction.Transaction
	// InputIndex is the input spending this cell's UTXO.
	InputIndex int
	// PrevSatoshis is the value of the UTXO being spent.
	PrevSatoshis uint64

	// ChangePKH and ChangeAmount describe the P2PKH change output the covenant
	// expects to find alongside its continuation output.
	ChangePKH    []byte
	ChangeAmount uint64
	// NewAmount is the satoshi value of the continuation output.
	NewAmount uint64
}

// UnlockingScript builds the unlocking script for one cell transition.
//
// Layout, matching RunarContract's stateful call path:
//
//	_codePart | nextRow | _changePKH | _changeAmount | _newAmount | txPreimage
//
// There is no method selector (step is the only public method) and no
// OP_PUSH_TX signature push: the compiled script derives the k=1 signature from
// the preimage on chain and OP_CHECKSIGs it against G.
func (c *Compiled) UnlockingScript(s Spend) ([]byte, error) {
	if s.Tx == nil {
		return nil, fmt.Errorf("cellscript: spend for cell %d has no transaction", s.CellIndex)
	}
	if s.NextRow.Cells() != c.cells {
		return nil, fmt.Errorf("cellscript: next row has %d cells, contract expects %d",
			s.NextRow.Cells(), c.cells)
	}
	if len(s.ChangePKH) != 20 {
		return nil, fmt.Errorf("cellscript: change pubkey hash must be 20 bytes, got %d", len(s.ChangePKH))
	}

	lockHex, err := c.lockingScriptHex(s.CellIndex, s.CurrentRow)
	if err != nil {
		return nil, err
	}
	codePart, err := c.CodePart(s.CellIndex, s.CurrentRow)
	if err != nil {
		return nil, err
	}

	// ComputeOpPushTx returns (signature, preimage) — in that order.
	_, preimage, err := runar.ComputeOpPushTxWithCodeSep(
		s.Tx.Hex(), s.InputIndex, lockHex, int64(s.PrevSatoshis), c.codeSepOffset,
	)
	if err != nil {
		return nil, fmt.Errorf("cellscript: compute preimage for cell %d: %w", s.CellIndex, err)
	}

	unlockHex := runar.EncodePushData(hex.EncodeToString(codePart)) +
		runar.EncodePushData(s.NextRow.Hex()) +
		runar.EncodePushData(hex.EncodeToString(s.ChangePKH)) +
		runar.EncodeScriptInt(int64(s.ChangeAmount)) +
		runar.EncodeScriptInt(int64(s.NewAmount)) +
		runar.EncodePushData(hex.EncodeToString(preimage))

	b, err := hex.DecodeString(unlockHex)
	if err != nil {
		return nil, fmt.Errorf("cellscript: decode unlocking script for cell %d: %w", s.CellIndex, err)
	}
	return b, nil
}

// VerifyInput runs one input's script pair through the go-sdk interpreter with
// full transaction context and Chronicle enabled.
//
// This is the same check the toolbox performs before broadcasting, except that
// the toolbox's default verifier does NOT enable Chronicle and so rejects these
// scripts on OP_2MUL. Wire this in via storage.WithScriptsVerifier.
func VerifyInput(tx *transaction.Transaction, inputIndex int) error {
	if tx == nil {
		return fmt.Errorf("cellscript: nil transaction")
	}
	if inputIndex < 0 || inputIndex >= len(tx.Inputs) {
		return fmt.Errorf("cellscript: input index %d out of range (%d inputs)", inputIndex, len(tx.Inputs))
	}
	src := tx.Inputs[inputIndex].SourceTxOutput()
	if src == nil {
		return fmt.Errorf("cellscript: input %d has no source output to verify against", inputIndex)
	}
	return interpreter.NewEngine().Execute(
		interpreter.WithTx(tx, inputIndex, src),
		interpreter.WithForkID(),
		interpreter.WithAfterChronicle(),
	)
}

// --- helpers ---------------------------------------------------------------

func toSDKArtifact(a *compiler.Artifact) (*runar.RunarArtifact, error) {
	raw, err := compiler.ArtifactToJSON(a)
	if err != nil {
		return nil, fmt.Errorf("cellscript: encode artifact: %w", err)
	}
	var out runar.RunarArtifact
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("cellscript: decode artifact: %w", err)
	}
	return &out, nil
}

// checkABI asserts the compiled contract still has the shape this package
// binds against: one public method named step, and the expected constructor
// parameters in the expected order.
func checkABI(art *runar.RunarArtifact) error {
	got := make([]string, 0, len(art.ABI.Constructor.Params))
	for _, p := range art.ABI.Constructor.Params {
		got = append(got, p.Name)
	}
	if strings.Join(got, ",") != strings.Join(ctorParams, ",") {
		return fmt.Errorf("cellscript: constructor changed\n  want: %v\n  got:  %v", ctorParams, got)
	}

	var public []string
	for _, m := range art.ABI.Methods {
		if m.IsPublic {
			public = append(public, m.Name)
		}
	}
	if len(public) != 1 || public[0] != stepMethod {
		return fmt.Errorf("cellscript: expected exactly one public method %q, got %v", stepMethod, public)
	}
	return nil
}

func codeSepOffset(art *runar.RunarArtifact, lockingScript []byte) (int, error) {
	idx := -1
	switch {
	case len(art.CodeSeparatorIndices) > 0:
		idx = art.CodeSeparatorIndices[0]
	case art.CodeSeparatorIndex != nil:
		idx = *art.CodeSeparatorIndex
	}
	if idx < 0 || idx >= len(lockingScript) {
		return 0, fmt.Errorf("cellscript: code separator offset %d outside script of %d bytes",
			idx, len(lockingScript))
	}
	if lockingScript[idx] != script.OpCODESEPARATOR {
		return 0, fmt.Errorf("cellscript: byte %d is %#x, not OP_CODESEPARATOR", idx, lockingScript[idx])
	}
	return idx, nil
}

func diagnostics(res *compiler.CompileResult) string {
	var msgs []string
	for _, d := range res.Diagnostics {
		msgs = append(msgs, d.FormatMessage())
	}
	if len(msgs) == 0 {
		return "no diagnostics reported"
	}
	return strings.Join(msgs, "; ")
}

func mustRow(cells int) ca.Row {
	row, err := ca.NewRow(cells)
	if err != nil {
		panic(err)
	}
	return row
}
