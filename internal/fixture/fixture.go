// Package fixture generates deterministic PumpPortal-shaped trade data with
// scripted wallet behaviors, so the full pipeline can be tested with no API key.
//
// Wallets:
//   - Good: buys early, real buyers follow, sells in two steps at ~2x. Copyable.
//   - Lucky: mostly small losers plus one 10x token. Positive only because of one trade.
//   - Draw: starts LIVE, a few good days then a losing streak. Should get demoted.
//   - Fast: flips within ~3s after a spike. Profitable for itself, not copyable at +2s.
//   - Idle: never trades. Should get auto-dropped.
//   - Early1/Early2 (untracked): buy right after Good on tokens that run 5x+ and exit at 2x+.
//     Discovery should add them to the bench.
package fixture

import (
	"encoding/json"
	"io"
	"math/rand/v2"
	"sort"
	"time"

	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/source"
)

type Wallets struct {
	Good, Lucky, Draw, Fast, Idle, Early1, Early2 string
}

type Meta struct {
	Start   time.Time      `json:"start"`
	End     time.Time      `json:"end"`
	Wallets []model.Wallet `json:"wallets"`
	Roles   Wallets        `json:"roles"`
}

type Data struct {
	Meta  Meta
	Lines []source.FixtureLine
}

type gen struct {
	r      *rand.Rand
	trades []model.Trade
	noise  []string
}

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func (g *gen) addr(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[g.r.IntN(len(alphabet))]
	}
	return string(b)
}

// token simulates one pump.fun curve, appending trades with post-trade reserves.
type token struct {
	g       *gen
	mint    string
	st      curve.State
	t0      time.Time
	holding map[string]float64
}

func (g *gen) newToken(t0 time.Time) *token {
	// Start a little up the curve, like a token a few minutes after launch.
	tk := &token{g: g, mint: g.addr(40) + "pump", t0: t0, holding: map[string]float64{},
		st: curve.State{VSol: curve.InitialVirtualSol, VTokens: curve.InitialVirtualTokens}}
	tk.buy(g.noise[0], 2+g.r.Float64()*2, -30*time.Second)
	return tk
}

func (tk *token) emit(trader, side string, sol, tokens float64, at time.Duration) {
	bal := tk.holding[trader]
	tk.g.trades = append(tk.g.trades, model.Trade{
		Signature: tk.g.addr(88), Mint: tk.mint, Trader: trader, Side: side, SolAmount: sol, TokenAmount: tokens,
		NewTokenBalance: bal, VSol: tk.st.VSol, VTokens: tk.st.VTokens, MarketCapSol: tk.st.Price() * 1e9,
		Pool: "pump", BondingCurveKey: tk.mint[:32], ReceivedAt: tk.t0.Add(at),
	})
}

func (tk *token) buy(trader string, sol float64, at time.Duration) {
	out, ns, err := tk.st.BuyOut(sol)
	if err != nil {
		return
	}
	tk.st = ns
	tk.holding[trader] += out
	tk.emit(trader, model.SideBuy, sol, out, at)
}

// sell sells frac of trader's holding.
func (tk *token) sell(trader string, frac float64, at time.Duration) {
	amt := tk.holding[trader] * frac
	if frac >= 1 {
		amt = tk.holding[trader]
	}
	if amt <= 0 {
		return
	}
	sol, ns, err := tk.st.SellOut(amt)
	if err != nil {
		return
	}
	tk.st = ns
	tk.holding[trader] -= amt
	if frac >= 1 {
		tk.holding[trader] = 0
	}
	tk.emit(trader, model.SideSell, sol, amt, at)
}

func (tk *token) noiseBuys(n int, from, to time.Duration, minSol, maxSol float64) {
	for i := 0; i < n; i++ {
		at := from + time.Duration(float64(to-from)*float64(i)/float64(max(n, 1)))
		at += time.Duration(tk.g.r.Float64() * float64(time.Second))
		tr := tk.g.noise[1+tk.g.r.IntN(len(tk.g.noise)-1)]
		tk.buy(tr, minSol+tk.g.r.Float64()*(maxSol-minSol), at)
	}
}

// dump makes every noise holder sell over a window.
func (tk *token) dump(from, to time.Duration) {
	var hs []string
	for tr := range tk.holding {
		hs = append(hs, tr)
	}
	sort.Strings(hs)
	var noise []string
	for _, tr := range hs {
		if tk.isNoise(tr) && tk.holding[tr] > 0 {
			noise = append(noise, tr)
		}
	}
	for i, tr := range noise {
		at := from + time.Duration(float64(to-from)*float64(i)/float64(max(len(noise), 1)))
		tk.sell(tr, 1, at)
	}
}

func (tk *token) isNoise(tr string) bool {
	for _, n := range tk.g.noise {
		if n == tr {
			return true
		}
	}
	return false
}

