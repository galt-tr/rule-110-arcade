package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/dymurray/rule-110-arcade/internal/chain"
	"github.com/dymurray/rule-110-arcade/internal/web"
)

// funder adapts the chain to what the HTTP layer needs of it.
//
// It lives here, in the composition root, rather than in either package,
// because it exists only to join them. web declares its own request and error
// types deliberately — that is what lets its defences be tested with a fake and
// no wallet — so something has to translate, and putting the translation in
// either package would undo the separation it depends on.
type funder struct {
	chain     *chain.Chain
	network   string
	min       uint64
	suggested uint64
}

func newFunder(c *chain.Chain, suggested uint64) *funder {
	return &funder{
		chain:     c,
		network:   string(c.Config.Network),
		min:       c.Config.MinPaymentSatoshis,
		suggested: suggested,
	}
}

func (f *funder) Target() (web.FundingTarget, error) {
	target, err := f.chain.FundingTarget()
	if err != nil {
		return web.FundingTarget{}, err
	}
	return web.FundingTarget{
		LockingScriptHex:  target.LockingScriptHex,
		Network:           f.network,
		MinSatoshis:       f.min,
		SuggestedSatoshis: f.suggested,
	}, nil
}

func (f *funder) Accept(ctx context.Context, beef []byte) (web.Accepted, error) {
	res, err := f.chain.AcceptPayment(ctx, beef, f.min)
	return accepted(res), translate(err)
}

func accepted(r *chain.InternalizeResult) web.Accepted {
	if r == nil {
		return web.Accepted{}
	}
	return web.Accepted{TxID: r.TxID, Satoshis: r.Satoshis, Outputs: r.Outputs, Mined: r.Mined}
}

// translate maps the chain's funding sentinels onto the web layer's.
//
// The message is preserved and the sentinel is replaced, because the two carry
// different halves of what a payer needs: the sentinel decides the status code,
// and the message is where "your coins are on a different chain" or "the
// minimum is N satoshis" actually lives. Anything unrecognised is passed
// through unchanged and becomes a 500 with an opaque body — an internal error
// is not something to hand a stranger.
func translate(err error) error {
	if err == nil {
		return nil
	}
	for _, pair := range []struct {
		from, to error
	}{
		{chain.ErrNoPaymentOutput, web.ErrNoPaymentOutput},
		{chain.ErrPaymentTooSmall, web.ErrPaymentTooSmall},
		{chain.ErrPaymentKnown, web.ErrPaymentKnown},
		{chain.ErrPaymentRefused, web.ErrPaymentRefused},
		{chain.ErrBadPayment, web.ErrBadPayment},
	} {
		if errors.Is(err, pair.from) {
			return fmt.Errorf("%w: %s", pair.to, trimPrefix(err.Error(), pair.from.Error()))
		}
	}
	return err
}

// trimPrefix drops the chain sentinel's own text from a wrapped message, so the
// payer sees one explanation rather than the same idea twice in two vocabularies.
func trimPrefix(msg, prefix string) string {
	if len(msg) > len(prefix)+2 && msg[:len(prefix)] == prefix {
		return msg[len(prefix)+2:]
	}
	return msg
}
