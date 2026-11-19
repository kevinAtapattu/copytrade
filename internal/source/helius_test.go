package source

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mr-tron/base58"

	"copytrade/internal/curve"
	"copytrade/internal/history"
	"copytrade/internal/model"
	"copytrade/internal/solana"
)

const (
	hMint   = "So11111111111111111111111111111111111111112"
	hWallet = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
	router  = "term9YPb9mzAsABaqN71A4xdbxHmpBNZavpBiQKZzN3"
)

func pumpLogs(evs ...history.Event) []string {
	logs := []string{"Program " + router + " invoke [1]", "Program " + curve.PumpProgram + " invoke [2]"}
	for _, e := range evs {
		logs = append(logs, "Program data: "+base64.StdEncoding.EncodeToString(history.EncodeEvent(e)))
	}
	return append(logs, "Program "+curve.PumpProgram+" success", "Program "+router+" success")
}

func TestTradesFromLogs(t *testing.T) {
	at := time.Unix(1700000000, 0)
	buy := history.Event{Mint: hMint, User: hWallet, IsBuy: true, SolAmount: 1.5, TokenAmount: 40e6, VSol: 40, VTokens: 800e6, Timestamp: 1}
	other := history.Event{Mint: hMint, User: "Other1111111111111111111111111111111111111", IsBuy: true, SolAmount: 1, TokenAmount: 1, VSol: 41, VTokens: 790e6}
	tr, trunc := TradesFromLogs("SIG", hWallet, pumpLogs(buy, other), at)
	if trunc || len(tr) != 1 || tr[0].Signature != "SIG" || tr[0].Side != model.SideBuy || tr[0].Pool != "pump" ||
		tr[0].SolAmount != 1.5 || tr[0].BondingCurveKey == "" || tr[0].MarketCapSol <= 0 {
		t.Fatalf("got %+v", tr)
	}
	// Two of the wallet's trades in one tx get distinct signatures.
	tr, _ = TradesFromLogs("SIG", hWallet, pumpLogs(buy, buy), at)
	if len(tr) != 2 || tr[0].Signature != "SIG#0" || tr[1].Signature != "SIG#1" {
		t.Fatalf("multi: %+v", tr)
	}
	// Non-SOL curve: no SOL reserves.
	tr, _ = TradesFromLogs("SIG", hWallet, pumpLogs(history.Event{Mint: hMint, User: hWallet, IsBuy: true, TokenAmount: 5}), at)
	if len(tr) != 1 || tr[0].Pool != "nonsol" {
		t.Fatalf("nonsol: %+v", tr)
	}
	if _, trunc := TradesFromLogs("SIG", hWallet, append(pumpLogs(buy), "Log truncated"), at); !trunc {
		t.Fatal("truncation not detected")
	}
}

type fakeChain struct {
	after    map[string]float64
	sigs     []solana.SigInfo
	txs      map[string]solana.TxEvents
	accounts map[string][]byte
	owners   map[string]map[string]float64 // owner+mint -> balance by token account
	touched  []string                      // token accounts every tx touches
	hops     map[string]bool               // sigs that leave the wallet's token balance unchanged
}

func (f fakeChain) OwnerTokenAccounts(_ context.Context, owner, mint string) (map[string]float64, error) {
	return f.owners[owner+mint], nil
}

func (f fakeChain) GetMultipleAccounts(_ context.Context, addrs []string) ([][]byte, error) {
	out := make([][]byte, len(addrs))
	for i, a := range addrs {
		out[i] = f.accounts[a]
	}
	return out, nil
}

func (f fakeChain) GetBalanceChange(_ context.Context, sig, owner, mint string) (solana.BalanceChange, error) {
	v, ok := f.after[sig]
	if !ok {
		return solana.BalanceChange{}, errors.New("not found")
	}
	if f.hops[sig] {
		return solana.BalanceChange{TokenAfter: v, TokenAccounts: f.touched}, nil // balance unchanged
	}
	return solana.BalanceChange{TokenAfter: v, TokenDelta: 1e9, TokenAccounts: f.touched}, nil
}
func (f fakeChain) GetTxEvents(_ context.Context, sig string) (solana.TxEvents, error) {
	tx, ok := f.txs[sig]
	if !ok {
		return solana.TxEvents{}, errors.New("not found")
	}
	return tx, nil
}
func (f fakeChain) GetSignaturesForAddress(context.Context, string, string, int) ([]solana.SigInfo, error) {
	return f.sigs, nil
}

