package main

import (
	"context"

	"github.com/dymurray/rule-110-arcade/internal/history"
	"github.com/dymurray/rule-110-arcade/internal/web"
)

// archive adapts the history store to what the HTTP layer needs of it.
//
// Here, in the composition root, for the same reason funder is: web declares
// its own types so its handlers can be tested without a database, so something
// has to translate, and putting the translation in either package would undo
// the separation it depends on.
type archive struct{ store *history.Store }

func (a archive) Window(ctx context.Context, from uint64, count, cells int) ([]web.CompactGeneration, error) {
	gens, err := a.store.LoadCompact(ctx, from, count, cells)
	if err != nil {
		return nil, err
	}
	out := make([]web.CompactGeneration, 0, len(gens))
	for _, g := range gens {
		out = append(out, web.CompactGeneration{Number: g.Number, RowHex: g.RowHex, States: g.States})
	}
	return out, nil
}

func (a archive) Extent(ctx context.Context) (oldest, newest uint64, ok bool, err error) {
	return a.store.Extent(ctx)
}

func (a archive) Cell(ctx context.Context, generation uint64, cell int) (web.CellDetail, bool, error) {
	d, ok, err := a.store.CellAt(ctx, generation, cell)
	if err != nil || !ok {
		return web.CellDetail{}, ok, err
	}
	return web.CellDetail{TxID: d.TxID, Status: d.Status, Err: d.Err}, true, nil
}
