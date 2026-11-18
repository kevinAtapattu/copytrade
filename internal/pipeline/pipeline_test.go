package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"copytrade/internal/api"
	"copytrade/internal/bus"
	"copytrade/internal/config"
	"copytrade/internal/fixture"
	"copytrade/internal/model"
	"copytrade/internal/rotation"
	"copytrade/internal/source"
	"copytrade/internal/store"
)

func testConfig() config.Config {
	os.Clearenv() // defaults only, never a developer's .env
	os.Setenv("NO_DOTENV", "1")
	c := config.Load()
	c.SolUSDFixed = 150
	return c
}

func replay(t *testing.T) (*store.Memory, fixture.Data, Report, config.Config) {
	return replayMode(t, "stream")
}

func replayMode(t *testing.T, mode string) (*store.Memory, fixture.Data, Report, config.Config) {
	t.Helper()
	c := testConfig()
	data := fixture.Generate(42, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 21)
	var lines bytes.Buffer
	if err := data.Write(&lines, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	src, err := source.NewReplay(&lines)
	if err != nil {
		t.Fatal(err)
	}
	st := store.NewMemory()
	ctx := context.Background()
	for _, w := range data.Meta.Wallets {
		st.AddWallet(ctx, w)
	}
	rep, err := Run(ctx, c, st, src, Options{SolUSD: 150, Mode: mode})
	if err != nil {
		t.Fatal(err)
	}
	return st, data, rep, c
}

func TestEndToEndReplay(t *testing.T) {
	st, data, rep, c := replay(t)
	ctx := context.Background()
	roles := data.Meta.Roles

	if rep.TargetTrades == 0 || rep.SimTrades != rep.TargetTrades*len(model.Delays) {
		t.Fatalf("sim fills %d, want 3 x %d target trades", rep.SimTrades, rep.TargetTrades)
	}
	if rep.Delivered >= int64(rep.Messages) {
		t.Fatal("replay should filter unsubscribed trades like the real stream")
	}

	status := map[string]model.Wallet{}
	ws, _ := st.ListWallets(ctx)
	for _, w := range ws {
		status[w.Address] = w
	}
	expect := func(addr, want, name string) {
		t.Helper()
		if got := status[addr].Status; got != want {
			t.Errorf("%s: status %q, want %q (reason %q)", name, got, want, status[addr].Reason)
		}
	}
	expect(roles.Good, model.StatusLive, "good wallet promoted")
	expect(roles.Idle, model.StatusDropped, "idle wallet auto-dropped")
	expect(roles.Early1, model.StatusBench, "early buyer 1 discovered")
	expect(roles.Early2, model.StatusBench, "early buyer 2 discovered")
	if status[roles.Early1].Source != model.SourceDiscovery || !strings.Contains(status[roles.Early1].Reason, "5x") {
		t.Errorf("discovery reason missing: %+v", status[roles.Early1])
	}
	if status[roles.Lucky].Status == model.StatusLive {
		t.Error("lucky wallet must never be promoted")
	}
	if status[roles.Fast].Status == model.StatusLive {
		t.Error("fast flipper is not copyable at +2s and must not be promoted")
	}

	rots, _ := st.ListRotations(ctx, 0)
	var drawDemoted, goodPromoted time.Time
	for _, r := range rots {
		if r.Wallet == roles.Draw && r.FromStatus == model.StatusLive && r.ToStatus == model.StatusBench {
			drawDemoted = r.At
		}
		if r.Wallet == roles.Good && r.ToStatus == model.StatusLive {
			goodPromoted = r.At
		}
	}
	if drawDemoted.IsZero() {
		t.Error("drawdown wallet was never demoted from live")
	}
	if goodPromoted.Before(data.Meta.Start.AddDate(0, 0, 14)) {
		t.Errorf("good wallet promoted at %v, before 14 days of history", goodPromoted)
	}

	// The lucky wallet is blocked specifically by the one-trade rule.
	scores, _ := st.LatestScores(ctx)
	ix := rotation.Index(scores)
	s14, _ := ix.Get(roles.Lucky, 2, 14)
	s30, _ := ix.Get(roles.Lucky, 2, 30)
	if !s14.LuckyTrade || s14.PnlUSD <= 0 {
		t.Errorf("lucky wallet score should be positive only via one trade: %+v", s14)
	}
	if b := rotation.FromConfig(c).PromoteBlock(s14, s30); !strings.Contains(b, "one trade") {
		t.Errorf("lucky promotion block = %q", b)
	}
	// Delay matters: the fast flipper's +2s copy loses.
	if f, _ := ix.Get(roles.Fast, 2, 30); f.PnlUSD >= 0 {
		t.Errorf("fast flipper +2s pnl %v should be negative", f.PnlUSD)
	}
	if g, _ := ix.Get(roles.Good, 2, 14); g.PnlUSD <= 0 || g.Score <= 0 {
		t.Errorf("good wallet should score positive: %+v", g)
	}

	// Executor (paper) copied the draw wallet while it was live and the good wallet after promotion.
	fills, _ := st.ListLiveFills(ctx, store.LiveFilter{})
	var goodFills, drawAfterDemotion int
	for _, f := range fills {
		if f.Wallet == roles.Good && f.Status == "filled" {
			goodFills++
			if f.TargetAt.Before(goodPromoted) {
				t.Errorf("copied good wallet before it was live: %v", f.TargetAt)
			}
		}
		if f.Wallet == roles.Draw && f.Side == model.SideBuy && f.TargetAt.After(drawDemoted.Add(time.Minute)) {
			drawAfterDemotion++
		}
	}
	if goodFills == 0 {
		t.Error("executor never copied the promoted wallet")
	}
	if drawAfterDemotion > 0 {
		t.Errorf("executor copied %d buys after demotion", drawAfterDemotion)
	}
}

func TestAPIOnReplayData(t *testing.T) {
	st, data, _, c := replay(t)
	s := api.New(st, bus.NewMemory(), nil, c)
	// The replay covers Sep 1-22; pages look back 30-45 days from "now".
	s.Now = func() time.Time { return time.Date(2026, 9, 22, 0, 0, 0, 0, time.UTC) }
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	get := func(path string, out any) {
		t.Helper()
		res, err := http.Get(srv.URL + path)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("GET %s: %v %v", path, err, res.Status)
		}
		defer res.Body.Close()
		if err := json.NewDecoder(res.Body).Decode(out); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
	}
	var ov struct {
		Equity []struct{ Cum float64 } `json:"equity"`
		Pnl    map[string]float64      `json:"pnl"`
	}
	get("/api/overview?delay=2&scope=all", &ov)
	if len(ov.Equity) == 0 {
		t.Error("overview equity empty")
	}
	var wallets []map[string]any
	get("/api/wallets", &wallets)
	if len(wallets) < 4 {
		t.Errorf("wallets %d", len(wallets))
	}
	var detail struct {
		Trades []struct {
			Sims map[string]model.SimTrade `json:"sims"`
		} `json:"trades"`
	}
	get("/api/wallets/"+data.Meta.Roles.Good, &detail)
	if len(detail.Trades) == 0 || len(detail.Trades[0].Sims) != 3 {
		t.Errorf("wallet detail should pair each trade with 3 sim fills")
	}
	var disc struct{ Hits []any }
	get("/api/discovery", &disc)
	if len(disc.Hits) == 0 {
		t.Error("discovery hits empty")
	}
	var ex struct {
		Fills      []model.LiveFill `json:"fills"`
		GapSamples int              `json:"gap_samples"`
	}
	get("/api/executor", &ex)
	if len(ex.Fills) == 0 || ex.GapSamples == 0 {
		t.Errorf("executor fills %d gap samples %d", len(ex.Fills), ex.GapSamples)
	}
	var liveView struct {
		Fills []model.LiveFill `json:"fills"`
		View  string           `json:"view"`
		Mode  string           `json:"mode"`
	}
	get("/api/executor?mode=live", &liveView)
	if liveView.View != "live" || liveView.Mode != "paper" || len(liveView.Fills) != 0 {
		t.Errorf("live view must exclude paper fills: view=%s mode=%s fills=%d", liveView.View, liveView.Mode, len(liveView.Fills))
	}
	var ovLive struct {
		ExecEquity []any  `json:"exec_equity"`
		ExecMode   string `json:"exec_mode"`
	}
	get("/api/overview?scope=all&mode=live", &ovLive)
	if ovLive.ExecMode != "live" || len(ovLive.ExecEquity) != 0 {
		t.Errorf("overview live view mixed in paper results: %+v", ovLive)
	}
	var h map[string]any
	get("/api/health", &h)

	// add / remove / kill switch
	post := func(path, body string) int {
		res, err := http.Post(srv.URL+path, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post("/api/wallets", `{"address":"not-valid"}`); code != 400 {
		t.Errorf("invalid address accepted: %d", code)
	}
	if code := post("/api/wallets", `{"address":"7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU","label":"t"}`); code != 200 {
		t.Errorf("add wallet: %d", code)
	}
	req, _ := http.NewRequest(http.MethodDelete, srv.URL+"/api/wallets/7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU", nil)
	if res, err := http.DefaultClient.Do(req); err != nil || res.StatusCode != 200 {
		t.Errorf("remove wallet failed")
	}
	if code := post("/api/executor/kill", `{"on":true}`); code != 200 {
		t.Errorf("kill: %d", code)
	}
	if v, _, _ := st.GetSetting(context.Background(), store.SettingKillSwitch); v != "on" {
		t.Error("kill switch not stored")
	}
}

func TestCommittedFixtureReplays(t *testing.T) {
	f, err := os.Open("../../fixtures/replay.jsonl")
	if err != nil {
		t.Skip("fixture file not present")
	}
	defer f.Close()
	src, err := source.NewReplay(f)
	if err != nil || src.Len() == 0 {
		t.Fatalf("fixture unreadable: %v", err)
	}
}

func TestSnapshotModeEndToEnd(t *testing.T) {
	st, data, rep, _ := replayMode(t, "snapshot")
	ctx := context.Background()
	roles := data.Meta.Roles
	_, _, full, _ := replay(t)

	// Only tracked wallets' own trades are metered: far fewer messages than stream mode.
	if rep.Delivered != int64(rep.TargetTrades) || rep.Delivered*5 > full.Delivered {
		t.Fatalf("snapshot mode delivered %d messages (targets %d), stream mode %d", rep.Delivered, rep.TargetTrades, full.Delivered)
	}
	if rep.SimTrades != rep.TargetTrades*len(model.Delays) {
		t.Fatalf("sim fills %d, want 3 x %d", rep.SimTrades, rep.TargetTrades)
	}
	sims, _ := st.ListSimTrades(ctx, store.SimFilter{})
	filled := 0
	for _, s := range sims {
		if !s.Skipped {
			filled++
		}
	}
	if filled == 0 {
		t.Fatal("no simulated fills priced from snapshots")
	}
	w, _, _ := st.GetWallet(ctx, roles.Good)
	if w.Status != model.StatusLive {
		t.Errorf("good wallet should be promoted in snapshot mode too: %s (%s)", w.Status, w.Reason)
	}
	scores, _ := st.LatestScores(ctx)
	ix := rotation.Index(scores)
	if f, _ := ix.Get(roles.Fast, 2, 30); f.PnlUSD >= 0 {
		t.Errorf("fast flipper should lose at +2s in snapshot mode: %v", f.PnlUSD)
	}
	fills, _ := st.ListLiveFills(ctx, store.LiveFilter{})
	if len(fills) == 0 {
		t.Error("executor did not trade in snapshot mode")
	}
	// Discovery works from fetched token history instead of the stream.
	for _, e := range []string{roles.Early1, roles.Early2} {
		if w, ok, _ := st.GetWallet(ctx, e); !ok || w.Source != model.SourceDiscovery {
			t.Errorf("early buyer %s not discovered in snapshot mode", e[:6])
		}
	}
}