func TestHeliusStreamsWalletTrades(t *testing.T) {
	up := websocket.Upgrader{}
	subscribed := make(chan string, 4)
	var mu sync.Mutex
	var conn *websocket.Conn
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		mu.Lock()
		conn = c
		mu.Unlock()
		for {
			var req struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
				Params []any  `json:"params"`
			}
			if c.ReadJSON(&req) != nil {
				return
			}
			if req.Method == "logsSubscribe" {
				m := req.Params[0].(map[string]any)["mentions"].([]any)[0].(string)
				c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": 77})
				subscribed <- m
			}
		}
	}))
	defer srv.Close()

	h := NewHelius("ws"+strings.TrimPrefix(srv.URL, "http"), fakeChain{after: map[string]float64{"BUY": 40e6, "SELL": 10e6}}, 0)
	h.SubscribeAccounts([]string{hWallet})
	got := make(chan model.Trade, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go h.Run(ctx, func(tr model.Trade) { got <- tr })
	if w := <-subscribed; w != hWallet {
		t.Fatalf("subscribed %s", w)
	}
	send := func(sig string, errv any, logs []string) {
		mu.Lock()
		defer mu.Unlock()
		conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "method": "logsNotification", "params": map[string]any{
			"subscription": 77, "result": map[string]any{"value": map[string]any{"signature": sig, "err": errv, "logs": logs}}}})
	}
	buy := history.Event{Mint: hMint, User: hWallet, IsBuy: true, SolAmount: 1.5, TokenAmount: 40e6, VSol: 40, VTokens: 800e6}
	sell := history.Event{Mint: hMint, User: hWallet, SolAmount: 1.2, TokenAmount: 30e6, VSol: 39, VTokens: 820e6}
	send("FAILED", map[string]any{"InstructionError": []any{0, "x"}}, pumpLogs(buy))   // failed tx: ignored
	send("SPAM", nil, []string{"Program 11111111111111111111111111111111 invoke [1]"}) // not pump: ignored
	send("BUY", nil, pumpLogs(buy))
	send("SELL", nil, pumpLogs(sell))
	b, s := <-got, <-got
	if b.Signature != "BUY" || b.NewTokenBalance != 40e6 || s.Signature != "SELL" || s.Side != model.SideSell || s.NewTokenBalance != 10e6 {
		t.Fatalf("buy %+v\nsell %+v", b, s)
	}
	// A partial sell must not look like a full exit: 30M sold of 40M held.
	if frac := sellFraction(s); frac < 0.74 || frac > 0.76 {
		t.Fatalf("sell fraction %.2f, want 0.75", frac)
	}
	if st := h.Stats(); !st.Connected || st.Messages != 2 || st.BytesToday == 0 {
		t.Fatalf("stats %+v", st)
	}
	_ = json.Marshal
}

// sellFraction mirrors sim.SellFraction (no import cycle in tests).
func sellFraction(t model.Trade) float64 {
	before := t.NewTokenBalance + t.TokenAmount
	return t.TokenAmount / before
}

