package nonsolsim

import (
	"context"
	"sync"
	"testing"
	"time"

	"copytrade/internal/model"
)

const (
	nad  = "NAD"
	mint = "MINTpump"
)

type fakeQuoter struct {
	mu    sync.Mutex
	calls []string
	buy   func(lamports uint64) (Quote, error)
	sell  func(tokens uint64) (Quote, error)
}

func (f *fakeQuoter) Quote(_ context.Context, in, out string, amount uint64) (Quote, error) {
	f.mu.Lock()
	f.calls = append(f.calls, in+">"+out)
	f.mu.Unlock()
	if in == WSOL {
		return f.buy(amount)
	}
	return f.sell(amount)
}

type fakeStore struct {
	mu   sync.Mutex
	rows map[string]model.NonsolCopy
	ws   []model.Wallet
}

func (s *fakeStore) SaveNonsolCopy(_ context.Context, c model.NonsolCopy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[c.ID] = c
	return nil
}
func (s *fakeStore) OpenNonsolCopies(context.Context) ([]model.NonsolCopy, error) {
	var out []model.NonsolCopy
	for _, c := range s.rows {
		if c.Status == model.NonsolOpen {
			out = append(out, c)
		}
	}
	return out, nil
}
func (s *fakeStore) ListWallets(context.Context) ([]model.Wallet, error) { return s.ws, nil }
func (s *fakeStore) all() []model.NonsolCopy {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.NonsolCopy
	for _, c := range s.rows {
		out = append(out, c)
	}
	return out
}

// 1 token raw per lamport on the buy; sells return half a lamport per token.
func setup(t *testing.T) (*Engine, *fakeQuoter, *fakeStore, *time.Time) {
	q := &fakeQuoter{
		buy: func(l uint64) (Quote, error) {
			return Quote{InAmount: l, OutAmount: l * 1000, Route: "Meteora DLMM > Pump.fun"}, nil
		},
		sell: func(tk uint64) (Quote, error) { return Quote{InAmount: tk, OutAmount: tk / 500}, nil },
	}
	st := &fakeStore{rows: map[string]model.NonsolCopy{}, ws: []model.Wallet{{Address: nad, Status: model.StatusLive}}}
	e := New(q, st, func() float64 { return 100 })
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	e.Sleep = func(context.Context, time.Duration) {}
	if err := e.RefreshWallets(context.Background()); err != nil {
		t.Fatal(err)
	}
	return e, q, st, &now
}

func trade(sig, side string, amt, bal float64, at time.Time) model.Trade {
	return model.Trade{Signature: sig, Mint: mint, Trader: nad, Side: side, TokenAmount: amt, NewTokenBalance: bal,
		Pool: PoolNonSOL, IsTarget: true, ReceivedAt: at}
}

func TestBuyPartialSellClose(t *testing.T) {
	e, _, st, now := setup(t)
	ctx := context.Background()
	e.HandleTrade(ctx, trade("b1", "buy", 1000, 1000, *now))
	e.Wait()
	rows := st.all()
	if len(rows) != 1 || rows[0].Status != model.NonsolOpen {
		t.Fatalf("want one open copy, got %+v", rows)
	}
	c := rows[0]
	// $10 at $100/SOL = 0.1 SOL = 1e8 lamports -> 1e11 raw tokens.
	if c.SolIn != 0.1 || c.TokensBought != 1e11 || c.TokensHeld != 1e11 || c.Route != "Meteora DLMM > Pump.fun" {
		t.Fatalf("bad buy: %+v", c)
	}
	// They sell 40% (400 of 1000).
	e.HandleTrade(ctx, trade("s1", "sell", 400, 600, *now))
	e.Wait()
	c = st.all()[0]
	if c.TokensHeld != 6e10 || c.Sells != 1 || c.Status != model.NonsolOpen || c.SolOut != 0.08 {
		t.Fatalf("bad partial sell: %+v", c)
	}
	// They sell the rest.
	e.HandleTrade(ctx, trade("s2", "sell", 600, 0, *now))
	e.Wait()
	c = st.all()[0]
	if c.Status != model.NonsolClosed || c.TokensHeld != 0 || c.ClosedAt == nil || c.Sells != 2 {
		t.Fatalf("not closed: %+v", c)
	}
	if got := c.SolOut; got < 0.1999 || got > 0.2001 {
		t.Fatalf("sol out %v", got)
	}
	if c.FeesSol < 3*0.000505-1e-9 || c.FeesSol > 3*0.000505+1e-9 {
		t.Fatalf("fees %v", c.FeesSol)
	}
	if e.OpenCount() != 0 {
		t.Fatal("still open in memory")
	}
}

func TestIgnoreBuysWhileOpen(t *testing.T) {
	e, q, st, now := setup(t)
	ctx := context.Background()
	e.HandleTrade(ctx, trade("b1", "buy", 1000, 1000, *now))
	e.HandleTrade(ctx, trade("b2", "buy", 500, 1500, *now))
	e.Wait()
	if n := len(st.all()); n != 1 {
		t.Fatalf("want 1 copy, got %d", n)
	}
	if len(q.calls) != 1 {
		t.Fatalf("want 1 quote, got %v", q.calls)
	}
}

