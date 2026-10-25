package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"copytrade/internal/curve"
	"copytrade/internal/history"
	"copytrade/internal/model"
	"copytrade/internal/solana"
)

// Helius streams tracked wallets' pump.fun trades from Solana logs (logsSubscribe,
// one subscription per wallet) over a Helius websocket. Unlike PumpPortal's account
// stream it sees trades placed through any trading terminal, and it is free on
// Helius's plan (billed by data volume, ~2 credits per 0.1 MB).
//
// Only account subscriptions are supported (snapshot mode reads prices over RPC).
//
// Quiet wallets: other copy-trading bots put popular wallets' addresses in
// thousands of (mostly failed) txs a minute, and logsSubscribe bills each one. A
// wallet over WalletDailyMB is moved to a quiet feed instead: accountSubscribe on
// the wallet itself, which only fires when its SOL balance changes (its own trades,
// or SOL sent to it). Each notification is resolved to the wallet's txs in that
// slot with getSignaturesForAddress + getTransaction (~1s slower, a few credits
// per trade). The wallet is never dropped from tracking.
type Helius struct {
	URL string
	// Chain fills in what logs don't carry: the wallet's token balance after the
	// trade (needed to copy partial sells) and events from truncated logs.
	Chain ChainReader
	// DailyMB caps streamed data per UTC day; over it the stream pauses until midnight.
	DailyMB float64
	// WalletDailyMB caps one wallet's log data per UTC day; over it the wallet moves
	// to the quiet feed and OnNoisy is called (so the caller can persist it with
	// SetQuiet on the next start).
	WalletDailyMB float64
	OnNoisy       func(wallet string, mb float64)

	MinBackoff, MaxBackoff time.Duration

	mu       sync.Mutex
	accounts map[string]bool
	dirty    chan struct{}
	paused   bool
	pauseCh  chan struct{}
	resumeCh chan struct{}
	stats    Stats
	day      time.Time
	bytes    int64
	byWallet map[string]int64 // today's bytes per wallet
	quiet    map[string]bool  // wallets on the balance-change feed
}

// ChainReader is the slice of the RPC client Helius needs.
type ChainReader interface {
	GetBalanceChange(ctx context.Context, sig, owner, mint string) (solana.BalanceChange, error)
	GetTxEvents(ctx context.Context, sig string) (solana.TxEvents, error)
	GetSignaturesForAddress(ctx context.Context, addr, before string, limit int) ([]solana.SigInfo, error)
}

func NewHelius(wsURL string, chain ChainReader, dailyMB float64) *Helius {
	return &Helius{URL: wsURL, Chain: chain, DailyMB: dailyMB, MinBackoff: 3 * time.Second, MaxBackoff: 5 * time.Minute,
		accounts: map[string]bool{}, quiet: map[string]bool{}, dirty: make(chan struct{}, 1), pauseCh: make(chan struct{}, 1), resumeCh: make(chan struct{}, 1)}
}

func (h *Helius) SubscribeAccounts(keys []string)   { h.setAccounts(keys, true) }
func (h *Helius) UnsubscribeAccounts(keys []string) { h.setAccounts(keys, false) }

// Token streams are not used in snapshot mode; prices come from curve reads.
func (h *Helius) SubscribeTokens([]string)   {}
func (h *Helius) UnsubscribeTokens([]string) {}

// SetQuiet puts wallets on the quiet (balance-change) feed.
func (h *Helius) SetQuiet(keys []string) {
	h.mu.Lock()
	for _, k := range keys {
		h.quiet[k] = true
	}
	h.mu.Unlock()
	signal(h.dirty)
}

// Quiet lists wallets on the quiet feed.
func (h *Helius) Quiet() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]string, 0, len(h.quiet))
	for k := range h.quiet {
		out = append(out, k)
	}
	return out
}

func (h *Helius) setAccounts(keys []string, add bool) {
	h.mu.Lock()
	for _, k := range keys {
		if add {
			h.accounts[k] = true
		} else {
			delete(h.accounts, k)
		}
	}
	h.mu.Unlock()
	signal(h.dirty)
}

func (h *Helius) Pause() {
	h.mu.Lock()
	h.paused = true
	h.mu.Unlock()
	signal(h.pauseCh)
}

func (h *Helius) Resume() {
	h.mu.Lock()
	h.paused = false
	h.mu.Unlock()
	signal(h.resumeCh)
}

func (h *Helius) Stats() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.stats
	s.Accounts = len(h.accounts)
	for a := range h.accounts {
		if h.quiet[a] {
			s.Quiet++
		}
	}
	s.BytesToday = h.bytes
	return s
}

