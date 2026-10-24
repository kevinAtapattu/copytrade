// Package api serves REST + SSE for the dashboard.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/compare"
	"copytrade/internal/config"
	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/score"
	"copytrade/internal/seed"
	"copytrade/internal/sim"
	"copytrade/internal/store"
)

// Lagger reports consumer group lag (Redis); optional.
type Lagger interface {
	Lag(ctx context.Context, stream, group string) (int64, error)
}

type Server struct {
	Store store.Store
	Bus   bus.Bus
	Lag   Lagger
	Cfg   config.Config
	Now   func() time.Time
	hub   *hub
}

func New(st store.Store, b bus.Bus, lag Lagger, c config.Config) *Server {
	return &Server{Store: st, Bus: b, Lag: lag, Cfg: c, Now: time.Now, hub: newHub()}
}

// RunHub tails the events stream and fans out to SSE clients. Blocks.
func (s *Server) RunHub(ctx context.Context) error {
	return s.Bus.Subscribe(ctx, bus.StreamEvents, "", "api", func(_ context.Context, b []byte) error {
		s.hub.broadcast(b)
		return nil
	})
}

func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/overview", s.overview)
	m.HandleFunc("GET /api/wallets", s.wallets)
	m.HandleFunc("POST /api/wallets", s.addWallet)
	m.HandleFunc("GET /api/wallets/{addr}", s.walletDetail)
	m.HandleFunc("DELETE /api/wallets/{addr}", s.removeWallet)
	m.HandleFunc("POST /api/wallets/{addr}/status", s.setStatus)
	m.HandleFunc("GET /api/trades", s.trades)
	m.HandleFunc("GET /api/discovery", s.discovery)
	m.HandleFunc("GET /api/rotations", s.rotations)
	m.HandleFunc("GET /api/health", s.health)
	m.HandleFunc("GET /api/executor", s.executor)
	m.HandleFunc("POST /api/executor/kill", s.kill)
	m.HandleFunc("GET /api/stream", s.stream)
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	return cors(m)
}

func cors(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func qInt(r *http.Request, k string, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get(k)); err == nil {
		return n
	}
	return def
}

// ---- overview

type point struct {
	Date string  `json:"date"`
	Pnl  float64 `json:"pnl"`
	Cum  float64 `json:"cum"`
}

type openPos struct {
	model.SimPosition
	MarkPnlUSD float64 `json:"mark_pnl_usd"`
	LastPrice  float64 `json:"last_price"`
}

func (s *Server) overview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.Now()
	delay := qInt(r, "delay", s.Cfg.RotationDelayS)
	scope := r.URL.Query().Get("scope")
	wallets, err := s.Store.ListWallets(ctx)
	if err != nil {
		fail(w, 500, err)
		return
	}
	inScope := map[string]bool{}
	counts := map[string]int{}
	for _, wl := range wallets {
		counts[wl.Status]++
		if wl.Status == model.StatusLive || (scope == "all" && wl.Status != model.StatusDropped) {
			inScope[wl.Address] = true
		}
	}
	if scope == "" && counts[model.StatusLive] == 0 {
		scope = "all" // nothing live yet: show the whole bench so the page is useful
		for _, wl := range wallets {
			if wl.Status != model.StatusDropped {
				inScope[wl.Address] = true
			}
		}
	}
	ps, err := s.Store.ListSimPositions(ctx, store.SimFilter{DelayS: delay, Since: now.AddDate(0, 0, -90)})
	if err != nil {
		fail(w, 500, err)
		return
	}
	fees := sim.FeesFrom(s.Cfg)
	toks := s.tokenCache(ctx)
	var outs []score.Outcome
	var open []openPos
	for _, p := range ps {
		if !inScope[p.Wallet] {
			continue
		}
		tok := toks(p.Mint)
		o := score.Value(p, tok, fees, now)
		outs = append(outs, o)
		if p.Status == model.PosOpen {
			op := openPos{SimPosition: p, MarkPnlUSD: o.PnlUSD}
			if tok != nil {
				op.LastPrice = tok.LastPrice
			}
			open = append(open, op)
		}
	}
	day := now.Truncate(24 * time.Hour)
	sum := func(since time.Time) float64 {
		t := 0.0
		for _, o := range outs {
			if !o.At.Before(since) {
				t += o.PnlUSD
			}
		}
		return t
	}
	execMode := s.viewMode(r)
	live, _ := s.Store.ListLivePositions(ctx, store.LiveFilter{Mode: execMode, Since: now.AddDate(0, 0, -90)})
	var execOuts []score.Outcome
	for _, p := range live {
		execOuts = append(execOuts, valueLive(p, toks(p.Mint), fees, now))
	}
	writeJSON(w, 200, map[string]any{
		"delay": delay, "scope": scope, "wallet_counts": counts, "bankroll_usd": s.Cfg.BankrollUSD,
		"pnl":            map[string]float64{"day": sum(day), "week": sum(now.AddDate(0, 0, -7)), "month": sum(now.AddDate(0, -1, 0)), "all": sum(time.Time{})},
		"equity":         dailySeries(outs),
		"exec_equity":    dailySeries(execOuts),
		"open_positions": open,
		"exec_open":      filterOpen(live),
		"exec_mode":      execMode,
		"positions":      len(outs),
	})
}

