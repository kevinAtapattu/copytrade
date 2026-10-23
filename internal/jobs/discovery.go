package jobs

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/config"
	"copytrade/internal/model"
	"copytrade/internal/rotation"
	"copytrade/internal/store"
)

// DiscoveryResult summarizes one discovery pass.
type DiscoveryResult struct {
	TokensScanned int
	NewHits       int
	Added         []string
	Dropped       []rotation.Change
}

// tradePrice is the realized SOL/token of a trade, falling back to post-trade reserves.
func tradePrice(t model.Trade) float64 {
	if t.SolAmount > 0 && t.TokenAmount > 0 {
		return t.SolAmount / t.TokenAmount
	}
	return t.Price()
}

// FindHits returns early buyers on a token who exited well. trades may be in any order.
func FindHits(tok model.Token, trades []model.Trade, tracked map[string]bool, c config.Config, now time.Time) []model.DiscoveryHit {
	sort.SliceStable(trades, func(i, j int) bool { return trades[i].ReceivedAt.Before(trades[j].ReceivedAt) })
	type agg struct {
		firstBuy       float64
		solIn, tokIn   float64
		solOut, tokOut float64
	}
	by := map[string]*agg{}
	var order []string
	for _, t := range trades {
		if tracked[t.Trader] || t.TokenAmount <= 0 {
			continue
		}
		a := by[t.Trader]
		if a == nil {
			a = &agg{}
			by[t.Trader] = a
			order = append(order, t.Trader)
		}
		if t.Side == model.SideBuy {
			if a.firstBuy == 0 {
				a.firstBuy = tradePrice(t)
			}
			a.solIn += t.SolAmount
			a.tokIn += t.TokenAmount
		} else {
			a.solOut += t.SolAmount
			a.tokOut += t.TokenAmount
		}
	}
	mult := tok.MaxPrice / tok.FirstPrice
	var hits []model.DiscoveryHit
	for _, w := range order {
		a := by[w]
		if a.firstBuy == 0 || a.tokIn <= 0 || a.tokOut <= 0 {
			continue
		}
		if a.firstBuy > tok.FirstPrice*c.DiscoveryEarlyMult { // not early
			continue
		}
		entry := a.solIn / a.tokIn
		exit := a.solOut / a.tokOut
		soldFrac := min(a.tokOut/a.tokIn, 1)
		if exit/entry < c.DiscoveryExitMult || soldFrac < 0.5 {
			continue
		}
		hits = append(hits, model.DiscoveryHit{
			Wallet: w, Mint: tok.Mint, TokenMult: mult, EntryPrice: entry, ExitPrice: exit, ExitMult: exit / entry,
			ProfitSol: a.solOut - a.solIn*soldFrac, FoundAt: now,
		})
	}
	return hits
}

// History supplies a token's trades for discovery. Stream mode reads what we
// stored; snapshot mode fetches the token's early history from the chain.
type History interface {
	TokenTrades(ctx context.Context, tok model.Token) ([]model.Trade, error)
}

// StoreHistory serves trades we stored from the stream.
type StoreHistory struct{ Store store.Store }

func (h StoreHistory) TokenTrades(ctx context.Context, tok model.Token) ([]model.Trade, error) {
	return h.Store.ListTokenTrades(ctx, store.TradeFilter{Mint: tok.Mint})
}

// Discover scans 5x+ tokens, records hits, adds repeat names to the bench, then
// prunes inactive/bad/over-cap bench wallets. hist nil means StoreHistory.
func Discover(ctx context.Context, st store.Store, b bus.Bus, c config.Config, now time.Time, hist History) (DiscoveryResult, error) {
	if hist == nil {
		hist = StoreHistory{st}
	}
	var res DiscoveryResult
	wallets, err := st.ListWallets(ctx)
	if err != nil {
		return res, err
	}
	known := map[string]bool{}
	tracked := map[string]bool{}
	for _, w := range wallets {
		known[w.Address] = true
		if w.Status != model.StatusDropped {
			tracked[w.Address] = true
		}
	}
	toks, err := st.ListTokens(ctx, c.DiscoveryMult, true)
	if err != nil {
		return res, err
	}
	for _, tok := range toks {
		trades, err := hist.TokenTrades(ctx, tok)
		if err != nil {
			// Leave the token unscanned so the next run retries it.
			slog.Warn("token history", "mint", tok.Mint, "err", err)
			continue
		}
		res.TokensScanned++
		// "Early" is judged against the earliest price we have: launch for chain
		// history, our first observation for stored stream data.
		eff := tok
		for _, t := range trades {
			if p := tradePrice(t); p > 0 && (eff.FirstPrice <= 0 || p < eff.FirstPrice) && !t.ReceivedAt.After(tok.FirstSeen) {
				eff.FirstPrice = p
			}
		}
		for _, h := range FindHits(eff, trades, tracked, c, now) {
			ok, err := st.InsertDiscoveryHit(ctx, h)
			if err != nil {
				return res, err
			}
			if ok {
				res.NewHits++
			}
		}
		if err := st.MarkTokenDiscovered(ctx, tok.Mint); err != nil {
			return res, err
		}
	}

	// Repeat names: distinct 5x tokens per wallet across all hits.
	hits, err := st.ListDiscoveryHits(ctx, 0)
	if err != nil {
		return res, err
	}
	per := map[string][]model.DiscoveryHit{}
	for _, h := range hits {
		per[h.Wallet] = append(per[h.Wallet], h)
	}
	cands := make([]string, 0, len(per))
	for w := range per {
		cands = append(cands, w)
	}
	sort.Strings(cands)
	for _, w := range cands {
		hs := per[w]
		if len(hs) < c.DiscoveryMinHits || known[w] {
			continue
		}
		var mints []string
		var sumExit float64
		for _, h := range hs {
			mints = append(mints, short(h.Mint))
			sumExit += h.ExitMult
		}
		reason := fmt.Sprintf("early buyer on %d tokens that ran %.0fx+ (%s), avg exit %.1fx",
			len(hs), c.DiscoveryMult, strings.Join(mints, ", "), sumExit/float64(len(hs)))
		added, err := st.AddWallet(ctx, model.Wallet{Address: w, Status: model.StatusBench, Source: model.SourceDiscovery,
			Reason: reason, AddedAt: now, StatusAt: now})
		if err != nil {
			return res, err
		}
		if added {
			res.Added = append(res.Added, w)
			slog.Info("discovered wallet", "wallet", w, "reason", reason)
			_ = bus.PublishJSON(ctx, b, bus.StreamEvents, model.Event{Kind: "discovery", At: now, Service: "discovery",
				Message: "new bench candidate " + short(w) + ": " + reason, Data: map[string]any{"wallet": w, "reason": reason}})
		}
	}

	// Prune with latest scores.
	wallets, err = st.ListWallets(ctx)
	if err != nil {
		return res, err
	}
	scores, err := st.LatestScores(ctx)
	if err != nil {
		return res, err
	}
	rules := rotation.FromConfig(c)
	res.Dropped, err = rotation.Apply(ctx, st, rules.Prune(wallets, rotation.Index(scores), now), now)
	EmitRotations(ctx, b, "discovery", res.Dropped, now)
	return res, err
}