// count meters streamed bytes and reports whether today's cap is used up.
func (h *Helius) count(n int, now time.Time) (over bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if d := now.UTC().Truncate(24 * time.Hour); !d.Equal(h.day) {
		h.day, h.bytes, h.byWallet = d, 0, nil
	}
	h.bytes += int64(n)
	return h.DailyMB > 0 && float64(h.bytes) >= h.DailyMB*1e6
}

// countWallet adds n bytes of log data to wallet's day. The first time the wallet
// goes over WalletDailyMB it moves to the quiet feed and noisy=true.
// Caller has called count for the same frame (which resets the day).
func (h *Helius) countWallet(wallet string, n int) (noisy bool, mb float64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.WalletDailyMB <= 0 || !h.accounts[wallet] || h.quiet[wallet] {
		return false, 0
	}
	if h.byWallet == nil {
		h.byWallet = map[string]int64{}
	}
	h.byWallet[wallet] += int64(n)
	mb = float64(h.byWallet[wallet]) / 1e6
	if mb < h.WalletDailyMB {
		return false, mb
	}
	h.quiet[wallet] = true
	return true, mb
}

func (h *Helius) isPaused() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.paused
}

// KeyQuietWallets is the setting that remembers quiet-feed wallets (JSON list).
const KeyQuietWallets = "helius_quiet_wallets"

var errOverCap = errors.New("helius daily data cap reached")

func (h *Helius) Run(ctx context.Context, handle Handler) error {
	if h.URL == "" {
		h.setErr("no Helius websocket URL (set SOLANA_RPC_URL to a Helius URL)")
		slog.Error("helius source: no websocket URL")
		<-ctx.Done()
		return ctx.Err()
	}
	// Notifications are converted in order on one worker so balance lookups never
	// stall the socket reader, and a wallet's buy is always handled before its sell.
	work := make(chan note, 1000)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case n := <-work:
				for _, t := range h.convert(ctx, n) {
					h.mu.Lock()
					h.stats.Messages++
					h.stats.LastMessageAt = n.at
					h.mu.Unlock()
					handle(t)
				}
			}
		}
	}()
	backoff := h.MinBackoff
	for ctx.Err() == nil {
		for h.isPaused() {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-h.resumeCh:
			}
		}
		started := time.Now()
		err := h.session(ctx, work)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(err, errOverCap) {
			// Sleep until the next UTC day, then reconnect.
			next := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
			h.setErr(fmt.Sprintf("daily data cap (%.0f MB) reached; paused until %s UTC", h.DailyMB, next.Format("Jan 2 15:04")))
			slog.Warn("helius daily data cap reached; paused until midnight UTC", "mb", h.DailyMB)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Until(next)):
			}
			continue
		}
		if errors.Is(err, errPaused) {
			continue
		}
		h.setErr(errString(err))
		if time.Since(started) > 2*time.Minute {
			backoff = h.MinBackoff
		}
		wait := backoff + time.Duration(rand.Int64N(int64(backoff/2)+1))
		slog.Warn("helius websocket disconnected, reconnecting", "err", err, "wait", wait.Round(time.Second))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
		backoff = min(backoff*2, h.MaxBackoff)
	}
	return ctx.Err()
}

type note struct {
	wallet, sig string
	logs        []string
	at          time.Time
	slot        uint64 // quiet feed: the slot the wallet's balance changed in (sig is empty)
}