func valueLive(p model.LivePosition, tok *model.Token, fees curve.Fees, now time.Time) score.Outcome {
	sp := model.SimPosition{Status: p.Status, ClosedAt: p.ClosedAt, OpenedAt: p.OpenedAt, CostSol: p.CostSol,
		ProceedsSol: p.ProceedsSol, TokensHeld: p.TokensHeld, SolUSD: p.SolUSD}
	return score.Value(sp, tok, fees, now)
}

func filterOpen(ps []model.LivePosition) []model.LivePosition {
	var out []model.LivePosition
	for _, p := range ps {
		if p.Status == model.PosOpen {
			out = append(out, p)
		}
	}
	return out
}

func dailySeries(outs []score.Outcome) []point {
	by := map[string]float64{}
	for _, o := range outs {
		by[o.At.UTC().Format("2006-01-02")] += o.PnlUSD
	}
	days := make([]string, 0, len(by))
	for d := range by {
		days = append(days, d)
	}
	sort.Strings(days)
	pts := make([]point, 0, len(days))
	cum := 0.0
	for _, d := range days {
		cum += by[d]
		pts = append(pts, point{Date: d, Pnl: round2(by[d]), Cum: round2(cum)})
	}
	return pts
}

func round2(f float64) float64 { return float64(int64(f*100+copysign(0.5, f))) / 100 }

func copysign(a, b float64) float64 {
	if b < 0 {
		return -a
	}
	return a
}

func (s *Server) tokenCache(ctx context.Context) func(string) *model.Token {
	cache := map[string]*model.Token{}
	return func(mint string) *model.Token {
		if t, ok := cache[mint]; ok {
			return t
		}
		t, ok, _ := s.Store.GetToken(ctx, mint)
		if !ok {
			cache[mint] = nil
			return nil
		}
		cache[mint] = &t
		return &t
	}
}

// ---- wallets

type walletRow struct {
	model.Wallet
	// scores[delay][window]
	Scores map[string]map[string]model.Score `json:"scores"`
	Open   int                               `json:"open_positions"`
	// Them vs us over the last 14 days (see compare.Build).
	Compare compare.Summary `json:"compare"`
	// Their trades seen in the last 24h (activity).
	Trades24h int `json:"trades_24h"`
}

