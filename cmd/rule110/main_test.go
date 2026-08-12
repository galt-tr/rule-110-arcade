package main

import "testing"

// TestUIURL covers the printed "UI ready at" line, which is the first thing an
// operator copies out of the logs. The wildcard cases are the ones that matter:
// a container sets RULE110_ADDR=0.0.0.0:8110, and the old string concatenation
// rendered that as "http://localhost0.0.0.0:8110".
func TestUIURL(t *testing.T) {
	cases := []struct{ addr, want string }{
		{":8110", "http://localhost:8110"},
		{"0.0.0.0:8110", "http://localhost:8110"},
		{"[::]:8110", "http://localhost:8110"},
		{"127.0.0.1:9000", "http://127.0.0.1:9000"},
		{"rule110.internal:80", "http://rule110.internal:80"},
		{"[::1]:8110", "http://[::1]:8110"},
		// Not a host:port at all. Printing something imperfect beats failing to
		// print the line the operator is looking for.
		{"8110", "http://8110"},
	}
	for _, c := range cases {
		if got := uiURL(c.addr); got != c.want {
			t.Errorf("uiURL(%q) = %q, want %q", c.addr, got, c.want)
		}
	}
}
