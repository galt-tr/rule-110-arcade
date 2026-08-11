// Command asmdump disassembles the compiled Cell locking script and reports
// any opcode that BSV leaves disabled post-Genesis.
package main

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/icellan/runar/compilers/go/compiler"
	runar "github.com/icellan/runar/packages/runar-go"
)

// Disabled post-Genesis on BSV. OP_MUL/OP_DIV/OP_MOD/OP_LSHIFT/OP_RSHIFT and
// the bitwise ops came back; these two never did.
var disabled = map[byte]string{
	0x8d: "OP_2MUL",
	0x8e: "OP_2DIV",
}

func main() {
	a, err := compiler.CompileFromSource("contracts/Cell.runar.go")
	if err != nil {
		log.Fatal(err)
	}
	raw, err := compiler.ArtifactToJSON(a)
	if err != nil {
		log.Fatal(err)
	}
	var art runar.RunarArtifact
	if err := json.Unmarshal(raw, &art); err != nil {
		log.Fatal(err)
	}

	c := runar.NewRunarContract(&art, []interface{}{
		"01", int64(0), int64(4), int64(0), int64(2), int64(0), int64(1), int64(1), int64(110),
	})
	lockHex := c.GetLockingScript()

	s, err := script.NewFromHex(lockHex)
	if err != nil {
		log.Fatal(err)
	}
	chunks, err := s.Chunks()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("locking script: %d bytes, %d chunks\n\n", len(lockHex)/2, len(chunks))

	offset := 0
	hits := 0
	for i, ch := range chunks {
		if name, bad := disabled[ch.Op]; bad && len(ch.Data) == 0 {
			hits++
			fmt.Printf("DISABLED %s at chunk %d (byte offset %d)\n", name, i, offset)
			fmt.Printf("  context: %s\n\n", context(chunks, i))
		}
		offset += chunkLen(ch)
	}
	fmt.Printf("disabled-opcode occurrences: %d\n", hits)
}

func context(chunks []*script.ScriptChunk, i int) string {
	lo, hi := max(0, i-8), min(len(chunks), i+8)
	out := ""
	for j := lo; j < hi; j++ {
		if j == i {
			out += ">>" + name(chunks[j]) + "<< "
			continue
		}
		out += name(chunks[j]) + " "
	}
	return out
}

func name(ch *script.ScriptChunk) string {
	if len(ch.Data) > 0 {
		if len(ch.Data) > 8 {
			return fmt.Sprintf("<%d bytes>", len(ch.Data))
		}
		return fmt.Sprintf("<%x>", ch.Data)
	}
	if n, ok := opName[ch.Op]; ok {
		return n
	}
	return fmt.Sprintf("op_%02x", ch.Op)
}

func chunkLen(ch *script.ScriptChunk) int {
	if len(ch.Data) == 0 {
		return 1
	}
	n := len(ch.Data)
	switch {
	case n < 76:
		return 1 + n
	case n <= 0xff:
		return 2 + n
	case n <= 0xffff:
		return 3 + n
	default:
		return 5 + n
	}
}

// opName inverts go-sdk's name->byte table.
var opName = func() map[byte]string {
	m := make(map[byte]string, len(script.OpCodeStrings))
	for n, b := range script.OpCodeStrings {
		if _, taken := m[b]; !taken {
			m[b] = n
		}
	}
	return m
}()