func (s *Server) wallets(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ws, err := s.Store.ListWallets(ctx)
	if err != nil {
		fail(w, 500, err)
		return
	}
	scores, err := s.Store.LatestScores(ctx)
	if err != nil {
		fail(w, 500, err)
		return
	}
	open, _ := s.Store.ListSimPositions(ctx, store.SimFilter{OpenOnly: true, DelayS: s.Cfg.RotationDelayS})
	openBy := map[string]int{}
	for _, p := range open {
		openBy[p.Wallet]++
	}
	rows := map[string]*walletRow{}
	var out []*walletRow
	for _, wl := range ws {
		if wl.Status == model.StatusDropped && r.URL.Query().Get("dropped") != "1" {
			continue
		}
		row := &walletRow{Wallet: wl, Scores: map[string]map[string]model.Score{}, Open: openBy[wl.Address]}
		rows[wl.Address] = row
		out = append(out, row)
	}
	now := s.Now()
	since := now.AddDate(0, 0, -14)
	tts, _ := s.Store.ListTargetTrades(ctx, store.TradeFilter{Since: since})
	sps, _ := s.Store.ListSimPositions(ctx, store.SimFilter{Since: since})
	ttBy, psBy := map[string][]model.Trade{}, map[string][]model.SimPosition{}
	dayAgo := now.Add(-24 * time.Hour)
	for _, t := range tts {
		ttBy[t.Trader] = append(ttBy[t.Trader], t)
		if row := rows[t.Trader]; row != nil && !t.ReceivedAt.Before(dayAgo) {
			row.Trades24h++
		}
	}
	for _, p := range sps {
		psBy[p.Wallet] = append(psBy[p.Wallet], p)
	}
	fees := sim.FeesFrom(s.Cfg)
	toks := s.tokenCache(ctx)
	for addr, row := range rows {
		_, row.Compare = compare.Build(ttBy[addr], psBy[addr], toks, fees, now)
	}
	for _, sc := range scores {
		row := rows[sc.Wallet]
		if row == nil {
			continue
		}
		d := strconv.Itoa(sc.DelayS)
		if row.Scores[d] == nil {
			row.Scores[d] = map[string]model.Score{}
		}
		row.Scores[d][strconv.Itoa(sc.WindowDays)] = sc
	}
	writeJSON(w, 200, out)
}

func (s *Server) addWallet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Address string `json:"address"`
		Label   string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, 400, err)
		return
	}
	body.Address = strings.TrimSpace(body.Address)
	if !seed.ValidAddress(body.Address) {
		fail(w, 400, errors.New("not a valid Solana address"))
		return
	}
	now := s.Now()
	added, err := s.Store.AddWallet(r.Context(), model.Wallet{Address: body.Address, Label: body.Label,
		Status: model.StatusBench, Source: model.SourceManual, Reason: "added from dashboard", AddedAt: now, StatusAt: now})
	if err != nil {
		fail(w, 500, err)
		return
	}
	if !added { // re-adding a dropped wallet puts it back on the bench
		if _, err := s.Store.SetWalletStatus(r.Context(), body.Address, model.StatusBench, "re-added from dashboard", now); err != nil {
			fail(w, 500, err)
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "new": added})
}

func (s *Server) removeWallet(w http.ResponseWriter, r *http.Request) {
	addr := r.PathValue("addr")
	ok, err := s.Store.SetWalletStatus(r.Context(), addr, model.StatusDropped, "removed from dashboard", s.Now())
	if err != nil {
		fail(w, 500, err)
		return
	}
	s.emitRotation(r.Context(), addr, model.StatusDropped, "removed from dashboard", ok)
	writeJSON(w, 200, map[string]any{"ok": true, "changed": ok})
}

func (s *Server) setStatus(w http.ResponseWriter, r *http.Request) {
	addr := r.PathValue("addr")
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, 400, err)
		return
	}
	switch body.Status {
	case model.StatusLive, model.StatusBench, model.StatusDropped, model.StatusPaused:
	default:
		fail(w, 400, fmt.Errorf("bad status %q", body.Status))
		return
	}
	reason := "manual: set to " + body.Status + " from dashboard"
	ok, err := s.Store.SetWalletStatus(r.Context(), addr, body.Status, reason, s.Now())
	if err != nil {
		fail(w, 500, err)
		return
	}
	s.emitRotation(r.Context(), addr, body.Status, reason, ok)
	writeJSON(w, 200, map[string]any{"ok": true, "changed": ok})
}

func (s *Server) emitRotation(ctx context.Context, addr, to, reason string, changed bool) {
	if !changed {
		return
	}
	_ = bus.PublishJSON(ctx, s.Bus, bus.StreamEvents, model.Event{Kind: "rotation", At: s.Now(), Service: "api",
		Message: addr + " -> " + to + ": " + reason, Data: map[string]string{"wallet": addr, "to": to, "reason": reason}})
}

type targetRow struct {
	model.Trade
	Price float64                    `json:"price"`
	Sims  map[string]*model.SimTrade `json:"sims"` // by delay
}

