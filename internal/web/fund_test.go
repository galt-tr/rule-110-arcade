package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeFunder is a Funder the test drives: no wallet, no arcade, no money.
type fakeFunder struct {
	mu sync.Mutex

	// block, when non-nil, holds Accept until it is closed — for asserting that
	// a second concurrent payment is refused rather than queued.
	block chan struct{}

	err      error
	accepted int
	lastBEEF []byte
	lastTxID string
}

func (f *fakeFunder) Target() (FundingTarget, error) {
	return FundingTarget{
		Address:           "mpz7rAwYignR5bybEGP4aZQbeikjxiRQ2U",
		LockingScriptHex:  "76a914aabbccddeeff0011223344556677889900aabbcc88ac",
		Network:           "ttn",
		MinSatoshis:       10_000,
		SuggestedSatoshis: 500_000,
	}, nil
}

func (f *fakeFunder) Accept(_ context.Context, beef []byte) (Accepted, error) {
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepted++
	f.lastBEEF = beef
	if f.err != nil {
		return Accepted{}, f.err
	}
	return Accepted{TxID: "abc123", Satoshis: 500_000, Outputs: []uint32{2}}, nil
}

func (f *fakeFunder) AcceptTxID(_ context.Context, txid string) (Accepted, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accepted++
	f.lastTxID = txid
	if f.err != nil {
		return Accepted{}, f.err
	}
	return Accepted{TxID: txid, Satoshis: 1_000, Mined: true}, nil
}

func (f *fakeFunder) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.accepted
}

func fundServer(t *testing.T, f Funder, opts ...Option) *httptest.Server {
	t.Helper()
	all := append([]Option{WithFunder(f)}, opts...)
	srv := httptest.NewServer(
		New(newFakeAutomaton(), slog.New(slog.NewTextHandler(io.Discard, nil)), all...).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func postFund(t *testing.T, url, body string) *http.Response {
	t.Helper()
	res, err := http.Post(url+"/api/fund", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = res.Body.Close() })
	return res
}

// Without a funder the endpoints must not exist at all. A deployment that does
// not want public funding should not carry the surface and refuse on it.
func TestFundingRoutesAreAbsentWithoutAFunder(t *testing.T) {
	srv := httptest.NewServer(
		New(newFakeAutomaton(), slog.New(slog.NewTextHandler(io.Discard, nil))).Handler())
	defer srv.Close()

	res, err := http.Get(srv.URL + "/api/funding")
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusNotFound {
		t.Errorf("GET /api/funding = %d with no funder, want 404", res.StatusCode)
	}

	post := postFund(t, srv.URL, `{"txid":"aa"}`)
	if post.StatusCode != http.StatusNotFound {
		t.Errorf("POST /api/fund = %d with no funder, want 404", post.StatusCode)
	}
}

func TestFundingTargetIsServed(t *testing.T) {
	srv := fundServer(t, &fakeFunder{})

	res, err := http.Get(srv.URL + "/api/funding")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()

	var got FundingTarget
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Address == "" || got.LockingScriptHex == "" {
		t.Error("the payer was given no address and no script to pay")
	}
	if got.MinSatoshis == 0 {
		t.Error("no minimum was published, so a payer cannot know what will be refused")
	}
}

// TestOversizedPaymentIsRefusedWithoutReadingIt is the memory guard. The body
// cap must stop the read, not merely fail afterwards.
func TestOversizedPaymentIsRefusedWithoutReadingIt(t *testing.T) {
	f := &fakeFunder{}
	srv := fundServer(t, f)

	huge := base64.StdEncoding.EncodeToString(make([]byte, maxFundBody+1<<16))
	res := postFund(t, srv.URL, `{"beef":"`+huge+`"}`)

	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized payment = %d, want 413", res.StatusCode)
	}
	if f.calls() != 0 {
		t.Error("an oversized body reached the wallet")
	}
}

// Only one payment may be in flight. Beyond bounding the work an anonymous
// caller can commission, this is what stops two posts of the same transaction
// both passing the duplicate check.
func TestASecondConcurrentPaymentIsRefused(t *testing.T) {
	f := &fakeFunder{block: make(chan struct{})}
	srv := fundServer(t, f)

	started := make(chan struct{})
	go func() {
		close(started)
		res, err := http.Post(srv.URL+"/api/fund", "application/json",
			strings.NewReader(`{"beef":"AAAA"}`))
		if err == nil {
			_ = res.Body.Close()
		}
	}()
	<-started
	// Give the first request time to take the slot.
	time.Sleep(50 * time.Millisecond)

	res := postFund(t, srv.URL, `{"beef":"AAAA"}`)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("second concurrent payment = %d, want 429", res.StatusCode)
	}
	close(f.block)
}

