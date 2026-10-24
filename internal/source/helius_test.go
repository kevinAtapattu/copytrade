package source

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

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

type fakeChain struct{ after map[string]float64 }

func (f fakeChain) GetBalanceChange(_ context.Context, sig, owner, mint string) (solana.BalanceChange, error) {
	v, ok := f.after[sig]
	if !ok {
		return solana.BalanceChange{}, errors.New("not found")
	}
	return solana.BalanceChange{TokenAfter: v}, nil
}
func (f fakeChain) GetTxEvents(context.Context, string) (solana.TxEvents, error) {
	return solana.TxEvents{}, errors.New("unused")
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

func TestHeliusUnsubscribesNoisyWallet(t *testing.T) {
	up := websocket.Upgrader{}
	subscribed := make(chan struct{}, 1)
	unsubscribed := make(chan struct{}, 1)
	var mu sync.Mutex
	var conn *websocket.Conn
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		for {
			var req struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			if c.ReadJSON(&req) != nil {
				return
			}
			switch req.Method {
			case "logsSubscribe":
				mu.Lock()
				conn = c
				c.WriteJSON(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": 77})
				mu.Unlock()
				subscribed <- struct{}{}
			case "logsUnsubscribe":
				unsubscribed <- struct{}{}
			}
		}
	}))
	defer srv.Close()

	h := NewHelius("ws"+strings.TrimPrefix(srv.URL, "http"), fakeChain{}, 0)
	h.WalletDailyMB = 0.001 // 1 KB
	noisy := make(chan string, 1)
	h.OnNoisy = func(w string, mb float64) { noisy <- w }
	h.SubscribeAccounts([]string{hWallet})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	go h.Run(ctx, func(model.Trade) {})
	<-subscribed
	junk := strings.Repeat("Program BevFQ2 failed ", 30)
	for i := 0; i < 3; i++ {
		mu.Lock()
		conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "method": "logsNotification", "params": map[string]any{
			"subscription": 77, "result": map[string]any{"value": map[string]any{"signature": "S", "err": "x", "logs": []string{junk}}}}})
		mu.Unlock()
	}
	select {
	case w := <-noisy:
		if w != hWallet {
			t.Fatalf("noisy %s", w)
		}
	case <-ctx.Done():
		t.Fatal("noisy wallet not reported")
	}
	select {
	case <-unsubscribed:
	case <-ctx.Done():
		t.Fatal("noisy wallet not unsubscribed")
	}
	if st := h.Stats(); st.Accounts != 0 {
		t.Fatalf("still tracking %d accounts", st.Accounts)
	}
}