func (s *Server) walletDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	addr := r.PathValue("addr")
	wl, ok, err := s.Store.GetWallet(ctx, addr)
	if err != nil {
		fail(w, 500, err)
		return
	}
	if !ok {
		fail(w, 404, errors.New("wallet not found"))
		return
	}
	now := s.Now()
	trades, err := s.Store.ListTargetTrades(ctx, store.TradeFilter{Wallet: addr, Limit: qInt(r, "limit", 200)})
	if err != nil {
		fail(w, 500, err)
		return
	}
	sims, err := s.Store.ListSimTrades(ctx, store.SimFilter{Wallet: addr, Since: now.AddDate(0, 0, -45)})
	if err != nil {
		fail(w, 500, err)
		return
	}
	simBy := map[string]map[string]*model.SimTrade{}
	for i := range sims {
		x := &sims[i]
		if simBy[x.TargetSig] == nil {
			simBy[x.TargetSig] = map[string]*model.SimTrade{}
		}
		simBy[x.TargetSig][strconv.Itoa(x.DelayS)] = x
	}
	rows := make([]targetRow, 0, len(trades))
	for _, t := range trades {
		sm := simBy[t.Signature]
		if sm == nil {
			sm = map[string]*model.SimTrade{}
		}
		rows = append(rows, targetRow{Trade: t, Price: t.Price(), Sims: sm})
	}
	scores, _ := s.Store.LatestScores(ctx)
	var mine []model.Score
	for _, sc := range scores {
		if sc.Wallet == addr {
			mine = append(mine, sc)
		}
	}
	history := map[string][]model.Score{}
	for _, d := range model.Delays {
		h, _ := s.Store.ScoreHistory(ctx, addr, d, 14, now.AddDate(0, 0, -30))
		history[strconv.Itoa(d)] = thin(h, 200)
	}
	ps, _ := s.Store.ListSimPositions(ctx, store.SimFilter{Wallet: addr, Since: now.AddDate(0, 0, -30)})
	fees := sim.FeesFrom(s.Cfg)
	toks := s.tokenCache(ctx)
	curves := map[string][]point{}
	for _, d := range model.Delays {
		var outs []score.Outcome
		for _, p := range ps {
			if p.DelayS == d {
				outs = append(outs, score.Value(p, toks(p.Mint), fees, now))
			}
		}
		curves[strconv.Itoa(d)] = dailySeries(outs)
	}
	rots, _ := s.Store.ListRotations(ctx, 500)
	var myRots []model.Rotation
	for _, x := range rots {
		if x.Wallet == addr {
			myRots = append(myRots, x)
		}
	}
	cmpRows, cmpSum := compare.Build(trades, ps, toks, fees, now)
	writeJSON(w, 200, map[string]any{"wallet": wl, "scores": mine, "score_history": history, "equity": curves,
		"trades": rows, "positions": ps, "rotations": myRots, "compare": cmpRows, "compare_summary": cmpSum})
}

func thin(s []model.Score, n int) []model.Score {
	if len(s) <= n {
		return s
	}
	step := float64(len(s)) / float64(n)
	out := make([]model.Score, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, s[int(float64(i)*step)])
	}
	return append(out, s[len(s)-1])
}

// ---- trade log, discovery, rotations

func (s *Server) trades(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lim := qInt(r, "limit", 200)
	wallet := r.URL.Query().Get("wallet")
	tt, err := s.Store.ListTargetTrades(ctx, store.TradeFilter{Wallet: wallet, Limit: lim})
	if err != nil {
		fail(w, 500, err)
		return
	}
	st, err := s.Store.ListSimTrades(ctx, store.SimFilter{Wallet: wallet, DelayS: qInt(r, "delay", 0), Limit: lim})
	if err != nil {
		fail(w, 500, err)
		return
	}
	lf, err := s.Store.ListLiveFills(ctx, store.LiveFilter{Wallet: wallet, Limit: lim})
	if err != nil {
		fail(w, 500, err)
		return
	}
	s.attachSim(ctx, lf)
	writeJSON(w, 200, map[string]any{"target_trades": tt, "sim_trades": st, "live_fills": lf})
}

