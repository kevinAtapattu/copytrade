package alert

import (
	"context"
	"math"
	"sort"
	"time"

	"copytrade/internal/compare"
	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/store"
)

// Reports are built from the database for a time window, so a restart never loses
// or double-sends a roll-up (the alerter stores the last window end in settings).

type reporter struct {
	st     store.Store
	labels map[string]string
	toks   map[string]*model.Token
}

func newReporter(ctx context.Context, st store.Store) *reporter {
	r := &reporter{st: st, labels: map[string]string{}, toks: map[string]*model.Token{}}
	if ws, err := st.ListWallets(ctx); err == nil {
		for _, w := range ws {
			r.labels[w.Address] = w.Label
		}
	}
	return r
}

func (r *reporter) name(addr string) string {
	if l := r.labels[addr]; l != "" {
		return l
	}
	return short(addr)
}

func (r *reporter) token(ctx context.Context) func(string) *model.Token {
	return func(mint string) *model.Token {
		if t, ok := r.toks[mint]; ok {
			return t
		}
		var tp *model.Token
		if t, ok, err := r.st.GetToken(ctx, mint); err == nil && ok {
			tp = &t
		}
		r.toks[mint] = tp
		return tp
	}
}

// theirPct is the wallet's own return on mint (NaN if we never saw their buy).
func (r *reporter) theirPct(ctx context.Context, trades []model.Trade, mint string, now time.Time) float64 {
	var mine []model.Trade
	for _, t := range trades {
		if t.Mint == mint {
			mine = append(mine, t)
		}
	}
	rows, _ := compare.Build(mine, nil, r.token(ctx), curve.Fees{}, now)
	if len(rows) == 0 {
		return math.NaN()
	}
	return rows[0].TheirPct
}

// ExecLines returns the executor's copies that closed in (from, to], oldest
// first, and the executor's mode ("" if none closed).
func ExecLines(ctx context.Context, st store.Store, from, to time.Time) ([]CopyLine, string, error) {
	look := from.Add(-48 * time.Hour) // copies can open well before they close
	ps, err := st.ListLivePositions(ctx, store.LiveFilter{Since: look})
	if err != nil {
		return nil, "", err
	}
	r := newReporter(ctx, st)
	trades := map[string][]model.Trade{} // wallet|mint -> their trades
	var out []CopyLine
	var at []time.Time
	mode := ""
	for _, p := range ps {
		if p.Status == model.PosOpen || p.ClosedAt == nil || !p.ClosedAt.After(from) || p.ClosedAt.After(to) {
			continue
		}
		k := p.Wallet + "|" + p.Mint
		if _, ok := trades[k]; !ok {
			trades[k], _ = st.ListTargetTrades(ctx, store.TradeFilter{Wallet: p.Wallet, Mint: p.Mint})
		}
		l := CopyLine{Wallet: r.name(p.Wallet), Mint: short(p.Mint), PnlUSD: (p.ProceedsSol - p.CostSol) * p.SolUSD,
			TheirPct: r.theirPct(ctx, trades[k], p.Mint, to), HoldS: p.ClosedAt.Sub(p.OpenedAt).Seconds()}
		if p.CostSol > 0 {
			l.PnlPct = (p.ProceedsSol - p.CostSol) / p.CostSol
		}
		out = append(out, l)
		at = append(at, *p.ClosedAt)
		mode = p.Mode
	}
	idx := make([]int, len(out))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(i, j int) bool { return at[idx[i]].Before(at[idx[j]]) })
	sorted := make([]CopyLine, len(out))
	for i, j := range idx {
		sorted[i] = out[j]
	}
	return sorted, mode, nil
}

func short(a string) string {
	if len(a) <= 10 {
		return a
	}
	return a[:4] + ".." + a[len(a)-4:]
}
