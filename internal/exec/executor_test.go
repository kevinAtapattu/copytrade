package exec

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mr-tron/base58"

	"copytrade/internal/bus"
	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/price"
	"copytrade/internal/solana"
	"copytrade/internal/store"
)

var t0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

func setup(t *testing.T, r Risk) (*Executor, *store.Memory) {
	st := store.NewMemory()
	ctx := context.Background()
	st.AddWallet(ctx, model.Wallet{Address: "LIVE", Status: model.StatusLive})
	st.AddWallet(ctx, model.Wallet{Address: "BENCH", Status: model.StatusBench})
	ex := New(Paper{Fees: curve.Fees{PumpPct: 1.25, BotPct: 0.5, NetworkSol: 0.0005}}, st, bus.NewMemory(), price.Fixed(150), r)
	ex.Sync = true
	if err := ex.Load(ctx, t0); err != nil {
		t.Fatal(err)
	}
	return ex, st
}

func trd(sig, trader, mint, side string, vsol float64, at time.Duration, target bool, tokAmt, newBal float64) model.Trade {
	st := curve.State{VSol: vsol, VTokens: 32.19e9 / vsol}
	return model.Trade{Signature: sig, Mint: mint, Trader: trader, Side: side, VSol: st.VSol, VTokens: st.VTokens,
		Pool: "pump", ReceivedAt: t0.Add(at), IsTarget: target, TokenAmount: tokAmt, NewTokenBalance: newBal}
}

func fills(st *store.Memory) map[string]int {
	fs, _ := st.ListLiveFills(context.Background(), store.LiveFilter{})
	m := map[string]int{}
	for _, f := range fs {
		m[f.Side+":"+f.Status]++
	}
	return m
}

func TestCopiesOnlyLiveWallets(t *testing.T) {
	ex, st := setup(t, Risk{SizeUSD: 25, MaxOpenPositions: 5})
	ctx := context.Background()
	ex.HandleTrade(ctx, trd("b1", "BENCH", "M1", "buy", 31, 0, true, 1e6, 1e6))
	ex.HandleTrade(ctx, trd("l1", "LIVE", "M1", "buy", 32, time.Second, true, 1e6, 1e6))
	ex.HandleTrade(ctx, trd("l2", "LIVE", "M1", "buy", 33, 2*time.Second, true, 1e6, 2e6)) // already in: ignored
	if f := fills(st); f["buy:filled"] != 1 || len(f) != 1 {
		t.Fatalf("fills %v", f)
	}
	ex.HandleTrade(ctx, trd("l3", "LIVE", "M1", "sell", 40, time.Minute, true, 1e6, 1e6)) // sold half
	ps, _ := st.ListLivePositions(ctx, store.LiveFilter{})
	if len(ps) != 1 || ps[0].Status != model.PosOpen {
		t.Fatalf("positions %+v", ps)
	}
	held := ps[0].TokensHeld
	ex.HandleTrade(ctx, trd("l4", "LIVE", "M1", "sell", 45, 2*time.Minute, true, 1e6, 0))
	ps, _ = st.ListLivePositions(ctx, store.LiveFilter{})
	if ps[0].Status != model.PosClosed || ps[0].ProceedsSol <= ps[0].CostSol || held <= 0 {
		t.Fatalf("should close at a profit: %+v", ps[0])
	}
	if ex.OpenCount() != 0 {
		t.Fatal("open count")
	}
}

func TestKillSwitchBlocksBuysNotSells(t *testing.T) {
	ex, st := setup(t, Risk{SizeUSD: 25, MaxOpenPositions: 5})
	ctx := context.Background()
	ex.HandleTrade(ctx, trd("a", "LIVE", "M1", "buy", 31, 0, true, 1e6, 1e6))
	st.SetSetting(ctx, store.SettingKillSwitch, "on")
	ex.Refresh(ctx)
	ex.HandleTrade(ctx, trd("b", "LIVE", "M2", "buy", 31, time.Second, true, 1e6, 1e6))
	ex.HandleTrade(ctx, trd("c", "LIVE", "M1", "sell", 33, time.Minute, true, 1e6, 0))
	f := fills(st)
	if f["buy:filled"] != 1 || f["buy:rejected"] != 1 || f["sell:filled"] != 1 {
		t.Fatalf("fills %v", f)
	}
}

func TestMaxOpenPositions(t *testing.T) {
	ex, st := setup(t, Risk{SizeUSD: 25, MaxOpenPositions: 2})
	ctx := context.Background()
	for i, m := range []string{"M1", "M2", "M3"} {
		ex.HandleTrade(ctx, trd("s"+m, "LIVE", m, "buy", 31, time.Duration(i)*time.Second, true, 1e6, 1e6))
	}
	if f := fills(st); f["buy:filled"] != 2 || f["buy:rejected"] != 1 {
		t.Fatalf("fills %v", f)
	}
}

