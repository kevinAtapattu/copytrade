package history

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mr-tron/base58"

	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/solana"
)

const (
	mint = "So11111111111111111111111111111111111111112"
	user = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
)

func ev(buy bool, sol float64, ts int64) Event {
	return Event{Mint: mint, SolAmount: sol, TokenAmount: 1_000_000, IsBuy: buy, User: user, Timestamp: ts, VSol: 31.5, VTokens: 1.02e9}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	e := ev(true, 0.25, 1_790_000_000)
	got, err := DecodeEvent(EncodeEvent(e)[8:])
	if err != nil || got != e {
		t.Fatalf("got %+v err %v", got, err)
	}
}

func TestEventsFromLogsAndInnerInstructions(t *testing.T) {
	buy := EncodeEvent(ev(true, 0.25, 100))
	sell := EncodeEvent(ev(false, 0.4, 200))
	emitCPI := append([]byte{0xe4, 0x45, 0xa5, 0x2e, 0x51, 0xcb, 0x9a, 0x1d}, sell...)
	tx := solana.TxEvents{
		Signature: "sig",
		Logs: []string{"Program log: Instruction: Buy", "Program data: " + base64.StdEncoding.EncodeToString(buy),
			"Program data: " + base64.StdEncoding.EncodeToString([]byte("not an event"))},
		InnerData: [][]byte{buy, emitCPI, {1, 2, 3}}, // buy appears twice: must dedupe
	}
	evs := EventsFromTx(tx)
	if len(evs) != 2 || !evs[0].IsBuy || evs[1].IsBuy || evs[1].SolAmount != 0.4 {
		t.Fatalf("events %+v", evs)
	}
	trades := ToTrades(tx, mint, "CURVE")
	if len(trades) != 2 || trades[0].Side != model.SideBuy || trades[0].Trader != user || !trades[0].ReceivedAt.Equal(time.Unix(100, 0).UTC()) {
		t.Fatalf("trades %+v", trades)
	}
	if len(ToTrades(tx, "OtherMint1111111111111111111111111111111111", "C")) != 0 {
		t.Fatal("other mints must be filtered")
	}
}

func TestFetcherWalksBackAndDecodesEarliest(t *testing.T) {
	// 2500 signatures newest-first across 3 pages; we want the oldest MaxTx.
	sigs := make([]string, 2500)
	for i := range sigs {
		sigs[i] = base58.Encode([]byte{byte(i >> 8), byte(i)}) // index 0 = newest
	}
	var txCalls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var res any
		switch req.Method {
		case "getSignaturesForAddress":
			opts := req.Params[1].(map[string]any)
			start := 0
			if b, ok := opts["before"].(string); ok {
				for i, s := range sigs {
					if s == b {
						start = i + 1
					}
				}
			}
			end := min(start+1000, len(sigs))
			var page []map[string]any
			for _, s := range sigs[start:end] {
				page = append(page, map[string]any{"signature": s, "err": nil})
			}
			res = page
		case "getTransaction":
			sig := req.Params[0].(string)
			txCalls = append(txCalls, sig)
			data := base64.StdEncoding.EncodeToString(EncodeEvent(ev(true, 0.1, 50)))
			res = map[string]any{"blockTime": 50, "meta": map[string]any{"err": nil,
				"logMessages": []string{"Program data: " + data}, "innerInstructions": []any{}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": res})
	}))
	defer srv.Close()
	f := Fetcher{RPC: solana.NewRPC(srv.URL), MaxPages: 5, MaxTx: 3}
	trades, err := f.TokenTrades(context.Background(), model.Token{Mint: mint, CurveKey: "CURVE"})
	if err != nil {
		t.Fatal(err)
	}
	if len(trades) != 3 || len(txCalls) != 3 {
		t.Fatalf("trades %d tx calls %d", len(trades), len(txCalls))
	}
	if txCalls[0] != sigs[2499] || txCalls[2] != sigs[2497] {
		t.Fatalf("should decode the oldest signatures first, got %v", txCalls)
	}
}

func TestFetcherSkipsMigratedTokenWhenLaunchUnreachable(t *testing.T) {
	var txCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		var res any
		switch req.Method {
		case "getSignaturesForAddress": // always a full page: history is longer than we walk
			page := make([]map[string]any, 1000)
			for i := range page {
				page[i] = map[string]any{"signature": base58.Encode([]byte{byte(i >> 8), byte(i)}), "err": nil}
			}
			res = page
		case "getTransaction":
			txCalls++
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": res})
	}))
	defer srv.Close()
	f := Fetcher{RPC: solana.NewRPC(srv.URL), MaxPages: 2, MaxTx: 10}
	trades, err := f.TokenTrades(context.Background(), model.Token{Mint: mint, CurveKey: "CURVE", Migrated: true})
	if err != nil || len(trades) != 0 || txCalls != 0 {
		t.Fatalf("want skip with no tx calls, got %d trades, %d calls, err %v", len(trades), txCalls, err)
	}
}

func TestEventsOnlyFromPumpProgram(t *testing.T) {
	const router = "term9YPb9mzAsABaqN71A4xdbxHmpBNZavpBiQKZzN3"
	real := EncodeEvent(ev(true, 1.25, 300))
	junk := EncodeEvent(Event{Mint: mint, User: user, IsBuy: true, Timestamp: 300}) // a router's own "TradeEvent": same discriminator, zero amounts
	b64 := base64.StdEncoding.EncodeToString
	tx := solana.TxEvents{
		Logs: []string{
			"Program " + router + " invoke [1]",
			"Program " + curve.PumpProgram + " invoke [2]",
			"Program data: " + b64(real),
			"Program " + curve.PumpProgram + " success",
			"Program data: " + b64(junk), // emitted by the router frame
			"Program " + router + " success",
		},
		InnerData:     [][]byte{junk, append([]byte{0xe4, 0x45, 0xa5, 0x2e, 0x51, 0xcb, 0x9a, 0x1d}, real...)},
		InnerPrograms: []string{router, curve.PumpProgram},
	}
	evs := EventsFromTx(tx)
	if len(evs) != 1 || evs[0].SolAmount != 1.25 {
		t.Fatalf("want only the pump event, got %+v", evs)
	}
}
