package contract

import _ "embed"

// Source is the Rúnar source of the Cell contract, embedded so the application
// compiles the exact contract it ships with rather than reading a file from
// disk at runtime.
//
// The same text is compiled two ways: by the Go toolchain (making Cell an
// ordinary, unit-testable Go type) and by the Rúnar compiler (producing the
// Bitcoin Script that runs on chain). Keeping one source for both is the point.
//
//go:embed Cell.runar.go
var Source string

// SourceName is the filename reported in compiler diagnostics.
const SourceName = "Cell.runar.go"