func TestStopLoss(t *testing.T) {
	ex, st := setup(t, Risk{SizeUSD: 25, MaxOpenPositions: 5, StopLossPct: 40})
	ctx := context.Background()
	ex.HandleTrade(ctx, trd("a", "LIVE", "M1", "buy", 40, 0, true, 1e6, 1e6))
	ex.HandleTrade(ctx, trd("x", "OTHER", "M1", "sell", 36, time.Second, false, 1e6, 0)) // -19% price: hold
	if f := fills(st); f["sell:filled"] != 0 {
		t.Fatalf("stopped too early %v", f)
	}
	ex.HandleTrade(ctx, trd("y", "OTHER", "M1", "sell", 30, 2*time.Second, false, 1e6, 0)) // -44% price
	fs, _ := st.ListLiveFills(ctx, store.LiveFilter{})
	if fs[0].Side != "sell" || fs[0].Reason != "stop_loss" || fs[0].Status != "filled" {
		t.Fatalf("expected stop-loss sell, got %+v", fs[0])
	}
	if ex.OpenCount() != 0 {
		t.Fatal("stop-loss should close the position")
	}
}

func TestDailyLossLimit(t *testing.T) {
	ex, st := setup(t, Risk{SizeUSD: 25, MaxOpenPositions: 5, DailyLossUSD: 10})
	ctx := context.Background()
	ex.HandleTrade(ctx, trd("a", "LIVE", "M1", "buy", 50, 0, true, 1e6, 1e6))
	ex.HandleTrade(ctx, trd("b", "LIVE", "M1", "sell", 31, time.Minute, true, 1e6, 0)) // big loss
	ex.HandleTrade(ctx, trd("c", "LIVE", "M2", "buy", 31, 2*time.Minute, true, 1e6, 1e6))
	fs, _ := st.ListLiveFills(ctx, store.LiveFilter{})
	if fs[0].Status != "rejected" || !strings.Contains(fs[0].Error, "daily loss") {
		t.Fatalf("expected daily loss rejection, got %+v", fs[0])
	}
	// next UTC day resets
	ex.HandleTrade(ctx, trd("d", "LIVE", "M3", "buy", 31, 24*time.Hour, true, 1e6, 1e6))
	fs, _ = st.ListLiveFills(ctx, store.LiveFilter{})
	if fs[0].Status != "filled" {
		t.Fatalf("new day should allow buys: %+v", fs[0])
	}
}

// ---- live trader against fake PumpPortal + RPC

func unsignedTx(signer []byte) []byte {
	msg := []byte{0x80, 1, 0, 1, 2}
	msg = append(msg, signer...)
	msg = append(msg, make([]byte, 32)...)
	msg = append(msg, make([]byte, 32)...) // blockhash
	msg = append(msg, 0, 0)
	tx := append([]byte{1}, make([]byte, 64)...)
	return append(tx, msg...)
}

func TestLiveTraderBuildSignSendConfirm(t *testing.T) {
	pk := ed25519.NewKeyFromSeed(make([]byte, 32))
	kp, _ := solana.ParseKeypair(base58.Encode(pk))
	owner := kp.PublicBase58()

	var builds, sends atomic.Int32
	var reqBody map[string]any
	pp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		builds.Add(1)
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &reqBody)
		w.Write(unsignedTx(kp.Public))
	}))
	defer pp.Close()
	var sentSig string
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		reply := func(v any) { json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": v}) }
		switch req.Method {
		case "sendTransaction":
			sends.Add(1)
			raw, _ := base64.StdEncoding.DecodeString(req.Params[0].(string))
			tx, err := solana.ParseTx(raw)
			sig, _ := base58.Decode(tx.Signature())
			if err != nil || !ed25519.Verify(kp.Public, tx.Message(), sig) {
				t.Errorf("bad signature sent")
			}
			sentSig = tx.Signature()
			reply(sentSig)
		case "getSignatureStatuses":
			reply(map[string]any{"value": []any{map[string]any{"err": nil, "confirmationStatus": "confirmed"}}})
		case "getTransaction":
			reply(map[string]any{"blockTime": 1, "meta": map[string]any{
				"fee": 5000, "preBalances": []uint64{1_000_000_000}, "postBalances": []uint64{833_000_000},
				"preTokenBalances":  []any{},
				"postTokenBalances": []any{map[string]any{"mint": "MINT", "owner": owner, "uiTokenAmount": map[string]any{"uiAmountString": "4500000"}}},
			}, "transaction": map[string]any{"message": map[string]any{"accountKeys": []any{map[string]any{"pubkey": owner}}}}})
		}
	}))
	defer rpc.Close()

	l := &Live{Key: kp, RPC: solana.NewRPC(rpc.URL), TradeURL: pp.URL, SlippagePct: 15, PriorityFee: 0.0005,
		MaxAttempts: 3, HTTP: http.DefaultClient}
	res, err := l.Execute(context.Background(), Order{Mint: "MINT", Side: "buy", SolIn: 0.1667})
	if err != nil {
		t.Fatal(err)
	}
	if res.Tokens != 4_500_000 || res.SolAmount < 0.1669 || res.SolAmount > 0.1671 || res.Signature != sentSig || res.Attempts != 1 {
		t.Fatalf("result %+v", res)
	}
	if reqBody["action"] != "buy" || reqBody["denominatedInSol"] != "true" || reqBody["publicKey"] != owner || reqBody["pool"] != "auto" {
		t.Fatalf("trade-local request %+v", reqBody)
	}
	if builds.Load() != 1 || sends.Load() != 1 {
		t.Fatalf("builds %d sends %d", builds.Load(), sends.Load())
	}
	// sells use percentage form
	r := l.request(Order{Side: "sell", SellPct: 37.5})
	if r.Amount != "37.50%" || r.DenominatedInSol != "false" {
		t.Fatalf("sell request %+v", r)
	}
	l.JitoTipSol = 0.001
	if r := l.request(Order{Side: "buy", SolIn: 0.1}); r.PriorityFee != 0.001 {
		t.Fatal("jito tip should be sent as priorityFee")
	}
}

