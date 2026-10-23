package rotation

import (
	"context"
	"strings"
	"testing"
	"time"

	"copytrade/internal/model"
	"copytrade/internal/store"
)

var now = time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)

var rules = Rules{DelayS: 2, LiveMax: 2, BenchMax: 3, MaxDrawdown: 0.25, PromoteMinDays: 14, PromoteMinTrades: 10,
	DropInactiveDays: 7, DropBadPnlUSD: -50, DropBadMinTrades: 15}

func good(w string, score float64) []model.Score {
	return []model.Score{
		{Wallet: w, DelayS: 2, WindowDays: 14, Positions: 20, PnlUSD: 80, PnlExBestUSD: 50, Score: score, MaxDrawdown: 0.05, ActiveDays: 14},
		{Wallet: w, DelayS: 2, WindowDays: 30, Positions: 30, PnlUSD: 90, PnlExBestUSD: 60, Score: score, MaxDrawdown: 0.05, ActiveDays: 20},
	}
}

func wallet(a, status string) model.Wallet {
	last := now.Add(-time.Hour)
	return model.Wallet{Address: a, Status: status, AddedAt: now.AddDate(0, 0, -30), LastTradeAt: &last}
}

func TestDemote(t *testing.T) {
	cases := map[string]model.Score{
		"pnl":   {Positions: 5, PnlUSD: -1, Score: 1},
		"dd":    {Positions: 5, PnlUSD: 10, Score: 1, MaxDrawdown: 0.25},
		"score": {Positions: 5, PnlUSD: 10, Score: -0.5},
	}
	for name, s := range cases {
		if rules.DemoteReason(s) == "" {
			t.Errorf("%s: expected demotion", name)
		}
	}
	if r := rules.DemoteReason(model.Score{Positions: 5, PnlUSD: 10, Score: 2, MaxDrawdown: 0.1}); r != "" {
		t.Errorf("healthy wallet demoted: %s", r)
	}
	if r := rules.DemoteReason(model.Score{}); r != "" {
		t.Errorf("no data should not demote: %s", r)
	}
}

func TestPromoteBlocks(t *testing.T) {
	g := good("A", 5)
	if b := rules.PromoteBlock(g[0], g[1]); b != "" {
		t.Fatalf("good wallet blocked: %s", b)
	}
	lucky := g[0]
	lucky.PnlExBestUSD, lucky.LuckyTrade = -10, true
	if b := rules.PromoteBlock(lucky, g[1]); !strings.Contains(b, "one trade") {
		t.Fatalf("lucky not blocked: %q", b)
	}
	young := g[1]
	young.ActiveDays = 9
	if b := rules.PromoteBlock(g[0], young); !strings.Contains(b, "days of history") {
		t.Fatalf("young not blocked: %q", b)
	}
	few := g[0]
	few.Positions = 4
	if b := rules.PromoteBlock(few, g[1]); !strings.Contains(b, "trades") {
		t.Fatalf("few trades not blocked: %q", b)
	}
	neg30 := g[1]
	neg30.PnlUSD = -1
	if b := rules.PromoteBlock(g[0], neg30); b == "" {
		t.Fatal("negative 30d should block")
	}
}

func TestDecideFillsSlotsByScore(t *testing.T) {
	ws := []model.Wallet{wallet("L1", model.StatusLive), wallet("B1", model.StatusBench), wallet("B2", model.StatusBench), wallet("B3", model.StatusBench)}
	var ss []model.Score
	ss = append(ss, good("L1", 3)...)
	ss = append(ss, good("B1", 1)...)
	ss = append(ss, good("B2", 9)...)
	ss = append(ss, good("B3", 4)...)
	ch := rules.Decide(ws, Index(ss))
	if len(ch) != 1 || ch[0].Wallet != "B2" || ch[0].To != model.StatusLive {
		t.Fatalf("expected only B2 promoted into the one free slot: %+v", ch)
	}
	// L1 goes negative: demoted, and two slots open
	ss[0].PnlUSD = -5
	ch = rules.Decide(ws, Index(ss))
	got := map[string]string{}
	for _, c := range ch {
		got[c.Wallet] = c.To
	}
	if got["L1"] != model.StatusBench || got["B2"] != model.StatusLive || got["B3"] != model.StatusLive || len(got) != 3 {
		t.Fatalf("unexpected changes %v", got)
	}
}

func TestPrune(t *testing.T) {
	idle := wallet("IDLE", model.StatusBench)
	old := now.AddDate(0, 0, -8)
	idle.LastTradeAt = &old
	never := model.Wallet{Address: "NEW", Status: model.StatusBench, AddedAt: now.AddDate(0, 0, -1)}
	bad := wallet("BAD", model.StatusBench)
	live := wallet("LIVE", model.StatusLive)
	live.LastTradeAt = &old // live wallets are never pruned
	ss := []model.Score{{Wallet: "BAD", DelayS: 2, WindowDays: 30, Positions: 20, PnlUSD: -60}}
	ch := rules.Prune([]model.Wallet{idle, never, bad, live}, Index(ss), now)
	got := map[string]bool{}
	for _, c := range ch {
		got[c.Wallet] = true
	}
	if !got["IDLE"] || !got["BAD"] || got["NEW"] || got["LIVE"] {
		t.Fatalf("prune result %v", got)
	}
}

func TestPruneBenchCap(t *testing.T) {
	var ws []model.Wallet
	var ss []model.Score
	for i, a := range []string{"A", "B", "C", "D", "E"} {
		ws = append(ws, wallet(a, model.StatusBench))
		ss = append(ss, model.Score{Wallet: a, DelayS: 2, WindowDays: 14, Score: float64(i)})
	}
	ch := rules.Prune(ws, Index(ss), now)
	got := map[string]bool{}
	for _, c := range ch {
		got[c.Wallet] = true
	}
	if len(ch) != 2 || !got["A"] || !got["B"] {
		t.Fatalf("expected the 2 lowest scores (A, B) dropped: %+v", ch)
	}
}

func TestApplyLogsRotations(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	st.AddWallet(ctx, wallet("A", model.StatusBench))
	done, err := Apply(ctx, st, []Change{{Wallet: "A", From: "bench", To: "live", Reason: "r"}, {Wallet: "A", From: "bench", To: "live", Reason: "dup"}}, now)
	if err != nil || len(done) != 1 {
		t.Fatalf("apply %v %v", done, err)
	}
	rs, _ := st.ListRotations(ctx, 10)
	if len(rs) != 1 || rs[0].ToStatus != "live" || rs[0].Reason != "r" {
		t.Fatalf("rotations %+v", rs)
	}
}
