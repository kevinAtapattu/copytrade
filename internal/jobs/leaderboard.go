package jobs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/model"
	"copytrade/internal/rotation"
	"copytrade/internal/store"
)

// Leader is one trader from a public leaderboard. Days is the leaderboard's
// window (1, 7 or 30); 0 means listed without stats.
type Leader struct {
	Address, Name string
	Profit        float64 // SOL over Days
	Wins, Losses  int
	Days          int
}

// WinRate is smoothed toward 50% so 16 wins and 0 losses doesn't outrank 555 and 76.
func (l Leader) WinRate() float64 {
	return float64(l.Wins+1) / float64(l.Wins+l.Losses+2)
}

var kolscanRow = regexp.MustCompile(`\{"wallet_address":"[1-9A-HJ-NP-Za-km-z]{32,44}"[^{}]*\}`)

// ParseKolscan reads the trader list Kolscan's leaderboard page embeds: about
// 500 traders, ~50 of them with 1/7/30-day profit and win/loss counts. One
// entry per address, keeping its 7-day stats if it has them, then 30, then 1.
func ParseKolscan(page []byte) []Leader {
	text := strings.ReplaceAll(string(page), `\"`, `"`)
	rank := map[int]int{7: 3, 30: 2, 1: 1}
	by := map[string]Leader{}
	var order []string
	for _, m := range kolscanRow.FindAllString(text, -1) {
		var r struct {
			Address   string   `json:"wallet_address"`
			Name      string   `json:"name"`
			Profit    *float64 `json:"profit"`
			Wins      int      `json:"wins"`
			Losses    int      `json:"losses"`
			Timeframe int      `json:"timeframe"`
		}
		if json.Unmarshal([]byte(m), &r) != nil {
			continue
		}
		l := Leader{Address: r.Address, Name: r.Name, Wins: r.Wins, Losses: r.Losses}
		if r.Profit != nil && rank[r.Timeframe] > 0 {
			l.Profit, l.Days = *r.Profit, r.Timeframe
		}
		old, seen := by[l.Address]
		if !seen {
			order = append(order, l.Address)
		}
		if !seen || rank[l.Days] > rank[old.Days] {
			if l.Name == "" {
				l.Name = old.Name
			}
			by[l.Address] = l
		}
	}
	out := make([]Leader, 0, len(order))
	for _, a := range order {
		out = append(out, by[a])
	}
	return out
}

func FetchKolscan(ctx context.Context, hc *http.Client, url string) ([]Leader, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("leaderboard %s", resp.Status)
	}
	page, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	ls := ParseKolscan(page)
	if len(ls) == 0 {
		return nil, fmt.Errorf("leaderboard page had no traders (layout changed?)")
	}
	return ls, nil
}

// Refill tops the watched set (live + bench) up to target with leaders we have
// not tried, best first: profitable with stats, by win rate then profit, then the
// rest of the list in page order. A dropped wallet gets another try once it was
// dropped at least retryAfter ago and is profitable on the board again; retryAfter
// should cover the 30-day score window, or the old losses drop it straight away.
func Refill(ctx context.Context, st store.Store, b bus.Bus, leaders []Leader, target int, retryAfter time.Duration, now time.Time) ([]string, error) {
	wallets, err := st.ListWallets(ctx)
	if err != nil {
		return nil, err
	}
	have := map[string]model.Wallet{}
	watched := 0
	for _, w := range wallets {
		have[w.Address] = w
		if w.Status != model.StatusDropped {
			watched++
		}
	}
	free := target - watched
	if free <= 0 {
		return nil, nil
	}
	good := func(l Leader) bool { return l.Days > 0 && l.Profit > 0 }
	var cands []Leader
	for _, l := range leaders {
		w, known := have[l.Address]
		if !known || (w.Status == model.StatusDropped && good(l) && now.Sub(w.StatusAt) >= retryAfter) {
			cands = append(cands, l)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if good(a) != good(b) {
			return good(a)
		}
		if !good(a) {
			return false // keep page order
		}
		if a.WinRate() != b.WinRate() {
			return a.WinRate() > b.WinRate()
		}
		return a.Profit > b.Profit
	})
	var added []string
	for _, l := range cands {
		if len(added) >= free {
			break
		}
		reason := "kolscan: listed trader"
		if l.Days > 0 {
			reason = fmt.Sprintf("kolscan %dd: %+.1f SOL, %d won / %d lost", l.Days, l.Profit, l.Wins, l.Losses)
		}
		var ok bool
		if w, known := have[l.Address]; known {
			reason = "back on the leaderboard: " + reason
			ok, err = st.SetWalletStatus(ctx, w.Address, model.StatusBench, reason, now)
			if ok {
				EmitRotations(ctx, b, "discovery", []rotation.Change{{Wallet: w.Address, From: model.StatusDropped, To: model.StatusBench, Reason: reason}}, now)
			}
		} else {
			ok, err = st.AddWallet(ctx, model.Wallet{Address: l.Address, Label: l.Name, Status: model.StatusBench,
				Source: model.SourceLeaderboard, Reason: reason, AddedAt: now, StatusAt: now})
			if ok {
				slog.Info("leaderboard wallet added", "wallet", l.Address, "name", l.Name, "reason", reason)
				_ = bus.PublishJSON(ctx, b, bus.StreamEvents, model.Event{Kind: "discovery", At: now, Service: "discovery",
					Message: "new bench wallet " + nameOr(l) + ": " + reason, Data: map[string]any{"wallet": l.Address, "reason": reason}})
			}
		}
		if err != nil {
			return added, err
		}
		if ok {
			added = append(added, l.Address)
		}
	}
	return added, nil
}

func nameOr(l Leader) string {
	if l.Name != "" {
		return l.Name
	}
	return short(l.Address)
}
