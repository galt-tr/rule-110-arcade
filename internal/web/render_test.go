package web

import (
	"os/exec"
	"path/filepath"
	"testing"
)

// TestRenderer runs the renderer's own tests, in testdata/render_test.js.
//
// The diagram is a plain browser script with no build step and no framework,
// which is the right shape for what it does but leaves nowhere to hang a test.
// The script stubs the DOM it touches and records the canvas context, so the
// thing worth asserting — how many rectangles a push actually paints — is
// directly countable.
//
// Driven from Go so `go test ./...` remains the single entry point, and skipped
// rather than failed where node is absent: the repository's checks are Go
// checks, and a renderer test that breaks the build on a machine without node
// would be worse than one that says why it did not run.
func TestRenderer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found; skipping the renderer tests (see testdata/render_test.js)")
	}

	out, err := exec.CommandContext(t.Context(), node,
		filepath.Join("testdata", "render_test.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("renderer tests failed: %v\n%s", err, out)
	}
	t.Logf("\n%s", out)
}