func (h *Helius) session(ctx context.Context, work chan<- note) error {
	conn, _, err := (&websocket.Dialer{HandshakeTimeout: 15 * time.Second}).DialContext(ctx, h.URL, nil)
	if err != nil {
		return errors.New("dial helius websocket failed") // the URL holds the API key: never log it
	}
	defer conn.Close()
	h.mu.Lock()
	h.stats.Connected = true
	h.stats.Reconnects++
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.stats.Connected = false
		h.mu.Unlock()
	}()

	// Subscription bookkeeping, owned by this goroutine.
	type sub struct {
		wallet string
		quiet  bool // accountSubscribe rather than logsSubscribe
	}
	subOf := map[string]int64{} // wallet -> subscription id
	kindOf := map[string]bool{} // wallet -> its subscription is the quiet feed
	walletOf := map[int64]sub{} // subscription id -> wallet
	reqWallet := map[int]sub{}  // pending subscribe request id -> wallet
	inflight := map[string]bool{}
	nextID := 0
	resync := func() error {
		h.mu.Lock()
		want := make(map[string]bool, len(h.accounts)) // wallet -> quiet
		for a := range h.accounts {
			want[a] = h.quiet[a]
		}
		h.mu.Unlock()
		// Drop subscriptions no longer wanted, or of the wrong kind.
		for a, sid := range subOf {
			if q, ok := want[a]; ok && q == kindOf[a] {
				continue
			}
			method := "logsUnsubscribe"
			if kindOf[a] {
				method = "accountUnsubscribe"
			}
			nextID++
			delete(subOf, a)
			delete(kindOf, a)
			delete(walletOf, sid)
			if err := conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": nextID, "method": method, "params": []any{sid}}); err != nil {
				return err
			}
		}
		for a, q := range want {
			if _, ok := subOf[a]; ok || inflight[a] {
				continue
			}
			nextID++
			reqWallet[nextID] = sub{a, q}
			inflight[a] = true
			req := map[string]any{"jsonrpc": "2.0", "id": nextID, "method": "logsSubscribe",
				"params": []any{map[string]any{"mentions": []string{a}}, map[string]any{"commitment": "confirmed"}}}
			if q {
				req["method"] = "accountSubscribe"
				req["params"] = []any{a, map[string]any{"commitment": "confirmed", "encoding": "base64"}}
			}
			if err := conn.WriteJSON(req); err != nil {
				return err
			}
		}
		return nil
	}
	if err := resync(); err != nil {
		return err
	}
	slog.Info("helius websocket connected", "accounts", len(reqWallet))

	type frame struct {
		data []byte
		at   time.Time
	}
	frames := make(chan frame, 1024)
	readErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	conn.SetPongHandler(func(string) error { return conn.SetReadDeadline(time.Now().Add(3 * time.Minute)) })
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Minute))
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				readErr <- err
				return
			}
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Minute))
			select {
			case frames <- frame{data, time.Now()}:
			case <-done:
				return
			}
		}
	}()
	ping := time.NewTicker(30 * time.Second) // Helius closes idle sockets after 10 minutes
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			_ = conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			return ctx.Err()
		case err := <-readErr:
			return err
		case <-ping.C:
			if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
				return err
			}
		case <-h.pauseCh:
			if h.isPaused() {
				return errPaused
			}
		case <-h.dirty:
			if err := resync(); err != nil {
				return err
			}
		case f := <-frames:
			if h.count(len(f.data), f.at) {
				return errOverCap
			}
			var m struct {
				ID     int             `json:"id"`
				Method string          `json:"method"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
				Params struct {
					Subscription int64 `json:"subscription"`
					Result       struct {
						Context struct {
							Slot uint64 `json:"slot"`
						} `json:"context"`
						Value struct {
							Signature string   `json:"signature"`
							Err       any      `json:"err"`
							Logs      []string `json:"logs"`
						} `json:"value"`
					} `json:"result"`
				} `json:"params"`
			}
			if json.Unmarshal(f.data, &m) != nil {
				continue
			}
			if m.ID != 0 {
				if r, ok := reqWallet[m.ID]; ok {
					delete(reqWallet, m.ID)
					delete(inflight, r.wallet)
					var sid int64
					if len(m.Error) > 0 || json.Unmarshal(m.Result, &sid) != nil {
						h.setErr("subscribe failed: " + string(m.Error))
						continue
					}
					subOf[r.wallet], kindOf[r.wallet], walletOf[sid] = sid, r.quiet, r
					// The wanted kind may have changed while the request was in flight.
					signal(h.dirty)
				}
				continue
			}
			sb := walletOf[m.Params.Subscription]
			w := sb.wallet
			if sb.quiet {
				if m.Method == "accountNotification" {
					select {
					case work <- note{wallet: w, slot: m.Params.Result.Context.Slot, at: f.at}:
					default:
						h.setErr("trade queue full; dropped a notification")
					}
				}
				continue
			}
			if w != "" {
				if noisy, mb := h.countWallet(w, len(f.data)); noisy {
					slog.Warn("helius: wallet over its daily log-data cap, moving it to the quiet feed", "wallet", w, "mb", mb)
					if err := resync(); err != nil {
						return err
					}
					if h.OnNoisy != nil {
						go h.OnNoisy(w, mb)
					}
					continue
				}
			}
			v := m.Params.Result.Value
			if w == "" || v.Err != nil || v.Signature == "" {
				continue // unknown sub, or a failed tx
			}
			if !mentionsPump(v.Logs) {
				continue // transfers, airdrop spam, other DEXes
			}
			select {
			case work <- note{wallet: w, sig: v.Signature, logs: v.Logs, at: f.at}:
			default:
				h.setErr("trade queue full; dropped a notification")
			}
		}
	}
}

func mentionsPump(logs []string) bool {
	for _, l := range logs {
		if strings.Contains(l, curve.PumpProgram) {
			return true
		}
	}
	return false
}

// convert turns one notification into the wallet's trades, filling in the
// wallet's token balance after the trade.
func (h *Helius) convert(ctx context.Context, n note) []model.Trade {
	if n.sig == "" {
		return h.convertQuiet(ctx, n)
	}
	trades, truncated := TradesFromLogs(n.sig, n.wallet, n.logs, n.at)
	if truncated && h.Chain != nil {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		tx, err := h.Chain.GetTxEvents(cctx, n.sig)
		cancel()
		if err == nil && !tx.Failed {
			trades = tradesFromEvents(n.sig, n.wallet, history.EventsFromTx(tx), n.at)
		}
	}
	for i := range trades {
		t := &trades[i]
		t.NewTokenBalance = h.balanceAfter(ctx, *t)
	}
	return trades
}

// convertQuiet resolves a quiet-feed balance change to the wallet's pump.fun
// trades in that slot. Bots fill the wallet's signature list with their own txs,
// so the list is filtered to successful txs in the notified slot.
func (h *Helius) convertQuiet(ctx context.Context, n note) []model.Trade {
	if h.Chain == nil {
		return nil
	}
	var sigs []string
	for attempt := 0; attempt < 3 && len(sigs) == 0; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(time.Second):
			}
		}
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		list, err := h.Chain.GetSignaturesForAddress(cctx, n.wallet, "", 1000)
		cancel()
		if err != nil {
			continue
		}
		for _, si := range list {
			if si.Slot == n.slot && si.Err == nil {
				sigs = append(sigs, si.Signature)
			}
		}
	}
	var trades []model.Trade
	for _, sig := range sigs {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		tx, err := h.Chain.GetTxEvents(cctx, sig)
		cancel()
		if err != nil || tx.Failed || !mentionsPump(tx.Logs) {
			continue // a SOL transfer, another DEX, or a lookup error
		}
		ts := tradesFromEvents(sig, n.wallet, history.EventsFromTx(tx), n.at)
		for i := range ts {
			ts[i].NewTokenBalance = h.balanceAfter(ctx, ts[i])
		}
		trades = append(trades, ts...)
	}
	return trades
}

// balanceAfter reads the wallet's post-trade balance (tx may need a moment to be
// queryable). Fallback: a buy holds what it bought, a sell is treated as a full exit.
func (h *Helius) balanceAfter(ctx context.Context, t model.Trade) float64 {
	sig, _, _ := strings.Cut(t.Signature, "#")
	if h.Chain != nil {
		for attempt := 0; attempt < 2; attempt++ {
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			bc, err := h.Chain.GetBalanceChange(cctx, sig, t.Trader, t.Mint)
			cancel()
			if err == nil {
				return bc.TokenAfter
			}
			select {
			case <-ctx.Done():
				return 0
			case <-time.After(500 * time.Millisecond):
			}
		}
	}
	if t.Side == model.SideBuy {
		return t.TokenAmount
	}
	return 0
}

// TradesFromLogs decodes the wallet's pump.fun trades from a transaction's logs.
// truncated reports that the RPC cut the logs short (events may be missing).
func TradesFromLogs(sig, wallet string, logs []string, at time.Time) (trades []model.Trade, truncated bool) {
	for _, l := range logs {
		if strings.Contains(l, "Log truncated") {
			truncated = true
		}
	}
	var evs []history.Event
	for _, b := range history.PumpLogData(logs) {
		if len(b) >= 8 && history.IsTradeEvent(b[:8]) {
			if e, err := history.DecodeEvent(b[8:]); err == nil {
				evs = append(evs, e)
			}
		}
	}
	return tradesFromEvents(sig, wallet, evs, at), truncated
}

func tradesFromEvents(sig, wallet string, evs []history.Event, at time.Time) []model.Trade {
	var mine []history.Event
	for _, e := range evs {
		if e.User == wallet {
			mine = append(mine, e)
		}
	}
	var out []model.Trade
	for i, e := range mine {
		t := model.Trade{Signature: sig, Mint: e.Mint, Trader: wallet, Side: model.SideSell,
			SolAmount: e.SolAmount, TokenAmount: e.TokenAmount, VSol: e.VSol, VTokens: e.VTokens,
			Pool: "pump", ReceivedAt: at.UTC()}
		if len(mine) > 1 {
			t.Signature = fmt.Sprintf("%s#%d", sig, i)
		}
		if e.IsBuy {
			t.Side = model.SideBuy
		}
		if e.VSol <= 0 || e.VTokens <= 0 {
			// Curves quoted in another token report no SOL reserves; our SOL math can't price them.
			t.Pool = "nonsol"
		} else {
			t.MarketCapSol = e.VSol / e.VTokens * 1e9
		}
		if key, err := curve.DeriveBondingCurve(e.Mint); err == nil {
			t.BondingCurveKey = key
		}
		out = append(out, t)
	}
	return out
}

func (h *Helius) setErr(s string) {
	h.mu.Lock()
	h.stats.LastError = s
	h.mu.Unlock()
}