func TestLiveRetriesOnOnchainFailure(t *testing.T) {
	pk := ed25519.NewKeyFromSeed(make([]byte, 32))
	kp, _ := solana.ParseKeypair(base58.Encode(pk))
	var builds atomic.Int32
	pp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		builds.Add(1)
		w.Write(unsignedTx(kp.Public))
	}))
	defer pp.Close()
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Method string }
		json.NewDecoder(r.Body).Decode(&req)
		res := any("sig")
		if req.Method == "getSignatureStatuses" {
			res = map[string]any{"value": []any{map[string]any{"err": map[string]any{"InstructionError": []any{0, "Custom"}}}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": res})
	}))
	defer rpc.Close()
	l := &Live{Key: kp, RPC: solana.NewRPC(rpc.URL), TradeURL: pp.URL, MaxAttempts: 3, HTTP: http.DefaultClient}
	_, err := l.Execute(context.Background(), Order{Mint: "M", Side: "buy", SolIn: 0.1})
	if err == nil || !strings.Contains(err.Error(), "failed on-chain") || builds.Load() != 3 {
		t.Fatalf("err %v builds %d (want 3 fresh builds)", err, builds.Load())
	}
}

func TestLiveTransportErrorWaitsForExpiryBeforeRetry(t *testing.T) {
	pk := ed25519.NewKeyFromSeed(make([]byte, 32))
	kp, _ := solana.ParseKeypair(base58.Encode(pk))
	var builds, validChecks atomic.Int32
	pp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		builds.Add(1)
		w.Write(unsignedTx(kp.Public))
	}))
	defer pp.Close()
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Method string }
		json.NewDecoder(r.Body).Decode(&req)
		var res any
		switch req.Method {
		case "sendTransaction":
			// transport failure: hang up without a JSON-RPC answer
			hj, _ := w.(http.Hijacker)
			c, _, _ := hj.Hijack()
			c.Close()
			return
		case "getSignatureStatuses":
			res = map[string]any{"value": []any{nil}} // never lands
		case "isBlockhashValid":
			// valid for the first two checks, then expired
			res = map[string]any{"value": validChecks.Add(1) <= 2}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": res})
	}))
	defer rpc.Close()
	l := &Live{Key: kp, RPC: solana.NewRPC(rpc.URL), TradeURL: pp.URL, MaxAttempts: 2, HTTP: http.DefaultClient,
		ConfirmWait: 50 * time.Millisecond}
	_, err := l.Execute(context.Background(), Order{Mint: "M", Side: "buy", SolIn: 0.1})
	if err == nil {
		t.Fatal("expected failure")
	}
	// First attempt must wait until the blockhash expired (2 valid checks) before the
	// second build; a naive retry would rebuild immediately.
	if builds.Load() != 2 || validChecks.Load() < 3 {
		t.Fatalf("builds %d, blockhash checks %d", builds.Load(), validChecks.Load())
	}
}