func (s *Server) discovery(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	hits, err := s.Store.ListDiscoveryHits(ctx, 500)
	if err != nil {
		fail(w, 500, err)
		return
	}
	ws, _ := s.Store.ListWallets(ctx)
	var cands []model.Wallet
	status := map[string]string{}
	for _, wl := range ws {
		status[wl.Address] = wl.Status
		if wl.Source == model.SourceDiscovery {
			cands = append(cands, wl)
		}
	}
	type hitRow struct {
		model.DiscoveryHit
		WalletStatus string `json:"wallet_status"`
	}
	rows := make([]hitRow, 0, len(hits))
	for _, h := range hits {
		rows = append(rows, hitRow{h, status[h.Wallet]})
	}
	toks, _ := s.Store.ListTokens(ctx, s.Cfg.DiscoveryMult, false)
	writeJSON(w, 200, map[string]any{"hits": rows, "candidates": cands, "tokens_5x": toks})
}

func (s *Server) rotations(w http.ResponseWriter, r *http.Request) {
	rs, err := s.Store.ListRotations(r.Context(), qInt(r, "limit", 200))
	if err != nil {
		fail(w, 500, err)
		return
	}
	writeJSON(w, 200, rs)
}

// ---- health

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.Now()
	hs, err := s.Store.ListHealth(ctx)
	if err != nil {
		fail(w, 500, err)
		return
	}
	type svc struct {
		Data      json.RawMessage `json:"data"`
		UpdatedAt time.Time       `json:"updated_at"`
		Stale     bool            `json:"stale"`
	}
	services := map[string]svc{}
	for k, v := range hs {
		services[k] = svc{v.Data, v.UpdatedAt, now.Sub(v.UpdatedAt) > 90*time.Second}
	}
	lag := map[string]int64{}
	if s.Lag != nil {
		for _, g := range []string{"simulator", "executor"} {
			if n, err := s.Lag.Lag(ctx, bus.StreamTrades, g); err == nil {
				lag[g] = n
			}
		}
	}
	day := now.UTC().Truncate(24 * time.Hour)
	today, _ := s.Store.CountTokenTradesSince(ctx, day)
	last24, _ := s.Store.CountTokenTradesSince(ctx, now.Add(-24*time.Hour))
	lastMin, _ := s.Store.CountTokenTradesSince(ctx, now.Add(-time.Minute))
	fills, _ := s.Store.ListLiveFills(ctx, store.LiveFilter{Since: now.Add(-24 * time.Hour)})
	failed := 0
	for _, f := range fills {
		if f.Status == "failed" {
			failed++
		}
	}
	// Stream lag in seconds: how far behind the simulator's last processed trade is.
	var lastTrade time.Time
	if t, err := s.Store.ListTokenTrades(ctx, store.TradeFilter{Limit: 1}); err == nil && len(t) > 0 {
		lastTrade = t[0].ReceivedAt
	}
	// PumpPortal bills per wallet-trade message only when it is the wallet source;
	// the Helius stream is free on its plan (data-capped instead).
	paid := 0.01 / 10000
	if s.Cfg.WalletSource == "helius" {
		paid = 0
	}
	writeJSON(w, 200, map[string]any{
		"now": now, "services": services, "stream_lag": lag,
		"wallet_source":   s.Cfg.WalletSource,
		"msgs_per_sec_1m": float64(lastMin) / 60,
		"messages_today":  today,
		"messages_24h":    last24,
		"est_sol_today":   float64(today) * paid,
		"est_sol_24h":     float64(last24) * paid,
		"est_sol_month":   float64(last24) * paid * 30,
		"failed_tx_24h":   failed,
		"live_fills_24h":  len(fills),
		"last_trade_at":   lastTrade,
	})
}

// ---- executor

