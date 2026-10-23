package snapshot

import (
	"context"
	"sort"
	"time"

	"copytrade/internal/curve"
	"copytrade/internal/model"
)

// HistoryReader answers curve reads from a recorded full trade stream: the state
// after the last trade at or before the requested moment. Used to test snapshot
// mode on replay data with no RPC.
type HistoryReader struct {
	byCurve map[string][]point
}

type point struct {
	at       time.Time
	st       curve.State
	migrated bool
}

func NewHistoryReader(trades []model.Trade) *HistoryReader {
	h := &HistoryReader{byCurve: map[string][]point{}}
	for _, t := range trades {
		if t.BondingCurveKey == "" {
			continue
		}
		h.byCurve[t.BondingCurveKey] = append(h.byCurve[t.BondingCurveKey], point{
			at: t.ReceivedAt, st: curve.State{VSol: t.VSol, VTokens: t.VTokens}, migrated: t.Pool != "" && t.Pool != "pump"})
	}
	for _, ps := range h.byCurve {
		sort.SliceStable(ps, func(i, j int) bool { return ps[i].at.Before(ps[j].at) })
	}
	return h
}

func (h *HistoryReader) Read(_ context.Context, curves []string, at time.Time) ([]Result, error) {
	out := make([]Result, len(curves))
	for i, c := range curves {
		ps := h.byCurve[c]
		k := sort.Search(len(ps), func(i int) bool { return ps[i].at.After(at) }) - 1
		if k < 0 {
			continue
		}
		out[i] = Result{Found: true, Account: curve.Account{State: ps[k].st, Complete: ps[k].migrated}}
	}
	return out, nil
}