func TestHeliusMovesNoisyWalletToQuietFeed(t *testing.T) {
	up := websocket.Upgrader{}
	reqs := make(chan string, 8)
	var mu sync.Mutex
	var conn *websocket.Conn
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		mu.Lock()
		conn = c
		mu.Unlock()
		for {
			var req struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if c.ReadJSON(&req) != nil {
				return
			}
			mu.Lock()
			switch req.Method {
			case "logsSubscribe":
				c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": 77})
			case "accountSubscribe":
				c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": 88})
			}
			mu.Unlock()
			reqs <- req.Method
		}
	}))
	defer srv.Close()

	buy := history.Event{Mint: hMint, User: hWallet, IsBuy: true, SolAmount: 1.5, TokenAmount: 40e6, VSol: 40, VTokens: 800e6}
	chain := fakeChain{
		after: map[string]float64{"BUY": 40e6},
		sigs: []solana.SigInfo{
			{Signature: "BOT", Slot: 5, Err: map[string]any{"InstructionError": 1}}, // other bots' failed txs
			{Signature: "BUY", Slot: 5},
			{Signature: "OLD", Slot: 4},
		},
		txs: map[string]solana.TxEvents{"BUY": {Signature: "BUY", Logs: pumpLogs(buy)},
			"OLD": {Signature: "OLD", Logs: pumpLogs(buy)}},
	}
	h := NewHelius("ws"+strings.TrimPrefix(srv.URL, "http"), chain, 0)
	h.WalletDailyMB = 0.001 // 1 KB
	noisy := make(chan string, 1)
	h.OnNoisy = func(w string, mb float64) { noisy <- w }
	h.SubscribeAccounts([]string{hWallet})
	got := make(chan model.Trade, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go h.Run(ctx, func(tr model.Trade) { got <- tr })
	expect := func(method string) {
		t.Helper()
		select {
		case m := <-reqs:
			if m != method {
				t.Fatalf("request %s, want %s", m, method)
			}
		case <-ctx.Done():
			t.Fatalf("no %s", method)
		}
	}
	notify := func(v any) {
		mu.Lock()
		defer mu.Unlock()
		conn.WriteJSON(v)
	}
	expect("logsSubscribe")
	junk := strings.Repeat("Program BevFQ2 failed ", 30)
	for i := 0; i < 3; i++ {
		notify(map[string]any{"jsonrpc": "2.0", "method": "logsNotification", "params": map[string]any{
			"subscription": 77, "result": map[string]any{"value": map[string]any{"signature": "S", "err": "x", "logs": []string{junk}}}}})
	}
	if w := <-noisy; w != hWallet {
		t.Fatalf("noisy %s", w)
	}
	expect("logsUnsubscribe")
	expect("accountSubscribe")
	if st := h.Stats(); st.Accounts != 1 || st.Quiet != 1 {
		t.Fatalf("wallet must stay tracked on the quiet feed: %+v", st)
	}
	// The wallet's balance changed in slot 5: only its own successful tx there counts.
	notify(map[string]any{"jsonrpc": "2.0", "method": "accountNotification", "params": map[string]any{
		"subscription": 88, "result": map[string]any{"context": map[string]any{"slot": 5}, "value": map[string]any{"lamports": 1}}}})
	select {
	case tr := <-got:
		if tr.Signature != "BUY" || tr.Side != model.SideBuy || tr.NewTokenBalance != 40e6 {
			t.Fatalf("quiet trade %+v", tr)
		}
	case <-ctx.Done():
		t.Fatal("quiet feed trade not delivered")
	}
	select {
	case tr := <-got:
		t.Fatalf("unexpected trade %+v", tr)
	case <-time.After(200 * time.Millisecond):
	}
}

// bust's real PumpSwap sell of Hiog5y (after it graduated) becomes a sell on pool
// "pumpswap", with the mint from the pool account. Before this, it was dropped.
func TestHeliusDecodesPumpSwapSell(t *testing.T) {
	raw, err := os.ReadFile("../pumpswap/testdata/mainnet.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Sell struct {
			Signature string   `json:"signature"`
			Logs      []string `json:"logs"`
		} `json:"sell"`
		PoolB64 string `json:"pool_b64"`
	}
	_ = json.Unmarshal(raw, &fx)
	poolData, _ := base64.StdEncoding.DecodeString(fx.PoolB64)
	const bust = "FzVQSzj8JJr6WMGqbUHzx2XH1KkrfxRrRPv6WcbbZmND"
	h := NewHelius("", fakeChain{after: map[string]float64{fx.Sell.Signature: 0},
		accounts: map[string][]byte{"4xMCDt7EuE1HDA6h4AF2F9xQWSBk1BXduj3y7eukkVby": poolData}}, 0)
	if !mentionsTrade(fx.Sell.Logs) {
		t.Fatal("a PumpSwap-only tx must pass the notification filter")
	}
	got := h.convert(context.Background(), note{wallet: bust, sig: fx.Sell.Signature, logs: fx.Sell.Logs, at: time.Unix(1792890588, 0)})
	if len(got) != 1 {
		t.Fatalf("trades %+v", got)
	}
	tr := got[0]
	if tr.Mint != "Hiog5ybjwPKa2bENQTyK9yNMEjUdUX9SStdRVRHLpump" || tr.Pool != model.PoolPumpSwap || tr.Side != model.SideSell ||
		tr.TokenAmount < 5740740 || tr.TokenAmount > 5740741 || tr.NewTokenBalance != 0 || tr.BondingCurveKey == "" {
		t.Fatalf("trade %+v", tr)
	}
	// Post-trade price includes the pool's virtual SOL (~17.58): (66.55 + 17.58) / 220.09M.
	if p := tr.Price(); p < 3.7e-7 || p > 3.9e-7 {
		t.Fatalf("price %.3e", p)
	}
	// Someone else's PumpSwap trade in the same tx is not ours.
	if other := h.convert(context.Background(), note{wallet: hWallet, sig: fx.Sell.Signature, logs: fx.Sell.Logs, at: time.Now()}); len(other) != 0 {
		t.Fatalf("attributed to the wrong wallet: %+v", other)
	}
}