// Generate builds `days` of data starting at start.
func Generate(seed uint64, start time.Time, days int) Data {
	g := &gen{r: rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))}
	for i := 0; i < 120; i++ {
		g.noise = append(g.noise, g.addr(44))
	}
	w := Wallets{Good: g.addr(44), Lucky: g.addr(44), Draw: g.addr(44), Fast: g.addr(44), Idle: g.addr(44),
		Early1: g.addr(44), Early2: g.addr(44)}

	for d := 0; d < days; d++ {
		day := start.AddDate(0, 0, d)
		for k := 0; k < 3; k++ {
			slot := day.Add(time.Duration(2+k*7)*time.Hour + time.Duration(g.r.IntN(3600))*time.Second)
			runner := k == 0 && d%3 == 1 // every 3rd day Good's first token runs 5x+
			g.goodToken(w, slot, runner)
			if k == 1 { // Lucky trades once a day
				g.luckyToken(w, slot.Add(17*time.Minute), d == 9)
			}
			if d < 3 {
				g.goodLike(w.Draw, slot.Add(33*time.Minute))
			} else {
				g.loser(w.Draw, slot.Add(33*time.Minute), 0.6)
			}
			g.fastToken(w.Fast, slot.Add(49*time.Minute))
		}
	}
	sort.SliceStable(g.trades, func(i, j int) bool { return g.trades[i].ReceivedAt.Before(g.trades[j].ReceivedAt) })

	data := Data{Meta: Meta{Start: start, End: start.AddDate(0, 0, days), Roles: w}}
	added := start.Add(-time.Hour)
	for _, x := range []struct{ a, label, status string }{
		{w.Good, "good", model.StatusBench}, {w.Lucky, "lucky", model.StatusBench}, {w.Draw, "draw", model.StatusLive},
		{w.Fast, "fast", model.StatusBench}, {w.Idle, "idle", model.StatusBench},
	} {
		data.Meta.Wallets = append(data.Meta.Wallets, model.Wallet{Address: x.a, Label: x.label, Status: x.status,
			Source: model.SourceSeed, Reason: "fixture", AddedAt: added, StatusAt: added})
	}
	for _, t := range g.trades {
		data.Lines = append(data.Lines, source.FixtureLine{At: t.ReceivedAt, Msg: source.EncodePumpPortal(t)})
	}
	return data
}

func (g *gen) goodToken(w Wallets, t0 time.Time, runner bool) {
	tk := g.newToken(t0)
	tk.buy(w.Good, 1.0, 0)
	if runner {
		tk.buy(w.Early1, 0.5, 1500*time.Millisecond)
		tk.buy(w.Early2, 0.4, 2500*time.Millisecond)
		tk.noiseBuys(40, 20*time.Second, 6*time.Minute, 0.8, 1.6)
		tk.sell(w.Early1, 1, 6*time.Minute+10*time.Second)
		tk.sell(w.Good, 0.5, 6*time.Minute+30*time.Second)
		tk.sell(w.Early2, 1, 6*time.Minute+50*time.Second)
		tk.sell(w.Good, 1, 7*time.Minute+30*time.Second)
		tk.dump(8*time.Minute, 14*time.Minute)
		return
	}
	tk.noiseBuys(18, 15*time.Second, 3*time.Minute, 0.5, 1.4)
	tk.sell(w.Good, 0.5, 3*time.Minute+20*time.Second)
	tk.noiseBuys(3, 3*time.Minute+30*time.Second, 4*time.Minute, 0.3, 0.6)
	tk.sell(w.Good, 1, 4*time.Minute+15*time.Second)
	tk.dump(5*time.Minute, 9*time.Minute)
}

// goodLike: same shape as Good, for another wallet.
func (g *gen) goodLike(wallet string, t0 time.Time) {
	tk := g.newToken(t0)
	tk.buy(wallet, 1.0, 0)
	tk.noiseBuys(18, 15*time.Second, 3*time.Minute, 0.5, 1.4)
	tk.sell(wallet, 1, 3*time.Minute+30*time.Second)
	tk.dump(4*time.Minute, 8*time.Minute)
}

// loser: wallet buys, a little follow-through, then holders dump and the wallet exits low.
func (g *gen) loser(wallet string, t0 time.Time, depth float64) {
	tk := g.newToken(t0)
	tk.noiseBuys(6, -25*time.Second, -2*time.Second, 0.8, 1.5)
	tk.buy(wallet, 1.0, 0)
	tk.noiseBuys(2, 5*time.Second, 20*time.Second, 0.2, 0.4)
	tk.dump(40*time.Second, 40*time.Second+time.Duration(depth*float64(2*time.Minute)))
	tk.sell(wallet, 1, 3*time.Minute)
}

// mildLoser: wallet buys, a couple of holders sell, wallet cuts early for a small loss.
func (g *gen) mildLoser(wallet string, t0 time.Time) {
	tk := g.newToken(t0)
	tk.noiseBuys(6, -25*time.Second, -2*time.Second, 0.8, 1.5)
	tk.buy(wallet, 1.0, 0)
	tk.sell(tk.g.noise[0], 1, 30*time.Second)
	tk.sell(wallet, 1, 75*time.Second)
	tk.dump(2*time.Minute, 4*time.Minute)
}

func (g *gen) luckyToken(w Wallets, t0 time.Time, moon bool) {
	if !moon {
		g.mildLoser(w.Lucky, t0)
		return
	}
	tk := g.newToken(t0)
	tk.buy(w.Lucky, 1.0, 0)
	tk.noiseBuys(55, 10*time.Second, 8*time.Minute, 1.0, 1.8)
	tk.sell(w.Lucky, 1, 8*time.Minute+20*time.Second)
	tk.dump(9*time.Minute, 15*time.Minute)
}

// fastToken: wallet buys, a burst of buys in the next second, wallet sells at +2.5s, then a dump.
func (g *gen) fastToken(wallet string, t0 time.Time) {
	tk := g.newToken(t0)
	tk.buy(wallet, 1.5, 0)
	tk.noiseBuys(8, 200*time.Millisecond, 1800*time.Millisecond, 1.0, 2.0)
	tk.sell(wallet, 1, 2500*time.Millisecond)
	tk.dump(3*time.Second, 8*time.Second)
}

// Write writes the fixture as JSONL plus its meta.
func (d Data) Write(lines io.Writer, meta io.Writer) error {
	enc := json.NewEncoder(lines)
	for _, l := range d.Lines {
		if err := enc.Encode(l); err != nil {
			return err
		}
	}
	return json.NewEncoder(meta).Encode(d.Meta)
}
