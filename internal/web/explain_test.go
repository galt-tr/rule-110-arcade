package web

import (
	"io/fs"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestExplainers runs the explainer dialogs' own tests, in
// testdata/explain_test.js.
//
// Same shape and same reasoning as TestRenderer: a browser script with no build
// step, a hand-stubbed DOM, driven from Go so `go test ./...` stays the single
// entry point, and skipped rather than failed where node is absent.
//
// Kept separate from the renderer's harness because the two files stub
// different things — this one needs <dialog>, createElement and appendChild,
// none of which the renderer touches.
func TestExplainers(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not found; skipping the explainer tests (see testdata/explain_test.js)")
	}

	out, err := exec.CommandContext(t.Context(), node,
		filepath.Join("testdata", "explain_test.js")).CombinedOutput()
	if err != nil {
		t.Fatalf("explainer tests failed: %v\n%s", err, out)
	}
	t.Logf("\n%s", out)
}

// TestExplainerDoesNotClaimAgreement guards the one claim this page is not
// entitled to make.
//
// The covenant proves each cell's own bit and says nothing about whether the
// ring's chains were handed the same row. The UI used to say `128/128 agree`,
// which named exactly the property Script does not establish, and the README
// forbids that wording anywhere. An explainer is precisely where somebody would
// reintroduce it while trying to be helpful, so the prose is checked rather
// than merely reviewed.
//
// This is a check on ASSERTIONS of agreement, not on the word: the honest
// paragraph has to be able to say the chains are not forced to agree.
func TestExplainerDoesNotClaimAgreement(t *testing.T) {
	page := readStatic(t, "index.html")

	claims := []*regexp.Regexp{
		regexp.MustCompile(`(?i)\d+\s*/\s*\d+\s+agree`),
		regexp.MustCompile(`(?i)\ball\s+(cells|chains|transactions)\s+agree`),
		regexp.MustCompile(`(?i)\b(cells|chains)\s+agree\s+on\b`),
		regexp.MustCompile(`(?i)\bproves?\s+(that\s+)?(the\s+)?(whole|entire|complete)\s+(row|generation)\b`),
	}
	for _, re := range claims {
		if m := re.FindString(page); m != "" {
			t.Errorf("index.html claims cross-cell agreement: %q\n"+
				"Script does not enforce it. See README, \"What is proved on chain, and what is not\".", m)
		}
	}

	// And the disclaimer has to actually be there, or the guard above is
	// satisfied by saying nothing at all.
	for _, want := range []string{
		"Each transaction proves exactly one bit",
		"Nothing in Bitcoin Script forces",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("index.html no longer explains what is NOT proved: missing %q", want)
		}
	}
}

// TestExplainerDoesNotHardcodeRingSize keeps a deployment-specific number out of
// the prose.
//
// The ring size is fixed at genesis and differs between deployments; the
// explainer is served by all of them. Numbers that depend on the deployment are
// written as N in the markup and filled in from /api/state, so a ring of 512
// does not read a page telling it that it has 128 cells.
func TestExplainerDoesNotHardcodeRingSize(t *testing.T) {
	page := readStatic(t, "index.html")

	// Only the explainer section is in scope: the zoom control's comment
	// legitimately discusses a 256-cell ring as a sizing constraint.
	start := strings.Index(page, `<dialog id="dlgRule110"`)
	if start < 0 {
		t.Fatal("index.html no longer contains the Rule 110 dialog")
	}
	dialogs := page[start:]

	re := regexp.MustCompile(`(?i)\b(\d{2,4})[- ](cell|coin|chain|transaction)s?\b`)
	for _, m := range re.FindAllStringSubmatch(dialogs, -1) {
		// 8-cell and 256-cell appear in the worked example and the OP_RETURN
		// illustration, which describe a specific listing rather than this
		// deployment. Those are spelled out in <code> or introduced as an
		// example; anything else is a leak.
		switch m[1] {
		case "8", "256":
			continue
		}
		t.Errorf("explainer hardcodes a ring size: %q", m[0])
	}

	if !strings.Contains(dialogs, `class="js-cells"`) {
		t.Error("explainer no longer has js-cells placeholders to fill from /api/state")
	}
}

// TestStaticScriptsDoNotShadowWindowGlobals catches a hazard that only exists
// because these are classic scripts.
//
// wallet.js, app.js and explain.js share ONE global scope, and a top-level
// `function open(...)` in a classic script does not merely shadow window.open —
// it replaces it. explain.js originally named its dialog opener `open`, which
// silently redefined the function app.js calls to show a cell's transaction.
// Nothing would have failed loudly: clicking a cell would have opened an
// explainer.
//
// Cheap to check and impossible to notice by reading one file at a time, which
// is exactly the shape of bug worth a test.
func TestStaticScriptsDoNotShadowWindowGlobals(t *testing.T) {
	// The subset that is both a window method and a plausible thing to name a
	// function in this codebase.
	reserved := []string{
		"open", "close", "print", "stop", "focus", "blur", "scroll",
		"find", "alert", "confirm", "prompt", "name", "status", "length",
		"top", "self", "parent", "origin", "event", "screen", "frames",
	}

	for _, file := range []string{"app.js", "explain.js", "wallet.js"} {
		src := readStatic(t, file)
		for _, name := range reserved {
			// Top-level only: column zero. Anything indented is inside a
			// function or an IIFE and cannot reach the global object.
			decl := regexp.MustCompile(
				`(?m)^(?:function\s+` + name + `\s*\(|(?:const|let|var)\s+` + name + `\b)`)
			if decl.MatchString(src) {
				t.Errorf("%s declares `%s` at top level, which overwrites window.%s "+
					"for every other script on the page", file, name, name)
			}
		}
	}
}

func readStatic(t *testing.T, name string) string {
	t.Helper()
	b, err := fs.ReadFile(staticRoot, name)
	if err != nil {
		t.Fatalf("read embedded %s: %v", name, err)
	}
	return string(b)
}