func TestSkipOnNoRoute(t *testing.T) {
	e, q, st, now := setup(t)
	q.buy = func(uint64) (Quote, error) {
		return Quote{}, &NoRouteError{Code: "TOKEN_NOT_TRADABLE", Msg: "not tradable"}
	}
	ctx := context.Background()
	e.HandleTrade(ctx, trade("b1", "buy", 1000, 1000, *now))
	e.HandleTrade(ctx, trade("b2", "buy", 1000, 2000, *now)) // same round: not tried again
	e.Wait()
	rows := st.all()
	if len(rows) != 1 || rows[0].Status != model.NonsolSkipped || rows[0].SkipReason != "TOKEN_NOT_TRADABLE" {
		t.Fatalf("want one skip, got %+v", rows)
	}
	// They exit; the next buy is a new try.
	e.HandleTrade(ctx, trade("s1", "sell", 2000, 0, *now))
	q.buy = func(l uint64) (Quote, error) { return Quote{OutAmount: l}, nil }
	e.HandleTrade(ctx, trade("b3", "buy", 1000, 1000, *now))
	e.Wait()
	if n := len(st.all()); n != 2 || e.OpenCount() != 1 {
		t.Fatalf("want skip + open, got %d rows, %d open", n, e.OpenCount())
	}
}

func TestSellFailureOwesAndRetriesOnMark(t *testing.T) {
	e, q, st, now := setup(t)
	ctx := context.Background()
	e.HandleTrade(ctx, trade("b1", "buy", 1000, 1000, *now))
	e.Wait()
	q.sell = func(uint64) (Quote, error) { return Quote{}, &NoRouteError{Code: "NO_ROUTES_FOUND"} }
	e.HandleTrade(ctx, trade("s1", "sell", 1000, 0, *now))
	e.Wait()
	c := st.all()[0]
	if c.Status != model.NonsolOpen || c.PendingSell != 1e11 || c.LastError == "" {
		t.Fatalf("want owed sell, got %+v", c)
	}
	q.sell = func(tk uint64) (Quote, error) { return Quote{OutAmount: tk / 1000}, nil }
	e.MarkOpen(ctx)
	e.Wait()
	c = st.all()[0]
	if c.Status != model.NonsolClosed || c.SolOut != 0.1 {
		t.Fatalf("retry did not close: %+v", c)
	}
}

func TestMarkOpen(t *testing.T) {
	e, _, st, now := setup(t)
	ctx := context.Background()
	e.HandleTrade(ctx, trade("b1", "buy", 1000, 1000, *now))
	e.Wait()
	e.MarkOpen(ctx)
	e.Wait()
	c := st.all()[0]
	if c.MarkSol != 0.2 || c.MarkedAt == nil || c.Status != model.NonsolOpen {
		t.Fatalf("bad mark: %+v", c)
	}
}

func TestFilters(t *testing.T) {
	e, q, st, now := setup(t)
	ctx := context.Background()
	// Not tracked, wrong pool, not target, too old: none copied.
	other := trade("x1", "buy", 1000, 1000, *now)
	other.Trader = "BENCH"
	sol := trade("x2", "buy", 1000, 1000, *now)
	sol.Pool = "pump"
	nt := trade("x3", "buy", 1000, 1000, *now)
	nt.IsTarget = false
	old := trade("x4", "buy", 1000, 1000, now.Add(-time.Minute))
	for _, tr := range []model.Trade{other, sol, nt, old} {
		e.HandleTrade(ctx, tr)
	}
	// Sell with no copy: ignored.
	e.HandleTrade(ctx, trade("x5", "sell", 1000, 0, *now))
	e.Wait()
	if len(st.all()) != 0 || len(q.calls) != 0 {
		t.Fatalf("want nothing, got %v rows, calls %v", st.all(), q.calls)
	}
	// KEEP_WALLETS are copied too.
	e.Keep["BENCH"] = true
	e.HandleTrade(ctx, other)
	e.Wait()
	if len(st.all()) != 1 {
		t.Fatal("keep wallet not copied")
	}
}

func TestSellWaitsForPendingBuy(t *testing.T) {
	e, _, st, now := setup(t)
	ctx := context.Background()
	release := make(chan struct{})
	slept := 0
	var mu sync.Mutex
	e.Sleep = func(context.Context, time.Duration) {
		mu.Lock()
		slept++
		first := slept == 1
		mu.Unlock()
		if first {
			<-release // the buy is still waiting for its 3s
		}
	}
	e.HandleTrade(ctx, trade("b1", "buy", 1000, 1000, *now))
	e.HandleTrade(ctx, trade("s1", "sell", 1000, 0, *now))
	close(release)
	e.Wait()
	rows := st.all()
	if len(rows) != 1 || rows[0].Status != model.NonsolClosed {
		t.Fatalf("sell should follow the pending buy: %+v", rows)
	}
}

func TestLoadOpenFollowsSellWhateverStatus(t *testing.T) {
	e, _, st, now := setup(t)
	ctx := context.Background()
	st.rows["c1"] = model.NonsolCopy{ID: "c1", Wallet: "DEMOTED", Mint: mint, Status: model.NonsolOpen,
		TokensBought: 1000, TokensHeld: 1000, SolIn: 0.1}
	if err := e.LoadOpen(ctx); err != nil {
		t.Fatal(err)
	}
	s := trade("s1", "sell", 10, 0, *now)
	s.Trader, s.IsTarget = "DEMOTED", false
	e.HandleTrade(ctx, s)
	e.Wait()
	if c := st.rows["c1"]; c.Status != model.NonsolClosed {
		t.Fatalf("want closed, got %+v", c)
	}
}
