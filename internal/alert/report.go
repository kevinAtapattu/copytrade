package alert

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
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

func closedIn(p model.SimPosition, from, to time.Time) bool {
	return p.Status != model.PosOpen && p.ClosedAt != nil && p.ClosedAt.After(from) && !p.ClosedAt.After(to)
}

// CopyLines returns paper copies at delay that closed in (from, to].
func CopyLines(ctx context.Context, st store.Store, delay int, from, to time.Time) ([]CopyLine, error) {
	look := from.Add(-48 * time.Hour) // positions can open well before they close
	ps, err := st.ListSimPositions(ctx, store.SimFilter{DelayS: delay, Since: look})
	if err != nil {
		return nil, err
	}
	tts, err := st.ListTargetTrades(ctx, store.TradeFilter{Since: look})
	if err != nil {
		return nil, err
	}
	byWallet := map[string][]model.Trade{}
	for _, t := range tts {
		byWallet[t.Trader] = append(byWallet[t.Trader], t)
	}
	r := newReporter(ctx, st)
	var out []CopyLine
	for _, p := range ps {
		if !closedIn(p, from, to) {
			continue
		}
		l := CopyLine{Wallet: r.name(p.Wallet), Mint: short(p.Mint), PnlUSD: p.RealizedSol() * p.SolUSD,
			TheirPct: r.theirPct(ctx, byWallet[p.Wallet], p.Mint, to), HoldS: p.ClosedAt.Sub(p.OpenedAt).Seconds(),
			Migrated: p.Status == model.PosMigrated}
		if p.CostSol > 0 {
			l.PnlPct = p.RealizedSol() / p.CostSol
		}
		out = append(out, l)
	}
	return out, nil
}

// ExecClosedText describes an executor position that just closed, with the
// wallet's own result on the token and its recent copy record.
func ExecClosedText(ctx context.Context, st store.Store, p model.LivePosition, delay int, now time.Time) string {
	r := newReporter(ctx, st)
	pnl := (p.ProceedsSol - p.CostSol) * p.SolUSD
	pct := 0.0
	if p.CostSol > 0 {
		pct = (p.ProceedsSol - p.CostSol) / p.CostSol
	}
	hold := 0.0
	if p.ClosedAt != nil {
		hold = p.ClosedAt.Sub(p.OpenedAt).Seconds()
	}
	mode := strings.ToUpper(p.Mode)
	s := fmt.Sprintf("[%s] Closed copy of %s on %s: %s (%s), held %s", mode, r.name(p.Wallet), short(p.Mint), USD(pnl), Pct(pct), Dur(hold))
	tts, _ := st.ListTargetTrades(ctx, store.TradeFilter{Wallet: p.Wallet, Mint: p.Mint})
	if their := r.theirPct(ctx, tts, p.Mint, now); !math.IsNaN(their) {
		s += fmt.Sprintf(". They made %s.", Pct(their))
	}
	if scores, err := st.LatestScores(ctx); err == nil {
		for _, sc := range scores {
			if sc.Wallet == p.Wallet && sc.DelayS == delay && sc.WindowDays == 14 {
				s += fmt.Sprintf("\n%s, 14d paper record at +%ds: %s over %d copies, %.0f%% won.", r.name(p.Wallet), delay, USD(sc.PnlUSD), sc.Positions, sc.WinRate*100)
			}
		}
	}
	return s
}

// Digest is the once-a-day summary of the last 24h.
func Digest(ctx context.Context, st store.Store, delay int, now time.Time) (string, error) {
	since := now.Add(-24 * time.Hour)
	r := newReporter(ctx, st)
	ps, err := st.ListSimPositions(ctx, store.SimFilter{DelayS: delay, Since: since.Add(-48 * time.Hour)})
	if err != nil {
		return "", err
	}
	type agg struct {
		n, wins int
		pnl     float64
	}
	by := map[string]*agg{}
	var net float64
	closed, open := 0, 0
	for _, p := range ps {
		if p.Status == model.PosOpen {
			open++
			continue
		}
		if !closedIn(p, since, now) {
			continue
		}
		a := by[p.Wallet]
		if a == nil {
			a = &agg{}
			by[p.Wallet] = a
		}
		v := p.RealizedSol() * p.SolUSD
		a.n++
		a.pnl += v
		if v > 0 {
			a.wins++
		}
		net += v
		closed++
	}
	type row struct {
		w string
		a *agg
	}
	var rows []row
	for w, a := range by {
		rows = append(rows, row{w, a})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].a.pnl > rows[j].a.pnl })

	var b strings.Builder
	fmt.Fprintf(&b, "Daily summary (last 24h, paper copies at +%ds)\n", delay)
	fmt.Fprintf(&b, "%d copies closed, %s net, %d still open\n", closed, USD(net), open)
	line := func(x row) string {
		return fmt.Sprintf("• %s: %s over %d copies, %d won\n", r.name(x.w), USD(x.a.pnl), x.a.n, x.a.wins)
	}
	if len(rows) > 0 {
		b.WriteString("Best:\n")
		for _, x := range rows[:min(3, len(rows))] {
			b.WriteString(line(x))
		}
	}
	if len(rows) > 3 {
		b.WriteString("Worst:\n")
		for i := len(rows) - 1; i >= max(3, len(rows)-2); i-- {
			b.WriteString(line(rows[i]))
		}
	}

	if lps, err := st.ListLivePositions(ctx, store.LiveFilter{Since: since.Add(-48 * time.Hour)}); err == nil {
		modes := map[string]float64{}
		count := map[string]int{}
		for _, p := range lps {
			if p.Status != model.PosOpen && p.ClosedAt != nil && p.ClosedAt.After(since) {
				modes[p.Mode] += (p.ProceedsSol - p.CostSol) * p.SolUSD
				count[p.Mode]++
			}
		}
		for _, m := range []string{"live", "paper"} {
			if count[m] > 0 {
				fmt.Fprintf(&b, "Executor (%s): %d closed, %s\n", m, count[m], USD(modes[m]))
			}
		}
	}

	var found, tracked []string
	if ws, err := st.ListWallets(ctx); err == nil {
		for _, w := range ws {
			if w.Status != model.StatusDropped {
				tracked = append(tracked, w.Address)
			}
			if w.Source == model.SourceDiscovery && w.AddedAt.After(since) {
				found = append(found, r.name(w.Address))
			}
		}
	}
	var dropped []string
	if rots, err := st.ListRotations(ctx, 500); err == nil {
		for _, x := range rots {
			if x.At.After(since) && x.ToStatus == model.StatusDropped {
				dropped = append(dropped, r.name(x.Wallet)+" ("+strings.TrimPrefix(x.Reason, "dropped: ")+")")
			}
		}
	}
	fmt.Fprintf(&b, "Tracking %d wallets", len(tracked))
	if len(found) > 0 {
		fmt.Fprintf(&b, " · discovered: %s", strings.Join(found, ", "))
	}
	b.WriteString("\n")
	if len(dropped) > 0 {
		fmt.Fprintf(&b, "Dropped: %s\n", strings.Join(dropped, "; "))
	}
	if n, err := st.CountTokenTradesSince(ctx, since); err == nil {
		fmt.Fprintf(&b, "PumpPortal: %d messages (~%.5f SOL)", n, float64(n)/10000*0.01)
	}
	out := b.String()
	if len(out) > MaxMsg {
		out = out[:MaxMsg-3] + "..."
	}
	return out, nil
}

func short(a string) string {
	if len(a) <= 10 {
		return a
	}
	return a[:4] + ".." + a[len(a)-4:]
}
