package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"time"
)

// Funder is what the HTTP layer needs in order to accept a payment.
//
// Narrow, and an interface for the same reason Automaton is one: this is the
// only endpoint an anonymous caller can use to make the wallet and the network
// do work, so its defences have to be testable without a wallet, an arcade or
// money.
type Funder interface {
	// Target is what a payer needs in order to build the payment.
	Target() (FundingTarget, error)
	// Accept takes a BEEF a browser wallet produced.
	Accept(ctx context.Context, beef []byte) (Accepted, error)
}

// FundingTarget is the payment instruction handed to a browser.
//
// Declared here rather than reused from the chain package so a test fake needs
// no chain import — the same instinct as Automaton.
type FundingTarget struct {
	LockingScriptHex  string `json:"lockingScript"`
	Network           string `json:"network"`
	MinSatoshis       uint64 `json:"minSatoshis"`
	SuggestedSatoshis uint64 `json:"suggestedSatoshis"`
}

// Accepted is what a credited payment reports back.
type Accepted struct {
	TxID     string   `json:"txid"`
	Satoshis uint64   `json:"satoshis"`
	Outputs  []uint32 `json:"outputs"`
	Mined    bool     `json:"mined"`
}

// The errors a Funder may return that map onto something a payer can act on.
// The web package declares them so the mapping can be tested with a fake, and
// the chain package's sentinels satisfy them by being wrapped.
var (
	ErrNoPaymentOutput = errors.New("no output pays this deployment")
	ErrPaymentTooSmall = errors.New("payment below the minimum")
	ErrPaymentKnown    = errors.New("payment already credited")
	ErrPaymentRefused  = errors.New("the network refused this payment")
	ErrBadPayment      = errors.New("the payment could not be parsed")
)

const (
	// maxFundBody bounds the request. A payment BEEF carries the transaction and
	// its proven ancestry, which is generous at a megabyte and nowhere near
	// enough to be interesting as a memory attack.
	maxFundBody = 1 << 20

	// fundTimeout bounds one attempt end to end: a broadcast and an internalize,
	// both of which touch the network. It also bounds how long a single caller
	// can hold the one in-flight slot.
	fundTimeout = 30 * time.Second

	// fundBurst and fundRefill are the token bucket. Three in hand and one every
	// five seconds is ample for a person funding a deployment — including
	// retrying a couple of times after a wallet error — and it caps what an
	// anonymous caller can commission.
	fundBurst  = 3
	fundRefill = 5 * time.Second
)

// bucket is a token bucket.
//
// GLOBAL, not per-IP, and that is deliberate. Behind an ingress RemoteAddr is
// the proxy and X-Forwarded-For is written by the caller, so a per-IP limiter
// here limits nobody while looking like it does. What is actually being
// protected is the wallet and the arcade behind it, and those are shared, so a
// shared budget is the honest shape.
type bucket struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	burst  float64
	refill time.Duration
	now    func() time.Time // injectable so the test does not sleep
}

func newBucket(burst float64, refill time.Duration, now func() time.Time) *bucket {
	if now == nil {
		now = time.Now
	}
	return &bucket{tokens: burst, last: now(), burst: burst, refill: refill, now: now}
}

func (b *bucket) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	t := b.now()
	b.tokens = min(b.burst, b.tokens+t.Sub(b.last).Seconds()/b.refill.Seconds())
	b.last = t
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// fundRequest is what a payer posts.
//
// A BEEF and nothing else. This endpoint used to take a bare txid as well, for
// somebody who had sent coin to the funding address by hand, and that path is
// gone: a transaction fetched by id carries no ancestry, so crediting one meant
// waiting for it to be mined and trusting an oracle for the merkle proof. A
// BEEF carries its own proof of where the money came from, which is the whole
// reason a BRC-100 wallet can be paid from and believed in one step.
type fundRequest struct {
	// BEEF is base64 because the browser gets a byte array from the wallet
	// substrate, and base64 is half the size of hex on the wire.
	BEEF string `json:"beef,omitempty"`
}

func (s *Server) handleFundingTarget(w http.ResponseWriter, _ *http.Request) {
	if s.funder == nil {
		http.NotFound(w, nil)
		return
	}
	target, err := s.funder.Target()
	if err != nil {
		s.logger.Error("derive funding target", "err", err)
		http.Error(w, "funding is unavailable", http.StatusInternalServerError)
		return
	}
	writeJSON(w, target)
}

func (s *Server) handleFund(w http.ResponseWriter, r *http.Request) {
	if s.funder == nil {
		http.NotFound(w, nil)
		return
	}

	// One payment at a time, process-wide. Beyond bounding the work an anonymous
	// caller can commission, this makes the duplicate check and the credit
	// effectively atomic at the HTTP layer as well as inside the chain: two
	// concurrent posts of the same transaction cannot both pass the "is this
	// known" test.
	select {
	case s.fundSlot <- struct{}{}:
		defer func() { <-s.fundSlot }()
	default:
		http.Error(w, "another payment is being processed; try again in a moment",
			http.StatusTooManyRequests)
		return
	}

	if !s.fundBucket.allow() {
		http.Error(w, "too many funding attempts; try again in a moment", http.StatusTooManyRequests)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxFundBody)
	var req fundRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// MaxBytesReader's error surfaces here; both this and malformed JSON are
		// the caller's problem and neither is worth distinguishing for them.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "payment too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request body", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), fundTimeout)
	defer cancel()

	if req.BEEF == "" {
		http.Error(w, "send a beef: this deployment is funded from a BRC-100 wallet",
			http.StatusBadRequest)
		return
	}
	raw, decodeErr := base64.StdEncoding.DecodeString(req.BEEF)
	if decodeErr != nil {
		http.Error(w, "the payment is not valid base64", http.StatusBadRequest)
		return
	}
	res, err := s.funder.Accept(ctx, raw)
	if err != nil {
		s.writeFundError(w, err)
		return
	}
	writeJSON(w, res)
}

// writeFundError maps a funding failure onto a status and a message the payer
// can act on.
//
// The distinctions are the point. "Your coins are on a different chain",
// "we already have this one" and "that is below the minimum" have completely
// different remedies, and a payer who gets 500 for all three has no way to tell
// which happened — including the case where their money was never at risk.
// Anything unrecognised stays opaque, because an internal error message is not
// something to hand a stranger.
func (s *Server) writeFundError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNoPaymentOutput):
		http.Error(w, "no output in that transaction pays this deployment", http.StatusBadRequest)
	case errors.Is(err, ErrPaymentTooSmall):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrBadPayment):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrPaymentKnown):
		http.Error(w, "that payment has already been credited", http.StatusConflict)
	case errors.Is(err, ErrPaymentRefused):
		// 502, not 400: the refusal came from the network, and on this
		// deployment it almost always means the payer's wallet is backed by a
		// different chain. Nothing was spent.
		http.Error(w, err.Error(), http.StatusBadGateway)
	default:
		s.logger.Error("accept payment", "err", err)
		http.Error(w, "the payment could not be processed", http.StatusInternalServerError)
	}
}