// Multi-pool routes (token -> USDC -> SOL) emit an event on a USDC/SOL pool too;
// only a graduated token's own pool counts as a trade.
func TestPumpSwapIgnoresNonCanonicalPool(t *testing.T) {
	raw, _ := os.ReadFile("../pumpswap/testdata/mainnet.json")
	var fx struct {
		Sell struct {
			Signature string   `json:"signature"`
			Logs      []string `json:"logs"`
		} `json:"sell"`
		PoolB64 string `json:"pool_b64"`
	}
	_ = json.Unmarshal(raw, &fx)
	poolData, _ := base64.StdEncoding.DecodeString(fx.PoolB64)
	usdc, _ := base58.Decode("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	copy(poolData[43:75], usdc)
	h := NewHelius("", fakeChain{after: map[string]float64{fx.Sell.Signature: 0},
		accounts: map[string][]byte{"4xMCDt7EuE1HDA6h4AF2F9xQWSBk1BXduj3y7eukkVby": poolData}}, 0)
	got := h.convert(context.Background(), note{wallet: "FzVQSzj8JJr6WMGqbUHzx2XH1KkrfxRrRPv6WcbbZmND", sig: fx.Sell.Signature, logs: fx.Sell.Logs, at: time.Now()})
	if len(got) != 0 {
		t.Fatalf("routing hop recorded as a trade: %+v", got)
	}
}

// King-Solomon bought token B through A's pool (SOL -> A -> B): the A legs leave
// his A balance unchanged and must not be copied as an A buy and sell.
func TestRoutingHopThroughPumpTokenIsDropped(t *testing.T) {
	raw, _ := os.ReadFile("../pumpswap/testdata/mainnet.json")
	var fx struct {
		Sell struct {
			Signature string   `json:"signature"`
			Logs      []string `json:"logs"`
		} `json:"sell"`
		PoolB64 string `json:"pool_b64"`
	}
	_ = json.Unmarshal(raw, &fx)
	poolData, _ := base64.StdEncoding.DecodeString(fx.PoolB64)
	h := NewHelius("", fakeChain{after: map[string]float64{fx.Sell.Signature: 5e6}, hops: map[string]bool{fx.Sell.Signature: true},
		accounts: map[string][]byte{"4xMCDt7EuE1HDA6h4AF2F9xQWSBk1BXduj3y7eukkVby": poolData}}, 0)
	got := h.convert(context.Background(), note{wallet: "FzVQSzj8JJr6WMGqbUHzx2XH1KkrfxRrRPv6WcbbZmND", sig: fx.Sell.Signature, logs: fx.Sell.Logs, at: time.Now()})
	if len(got) != 0 {
		t.Fatalf("routing hop recorded as a trade: %+v", got)
	}
}

// theo keeps one token in two accounts: a sell that empties one of them must not
// read as a full exit (our copy would sell everything instead of a third).
func TestSellBalanceCountsAllAccounts(t *testing.T) {
	sell := model.Trade{Signature: "S", Trader: hWallet, Mint: hMint, Side: model.SideSell, TokenAmount: 29.8e6}
	h := NewHelius("", fakeChain{after: map[string]float64{"S": 0}, touched: []string{"A"},
		owners: map[string]map[string]float64{hWallet + hMint: {"A": 0, "B": 61.4e6}}}, 0)
	if got, _ := h.balanceAfter(context.Background(), sell); got != 61.4e6 {
		t.Fatalf("balance after %v, want 61.4M held in the other account", got)
	}
	// A real full exit stays a full exit.
	h = NewHelius("", fakeChain{after: map[string]float64{"S": 0}, touched: []string{"A"}}, 0)
	if got, _ := h.balanceAfter(context.Background(), sell); got != 0 {
		t.Fatalf("balance after %v, want 0", got)
	}
	// The RPC can still show the sold account's pre-sell balance (CXVJew): the tx
	// is the truth for accounts it touched, so this is still a full exit.
	h = NewHelius("", fakeChain{after: map[string]float64{"S": 0}, touched: []string{"A"},
		owners: map[string]map[string]float64{hWallet + hMint: {"A": 29.8e6}}}, 0)
	if got, _ := h.balanceAfter(context.Background(), sell); got != 0 {
		t.Fatalf("balance after %v, want 0: a stale read of the sold account must not count", got)
	}
}

// nad's real trades on curves priced in another token (captured November 2026):
// the TradeEvent's sol_amount and SOL reserves are 0 and the size is in the
// extended quote fields. The trade stays pool "nonsol" (never
// copied or simulated), VSol stays 0, and SolAmount carries the quote amount in raw units / 1e9,
// the scale PumpSwap trades use. A SOL curve trade is unchanged.
func TestNonSOLCurveTradesRecordQuoteAmount(t *testing.T) {
	raw, err := os.ReadFile("testdata/nonsol.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx map[string]struct {
		Signature string   `json:"signature"`
		BlockTime int64    `json:"block_time"`
		Logs      []string `json:"logs"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	const nad = "363sqMFaxZgvCoGzxKjXe1BqMGYkSVoCwmghZUndXuaT"
	for _, c := range []struct {
		name, side string
		tokens     float64
		quoteRaw   uint64
	}{
		{"sell", model.SideSell, 9048876.509232, 62853244},
		{"buy_exact_quote_in", model.SideBuy, 1808512.492443, 158346},
		{"multi_hop_swap", model.SideSell, 20629191.436820, 2226220788527},
	} {
		tx := fx[c.name]
		got, _ := TradesFromLogs(tx.Signature, nad, tx.Logs, time.Unix(tx.BlockTime, 0))
		if len(got) != 1 {
			t.Fatalf("%s: trades %+v", c.name, got)
		}
		tr := got[0]
		if tr.Pool != "nonsol" || tr.VSol != 0 || tr.MarketCapSol != 0 || tr.Side != c.side ||
			model.Priced(tr.Pool) || tr.TokenAmount < c.tokens-1e-6 || tr.TokenAmount > c.tokens+1e-6 {
			t.Fatalf("%s: trade %+v", c.name, tr)
		}
		if want := float64(c.quoteRaw) / 1e9; tr.SolAmount != want {
			t.Fatalf("%s: quote amount %v, want %v", c.name, tr.SolAmount, want)
		}
	}
	evs := history.EventsFromTx(solana.TxEvents{Logs: fx["sell"].Logs})
	if len(evs) != 1 || evs[0].QuoteMint == "" || evs[0].SolAmount != 0 || evs[0].VQuote != 4.917197284 {
		t.Fatalf("event %+v", evs)
	}

	sol := fx["sol_buy"]
	got, _ := TradesFromLogs(sol.Signature, nad, sol.Logs, time.Unix(sol.BlockTime, 0))
	if len(got) != 1 || got[0].Pool != "pump" || got[0].SolAmount != 1.466666666 || got[0].VSol != 102.483779137 {
		t.Fatalf("SOL curve trade changed: %+v", got)
	}
	if evs := history.EventsFromTx(solana.TxEvents{Logs: sol.Logs}); len(evs) != 1 || evs[0].QuoteMint != "" || evs[0].QuoteAmount != evs[0].SolAmount {
		t.Fatalf("SOL event %+v", evs)
	}
}
