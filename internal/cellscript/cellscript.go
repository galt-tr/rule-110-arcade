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
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

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

	// bindings maps a code part back to the cell it binds, for Decode. Built on
	// first use rather than at Compile: every subcommand compiles the contract
	// and only the auditor decodes, so the N locking scripts this costs should
	// not be on the engine's startup path.
	bindingsOnce sync.Once
	bindings     map[string]int
	bindingsErr  error
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
	suffix := c.stateSuffixHex(row)
	if !strings.HasSuffix(lockHex, suffix) {
		return nil, fmt.Errorf("cellscript: cell %d locking script does not end with OP_RETURN|state", i)
	}
	b, err := hex.DecodeString(lockHex[:len(lockHex)-len(suffix)])
	if err != nil {
		return nil, fmt.Errorf("cellscript: decode code part for cell %d: %w", i, err)
	}
	return b, nil
}

// stateSuffixHex is the tail every cell locking script ends with: the OP_RETURN
// that separates code from state, then the serialized row.
//
// The serialization is the contract compiler's, not ours, so it is asked for
// rather than reimplemented — a hand-rolled pushdata that disagreed with it by
// one byte would produce a script that no cell can spend, and the only symptom
// would be a covenant failure in an unrelated place.
func (c *Compiled) stateSuffixHex(row ca.Row) string {
	return opReturn + runar.SerializeState(c.artifact.StateFields, map[string]interface{}{"row": row.Hex()})
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

	preimage, err := c.preimage(s.Tx.Hex(), s.InputIndex, lockHex, s.PrevSatoshis)
	if err != nil {
		return nil, fmt.Errorf("cellscript: cell %d: %w", s.CellIndex, err)
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

// Preimage recomputes the BIP-143 sighash preimage that a spend of prevLock must
// carry in its unlocking script.
//
// The preimage is not a signature and nothing about it is secret: it is a
// serialization of the spending transaction, the outpoint being spent, that
// output's value, and the scriptCode. Recomputing it and comparing byte for byte
// with the one a transaction actually pushed is how a reader establishes, with
// no script engine involved, that the covenant was bound to THIS spend — that
// the row the script read really came from the output this input names, rather
// than from a preimage assembled to say something more convenient.
//
// The tx must be the finished transaction. BIP-143 commits to inputs by outpoint
// and sequence only, so the unlocking scripts it now carries do not disturb the
// result — which is what makes recomputation possible at all after the fact.
func (c *Compiled) Preimage(tx *transaction.Transaction, inputIndex int, prevLock []byte, prevSats uint64) ([]byte, error) {
	if tx == nil {
		return nil, fmt.Errorf("cellscript: nil transaction")
	}
	if inputIndex < 0 || inputIndex >= len(tx.Inputs) {
		return nil, fmt.Errorf("cellscript: input index %d out of range (%d inputs)", inputIndex, len(tx.Inputs))
	}
	return c.preimage(tx.Hex(), inputIndex, hex.EncodeToString(prevLock), prevSats)
}

func (c *Compiled) preimage(txHex string, inputIndex int, lockHex string, prevSats uint64) ([]byte, error) {
	// ComputeOpPushTx returns (signature, preimage) — in that order.
	_, preimage, err := runar.ComputeOpPushTxWithCodeSep(
		txHex, inputIndex, lockHex, int64(prevSats), c.codeSepOffset, //nolint:gosec // satoshi values are far inside int64
	)
	if err != nil {
		return nil, fmt.Errorf("cellscript: compute preimage for input %d: %w", inputIndex, err)
	}
	return preimage, nil
}

// ScriptCode returns the part of a cell locking script that its spender's
// sighash preimage commits to: everything after the OP_CODESEPARATOR that scopes
// the step method.
//
// This is what makes a preimage worth reading. The scriptCode field inside it is
// a copy of the output being spent — code AND state — so a transaction's own
// unlocking script contains the row its covenant read, and an auditor can
// recover it without fetching anything else. Comparing that copy against the
// real output is what turns "the spender says it read this row" into "the
// spender read this row".
func (c *Compiled) ScriptCode(lockingScript []byte) ([]byte, error) {
	if len(lockingScript) <= c.codeSepOffset+1 {
		return nil, fmt.Errorf("cellscript: script of %d bytes is too short to be a cell locking script",
			len(lockingScript))
	}
	return lockingScript[c.codeSepOffset+1:], nil
}

// Binding is what a cell locking script says it is: which cell of the ring it
// verifies, and the row it carries as state.
//
// Recovering this from a script is what makes the automaton auditable at all.
// The chain enforces each cell's own bit and says NOTHING about whether the
// neighbourhood it read matches what its neighbours did — see the package docs
// and TestOtherCellsBitsAreNotChecked — so the only way to check that N
// independent chains ran the same automaton is to read back what each of them
// asserted. This is that assertion: cell Cell read its three neighbourhood bits
// out of Row.
type Binding struct {
	// Cell is the ring index the script verifies, recovered from the six
	// neighbourhood constants baked into its code rather than assumed from
	// whatever record pointed us at the script.
	Cell int
	// Row is the state the output carries: the whole row, as THIS cell claims
	// it. Nothing in Script forces it to agree with any other cell's.
	Row ca.Row
}

// Decode recovers the binding a cell locking script encodes.
//
// It is the exact inverse of LockingScript, and it proves it: the cell index
// comes from matching the script's code part against every cell's compiled code,
// the row comes from the state after the OP_RETURN, and the pair is then used to
// rebuild the whole script and compare it byte for byte with the input. So a
// successful decode is not a parse that happened to work — it is a demonstration
// that this script IS cell N's binding of this contract carrying this row, and
// nothing else. Anything a byte away from that is refused rather than guessed at.
func (c *Compiled) Decode(lockingScript []byte) (Binding, error) {
	index, err := c.bindingIndex()
	if err != nil {
		return Binding{}, err
	}

	rowBytes := c.cells / 8
	// The state's push encoding depends only on the row's LENGTH, which is fixed
	// for a ring, so serializing a throwaway row of the right size reveals how
	// many bytes the suffix takes without this code having to know how the
	// compiler encodes a push.
	suffix := len(c.stateSuffixHex(make(ca.Row, rowBytes))) / 2
	if len(lockingScript) <= suffix {
		return Binding{}, fmt.Errorf(
			"cellscript: script of %d bytes is too short to carry a %d-cell row", len(lockingScript), c.cells)
	}

	split := len(lockingScript) - suffix
	if lockingScript[split] != script.OpRETURN {
		return Binding{}, fmt.Errorf(
			"cellscript: byte %d is %#x, not the OP_RETURN that introduces a cell's state",
			split, lockingScript[split])
	}
	row := ca.Row(bytes.Clone(lockingScript[len(lockingScript)-rowBytes:]))

	cell, ok := index[string(lockingScript[:split])]
	if !ok {
		return Binding{}, fmt.Errorf(
			"cellscript: script's code part matches no cell of a %d-cell ring running rule %d "+
				"(so it is not this deployment's covenant, or the contract has been recompiled since)",
			c.cells, c.rule)
	}

	want, err := c.LockingScript(cell, row)
	if err != nil {
		return Binding{}, err
	}
	if !bytes.Equal(want, lockingScript) {
		return Binding{}, fmt.Errorf(
			"cellscript: script decoded as cell %d carrying row %s, but that binding rebuilds to "+
				"%d bytes that differ from the %d given", cell, row.Hex(), len(want), len(lockingScript))
	}
	return Binding{Cell: cell, Row: row}, nil
}

// bindingIndex maps every cell's code part to its cell index.
//
// The code part is the locking script minus its state, so it is constant across
// generations (TestCodePartIsStableAcrossGenerations) and distinct per cell
// (TestCellsHaveDistinctScripts) — the six neighbourhood constants see to that.
// Those two properties are what make this a lookup table rather than a heuristic,
// and the duplicate check below refuses to build one if the second ever stops
// holding: two cells sharing a code part would make Decode's answer arbitrary,
// and an arbitrary cell index is worse than no answer at all.
func (c *Compiled) bindingIndex() (map[string]int, error) {
	c.bindingsOnce.Do(func() {
		row := mustRow(c.cells)
		index := make(map[string]int, c.cells)
		for cell := range c.cells {
			code, err := c.CodePart(cell, row)
			if err != nil {
				c.bindingsErr = err
				return
			}
			if prev, dup := index[string(code)]; dup {
				c.bindingsErr = fmt.Errorf(
					"cellscript: cells %d and %d compile to identical code, so a script cannot be "+
						"attributed to either", prev, cell)
				return
			}
			index[string(code)] = cell
		}
		c.bindings = index
	})
	return c.bindings, c.bindingsErr
}

// unlockArgs is how many values a cell unlocking script pushes. See
// UnlockingScript for what they are and why there is nothing else.
const unlockArgs = 6

// Unlock is a cell unlocking script decoded back into the arguments it pushed.
type Unlock struct {
	// CodePart is the code the covenant rebuilds its continuation output from.
	CodePart []byte
	// NextRow is the successor row the spender presented. Script checks exactly
	// one bit of it — this cell's.
	NextRow ca.Row
	// ChangePKH and ChangeAmount describe the change output the covenant was
	// told to expect, and NewAmount the value it was told to continue with.
	ChangePKH    []byte
	ChangeAmount uint64
	NewAmount    uint64
	// Preimage is the sighash preimage the covenant verified against, and the
	// only argument that describes the spend rather than the result. See
	// Preimage and ScriptCode.
	Preimage []byte
}

// DecodeUnlocking parses a cell unlocking script back into its arguments.
//
// The layout is UnlockingScript's, and this must be read beside it. There is no
// signature and no method selector to skip: the script is six pushes and
// nothing else, which is the only reason a reader can index them positionally.
func (c *Compiled) DecodeUnlocking(unlockingScript []byte) (Unlock, error) {
	chunks, err := script.NewFromBytes(unlockingScript).Chunks()
	if err != nil {
		return Unlock{}, fmt.Errorf("cellscript: parse unlocking script: %w", err)
	}
	if len(chunks) != unlockArgs {
		return Unlock{}, fmt.Errorf(
			"cellscript: unlocking script pushes %d values, want %d "+
				"(codePart, nextRow, changePKH, changeAmount, newAmount, preimage)",
			len(chunks), unlockArgs)
	}

	nextRow := ca.Row(pushedBytes(chunks[1]))
	if len(nextRow) != c.cells/8 {
		return Unlock{}, fmt.Errorf(
			"cellscript: unlocking script presents a %d-byte row, want %d for %d cells",
			len(nextRow), c.cells/8, c.cells)
	}
	changeAmount, err := scriptUint(chunks[3])
	if err != nil {
		return Unlock{}, fmt.Errorf("cellscript: change amount: %w", err)
	}
	newAmount, err := scriptUint(chunks[4])
	if err != nil {
		return Unlock{}, fmt.Errorf("cellscript: continuation amount: %w", err)
	}

	return Unlock{
		CodePart:     pushedBytes(chunks[0]),
		NextRow:      nextRow,
		ChangePKH:    pushedBytes(chunks[2]),
		ChangeAmount: changeAmount,
		NewAmount:    newAmount,
		Preimage:     chunks[5].Data,
	}, nil
}

// pushedBytes recovers the byte string a chunk actually puts on the stack.
//
// It is not always chunk.Data. runar.EncodePushData encodes minimally, so a
// value of 1 to 16 becomes OP_1..OP_16 and 0x81 becomes OP_1NEGATE — opcodes
// that carry no data but push a one-byte string all the same. Only a ring of 8
// cells has a row small enough to be encoded that way, and a decoder that read
// chunk.Data alone would report every one of its transitions as malformed while
// working perfectly at 128.
func pushedBytes(chunk *script.ScriptChunk) []byte {
	switch {
	case chunk.Op == script.Op0:
		return nil // the empty string
	case chunk.Op == script.Op1NEGATE:
		return []byte{0x81}
	case chunk.Op >= script.Op1 && chunk.Op <= script.Op16:
		return []byte{chunk.Op - script.Op1 + 1}
	}
	return chunk.Data
}

// scriptUint decodes a pushed Script number that must be non-negative.
//
// Script numbers are little-endian sign-magnitude with the sign in the high bit
// of the last byte, so a value that reads as negative here is not a satoshi
// amount however it was meant — and silently taking its magnitude would report
// an amount the covenant never saw.
func scriptUint(chunk *script.ScriptChunk) (uint64, error) {
	b := pushedBytes(chunk)
	if len(b) == 0 {
		return 0, nil
	}
	if len(b) > 8 {
		return 0, fmt.Errorf("%d bytes is not a Script number", len(b))
	}
	if b[len(b)-1]&0x80 != 0 {
		return 0, fmt.Errorf("Script number %x is negative", b)
	}
	var n uint64
	for i := len(b) - 1; i >= 0; i-- {
		n = n<<8 | uint64(b[i])
	}
	return n, nil
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