// Take profit: sell half now, then the wallet's own full exit sells the rest.
func TestManualSellTakesProfitThenCopyFinishes(t *testing.T) {
	ex, st := setup(t, Risk{SizeUSD: 25, MaxOpenPositions: 5})
	ctx := context.Background()
	ex.HandleTrade(ctx, trd("l1", "LIVE", "M1", "buy", 32, 0, true, 1e6, 1e6))
	ex.HandleTrade(ctx, trd("px", "OTHER", "M1", "buy", 60, time.Minute, false, 1e6, 1e6)) // price up
	ps, _ := st.ListLivePositions(ctx, store.LiveFilter{})
	bought := ps[0].TokensHeld
	if err := ex.ManualSell(ctx, ps[0].ID, 50, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	ps, _ = st.ListLivePositions(ctx, store.LiveFilter{})
	if ps[0].Status != model.PosOpen || ps[0].TokensHeld > bought*0.51 || ps[0].TokensHeld < bought*0.49 || ps[0].ProceedsSol <= 0 {
		t.Fatalf("after half sell %+v", ps[0])
	}
	if f := fills(st); f["sell:filled"] != 1 {
		t.Fatalf("fills %v", f)
	}
	ex.HandleTrade(ctx, trd("l2", "LIVE", "M1", "sell", 60, 3*time.Minute, true, 1e6, 0)) // wallet exits
	ps, _ = st.ListLivePositions(ctx, store.LiveFilter{})
	if ps[0].Status != model.PosClosed || ps[0].ProceedsSol <= ps[0].CostSol {
		t.Fatalf("should close at a profit: %+v", ps[0])
	}
	// Nothing left to sell: manual and copy sells are no-ops.
	if err := ex.ManualSell(ctx, ps[0].ID, 100, t0.Add(4*time.Minute)); err == nil {
		t.Fatal("sold a closed position")
	}
	ex.HandleTrade(ctx, trd("l3", "LIVE", "M1", "sell", 60, 5*time.Minute, true, 1e6, 0))
	if f := fills(st); f["sell:filled"] != 2 {
		t.Fatalf("fills %v", f)
	}
	if err := ex.ManualSell(ctx, ps[0].ID, 150, t0); err == nil {
		t.Fatal("accepted 150%")
	}
}

// PumpPortal prices PumpSwap buys without the pool's virtual reserve, so buys on
// graduated tokens go through Jupiter; everything else stays on PumpPortal.
func TestLiveBuysGraduatedTokensThroughJupiter(t *testing.T) {
	pk := ed25519.NewKeyFromSeed(make([]byte, 32))
	kp, _ := solana.ParseKeypair(base58.Encode(pk))
	var pumpportal atomic.Int32
	pp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pumpportal.Add(1)
		w.Write(unsignedTx(kp.Public))
	}))
	defer pp.Close()
	var quote url.Values
	var swap map[string]any
	jup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/quote":
			quote = r.URL.Query()
			w.Write([]byte(`{"outAmount":"123"}`))
		case "/swap":
			json.NewDecoder(r.Body).Decode(&swap)
			json.NewEncoder(w).Encode(map[string]string{"swapTransaction": base64.StdEncoding.EncodeToString(unsignedTx(kp.Public))})
		}
	}))
	defer jup.Close()
	l := &Live{Key: kp, TradeURL: pp.URL, JupiterURL: jup.URL, SlippagePct: 15, PriorityFee: 0.0005, HTTP: http.DefaultClient}
	owner := kp.PublicBase58()
	ctx := context.Background()

	if _, err := l.build(ctx, Order{Mint: "MINT", Side: "buy", SolIn: 0.25, State: curve.State{VSol: 30, VTokens: 1e9, AMM: true}}, owner); err != nil {
		t.Fatal(err)
	}
	if pumpportal.Load() != 0 || quote.Get("outputMint") != "MINT" || quote.Get("amount") != "250000000" || quote.Get("slippageBps") != "1500" ||
		swap["userPublicKey"] != owner || swap["quoteResponse"] == nil {
		t.Fatalf("graduated buy: pumpportal %d quote %v swap %v", pumpportal.Load(), quote, swap)
	}
	if !l.jupiter(Order{Side: "buy", State: curve.State{AMM: true}}) || l.jupiter(Order{Side: "sell", State: curve.State{AMM: true}}) {
		t.Fatal("only graduated buys use jupiter")
	}
	for _, o := range []Order{
		{Mint: "MINT", Side: "buy", SolIn: 0.25, State: curve.State{VSol: 30, VTokens: 1e9}},              // bonding curve buy
		{Mint: "MINT", Side: "sell", SellPct: 100, State: curve.State{VSol: 30, VTokens: 1e9, AMM: true}}, // graduated sell
	} {
		if _, err := l.build(ctx, o, owner); err != nil {
			t.Fatal(err)
		}
	}
	if pumpportal.Load() != 2 {
		t.Fatalf("pumpportal builds %d, want 2", pumpportal.Load())
	}
}