func (s *Server) executor(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := s.Now()
	kill, _, _ := s.Store.GetSetting(ctx, store.SettingKillSwitch)
	view := s.viewMode(r)
	fills, err := s.Store.ListLiveFills(ctx, store.LiveFilter{Mode: view, Limit: qInt(r, "limit", 200)})
	if err != nil {
		fail(w, 500, err)
		return
	}
	s.attachSim(ctx, fills)
	pos, _ := s.Store.ListLivePositions(ctx, store.LiveFilter{Mode: view, Since: now.AddDate(0, 0, -30)})
	day := now.UTC().Truncate(24 * time.Hour)
	var dayPnl, gapSum float64
	gapN := 0
	for _, p := range pos {
		if p.ClosedAt != nil && !p.ClosedAt.Before(day) {
			dayPnl += (p.ProceedsSol - p.CostSol) * p.SolUSD
		}
	}
	for _, f := range fills {
		if f.Status == "filled" && f.SimFillPrice > 0 && f.FillPrice > 0 {
			g := (f.FillPrice/f.SimFillPrice - 1) * 100
			if f.Side == model.SideSell {
				g = -g
			}
			gapSum += g
			gapN++
		}
	}
	avgGap := 0.0
	if gapN > 0 {
		avgGap = gapSum / float64(gapN)
	}
	writeJSON(w, 200, map[string]any{
		// mode: what the executor is running now; view: which results this response shows
		"mode": s.runMode(), "view": view, "kill_switch": kill == "on", "fills": fills, "positions": pos,
		"day_realized_usd": dayPnl, "daily_loss_limit_usd": s.Cfg.DailyLossLimitUSD,
		"max_open_positions": s.Cfg.MaxOpenPositions, "stop_loss_pct": s.Cfg.StopLossPct, "size_usd": s.Cfg.ExecSizeUSD,
		// positive = worse than simulated
		"avg_gap_vs_sim_pct": avgGap, "gap_samples": gapN,
	})
}

// attachSim fills in what the simulator got for the same target trade at the
// rotation delay, so the dashboard can show the live-vs-sim gap.
func (s *Server) attachSim(ctx context.Context, fills []model.LiveFill) {
	for i := range fills {
		f := &fills[i]
		if f.SimFillPrice > 0 || f.TargetSig == "" || f.Reason != "copy" {
			continue
		}
		sims, err := s.Store.SimTradesForTarget(ctx, f.TargetSig)
		if err != nil {
			continue
		}
		for _, x := range sims {
			if x.DelayS == s.Cfg.RotationDelayS && x.Wallet == f.Wallet && !x.Skipped {
				f.SimFillPrice = x.FillPrice
			}
		}
	}
}

// runMode is the executor's configured mode.
func (s *Server) runMode() string {
	if s.Cfg.LiveTrading {
		return "live"
	}
	return "paper"
}

// viewMode picks which executor results to show: ?mode=paper|live, defaulting to
// the mode the executor is running in, so paper and real money never mix in totals.
func (s *Server) viewMode(r *http.Request) string {
	switch m := r.URL.Query().Get("mode"); m {
	case "paper", "live":
		return m
	}
	return s.runMode()
}

func (s *Server) kill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		On bool `json:"on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(w, 400, err)
		return
	}
	v := "off"
	if body.On {
		v = "on"
	}
	if err := s.Store.SetSetting(r.Context(), store.SettingKillSwitch, v); err != nil {
		fail(w, 500, err)
		return
	}
	_ = bus.PublishJSON(r.Context(), s.Bus, bus.StreamEvents, model.Event{Kind: "info", At: s.Now(), Service: "api",
		Message: "kill switch " + v})
	writeJSON(w, 200, map[string]any{"ok": true, "kill_switch": body.On})
}

// ---- SSE

type hub struct {
	mu      sync.Mutex
	clients map[chan []byte]struct{}
}

func newHub() *hub { return &hub{clients: map[chan []byte]struct{}{}} }

func (h *hub) add() chan []byte {
	c := make(chan []byte, 64)
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

func (h *hub) remove(c chan []byte) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *hub) broadcast(b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for c := range h.clients {
		select {
		case c <- b:
		default: // slow client: drop
		}
	}
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		fail(w, 500, errors.New("streaming unsupported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	c := s.hub.add()
	defer s.hub.remove(c)
	fmt.Fprint(w, ": connected\n\n")
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case b := <-c:
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		}
	}
}

// Serve runs the HTTP server until ctx ends.
func (s *Server) Serve(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sh, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sh)
	}()
	slog.Info("api listening", "addr", addr)
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