// The bucket bounds sustained attempts, and it must refill — a payer who
// retries after a wallet error should not be locked out.
func TestFundingAttemptsAreRateLimitedAndRecover(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }

	f := &fakeFunder{}
	srv := fundServer(t, f, withClock(clock))

	for i := range fundBurst {
		if res := postFund(t, srv.URL, `{"beef":"AAAA"}`); res.StatusCode != http.StatusOK {
			t.Fatalf("attempt %d within the burst = %d, want 200", i, res.StatusCode)
		}
	}
	if res := postFund(t, srv.URL, `{"beef":"AAAA"}`); res.StatusCode != http.StatusTooManyRequests {
		t.Errorf("attempt past the burst = %d, want 429", res.StatusCode)
	}

	now = now.Add(fundRefill)
	if res := postFund(t, srv.URL, `{"beef":"AAAA"}`); res.StatusCode != http.StatusOK {
		t.Errorf("attempt after a refill = %d, want 200; a payer retrying is locked out", res.StatusCode)
	}
}

// TestFundingErrorsReachThePayerAsSomethingActionable is the whole reason the
// sentinels exist. Each of these has a different remedy, and a payer who is
// told "500" for all of them cannot tell which happened — including the case
// where their money was never at risk.
func TestFundingErrorsReachThePayerAsSomethingActionable(t *testing.T) {
	tests := map[string]struct {
		err  error
		want int
	}{
		"pays somebody else":  {ErrNoPaymentOutput, http.StatusBadRequest},
		"below the minimum":   {fmt.Errorf("%w: 5 satoshis", ErrPaymentTooSmall), http.StatusBadRequest},
		"unparseable":         {fmt.Errorf("%w: bad beef", ErrBadPayment), http.StatusBadRequest},
		"already credited":    {fmt.Errorf("%w: abc", ErrPaymentKnown), http.StatusConflict},
		"wrong chain":         {fmt.Errorf("%w: missing inputs", ErrPaymentRefused), http.StatusBadGateway},
		"something else went": {fmt.Errorf("the database is on fire"), http.StatusInternalServerError},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			srv := fundServer(t, &fakeFunder{err: test.err})
			res := postFund(t, srv.URL, `{"beef":"AAAA"}`)
			if res.StatusCode != test.want {
				t.Errorf("status = %d, want %d", res.StatusCode, test.want)
			}
			body, _ := io.ReadAll(res.Body)
			if test.want == http.StatusInternalServerError &&
				strings.Contains(string(body), "database is on fire") {
				t.Error("an internal error message was handed to a stranger")
			}
		})
	}
}

// A txid-only request routes to the by-id path, and a request carrying both
// prefers the BEEF: it is strictly more evidence.
func TestFundRoutesByWhatItWasGiven(t *testing.T) {
	byID := &fakeFunder{}
	srv := fundServer(t, byID)
	if res := postFund(t, srv.URL, `{"txid":"deadbeef"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("txid request = %d", res.StatusCode)
	}
	if byID.lastTxID != "deadbeef" {
		t.Errorf("txid path saw %q", byID.lastTxID)
	}

	both := &fakeFunder{}
	srv2 := fundServer(t, both)
	if res := postFund(t, srv2.URL, `{"beef":"AAAA","txid":"deadbeef"}`); res.StatusCode != http.StatusOK {
		t.Fatalf("combined request = %d", res.StatusCode)
	}
	if both.lastTxID != "" || len(both.lastBEEF) == 0 {
		t.Error("a request carrying both did not prefer the BEEF")
	}
}

// An empty request is the payer's mistake and must say so rather than reaching
// the wallet.
func TestFundRefusesARequestWithNoPayment(t *testing.T) {
	f := &fakeFunder{}
	srv := fundServer(t, f)

	if res := postFund(t, srv.URL, `{}`); res.StatusCode != http.StatusBadRequest {
		t.Errorf("empty request = %d, want 400", res.StatusCode)
	}
	if res := postFund(t, srv.URL, `{"beef":"not base64!!"}`); res.StatusCode != http.StatusBadRequest {
		t.Errorf("non-base64 payment = %d, want 400", res.StatusCode)
	}
	if f.calls() != 0 {
		t.Error("a request with no usable payment reached the wallet")
	}
}
