package exec

import (
	"bytes"
	"context"
	"copytrade/internal/alert"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
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
	if fs[0].Status != "rejected" || !strings.Contains(fs[0].Error, "daily limit") {
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

	if _, _, err := l.build(ctx, Order{Mint: "MINT", Side: "buy", SolIn: 0.25, State: curve.State{VSol: 30, VTokens: 1e9, AMM: true}}, owner); err != nil {
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
		if _, _, err := l.build(ctx, o, owner); err != nil {
			t.Fatal(err)
		}
	}
	if pumpportal.Load() != 2 {
		t.Fatalf("pumpportal builds %d, want 2", pumpportal.Load())
	}
}

type recorder struct {
	Paper
	orders []Order
}

func (r *recorder) Execute(ctx context.Context, o Order) (Result, error) {
	r.orders = append(r.orders, o)
	return r.Paper.Execute(ctx, o)
}

// Two live wallets buy the same token: our balance holds both copies, so a sell
// for one must be its exact token count, not a percent that would sell both.
func TestSharedTokenSellsExactAmount(t *testing.T) {
	ex, st := setup(t, Risk{SizeUSD: 25, MaxOpenPositions: 5})
	ctx := context.Background()
	st.AddWallet(ctx, model.Wallet{Address: "LIVE2", Status: model.StatusLive})
	rec := &recorder{Paper: ex.Trader.(Paper)}
	ex.Trader = rec
	if err := ex.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	ex.HandleTrade(ctx, trd("a1", "LIVE", "M1", "buy", 32, 0, true, 1e6, 1e6))
	ex.HandleTrade(ctx, trd("b1", "LIVE2", "M1", "buy", 33, time.Second, true, 1e6, 1e6))
	ex.HandleTrade(ctx, trd("a2", "LIVE", "M1", "sell", 40, time.Minute, true, 1e6, 0))
	last := rec.orders[len(rec.orders)-1]
	if last.Side != model.SideSell || last.SellPct != 0 || last.Tokens <= 0 {
		t.Fatalf("shared sell %+v", last)
	}
	ex.HandleTrade(ctx, trd("b2", "LIVE2", "M1", "sell", 41, 2*time.Minute, true, 1e6, 0))
	if last := rec.orders[len(rec.orders)-1]; last.SellPct != 100 {
		t.Fatalf("last copy should sell 100%% of the balance: %+v", last)
	}
	if r := (&Live{}).request(Order{Side: "sell", Tokens: 1234.5678919}); r.Amount != "1234.567891" || r.DenominatedInSol != "false" {
		t.Fatalf("exact sell request %+v", r)
	}
}

// A buy with a deadline gets a blockhash that expires at the deadline (so it lands
// in time or never), and once the deadline has passed nothing more is sent.
func TestLiveBuyDeadline(t *testing.T) {
	pk := ed25519.NewKeyFromSeed(make([]byte, 32))
	kp, _ := solana.ParseKeypair(base58.Encode(pk))
	old := base58.Encode(bytes.Repeat([]byte{7}, 32))
	var builds atomic.Int32
	pp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		builds.Add(1)
		w.Write(unsignedTx(kp.Public))
	}))
	defer pp.Close()
	var sentHash string
	var blockSlot float64
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Method string `json:"method"`
			Params []any  `json:"params"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		reply := func(v any) { json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": v}) }
		switch req.Method {
		case "getSlot":
			reply(1000)
		case "getBlock":
			blockSlot = req.Params[0].(float64)
			reply(map[string]any{"blockhash": old})
		case "sendTransaction":
			raw, _ := base64.StdEncoding.DecodeString(req.Params[0].(string))
			tx, _ := solana.ParseTx(raw)
			sig, _ := base58.Decode(tx.Signature())
			if !ed25519.Verify(kp.Public, tx.Message(), sig) {
				t.Errorf("signature must cover the swapped blockhash")
			}
			sentHash = tx.BlockhashBase58()
			reply(tx.Signature())
		case "getSignatureStatuses":
			reply(map[string]any{"value": []any{nil}})
		case "isBlockhashValid":
			reply(map[string]any{"value": false})
		}
	}))
	defer rpc.Close()
	l := &Live{Key: kp, RPC: solana.NewRPC(rpc.URL), TradeURL: pp.URL, MaxAttempts: 3, HTTP: http.DefaultClient}
	now := time.Now()
	_, err := l.Execute(context.Background(), Order{Mint: "MINT", Side: "buy", SolIn: 0.1, At: now.Add(-7 * time.Second), Deadline: now.Add(3 * time.Second)})
	if !errors.Is(err, ErrTooLate) {
		t.Fatalf("never landed before the deadline: want too late, got %v", err)
	}
	if sentHash != old {
		t.Fatalf("sent with blockhash %s, want the expiring one", sentHash)
	}
	if back := 1000 - blockSlot; back < 137 || back > 142 { // ~3s = ~7 blocks of life left
		t.Fatalf("picked a block %v slots back", back)
	}
	if builds.Load() != 1 {
		t.Fatalf("built %d times: no retry after the deadline", builds.Load())
	}
	_, err = l.Execute(context.Background(), Order{Mint: "MINT", Side: "buy", SolIn: 0.1, At: now.Add(-20 * time.Second), Deadline: now.Add(-10 * time.Second)})
	if !errors.Is(err, ErrTooLate) || builds.Load() != 1 {
		t.Fatalf("already late: want nothing built, got %v (builds %d)", err, builds.Load())
	}
}

// PumpPortal won't build a Jito bundle sell for a bonding-curve token: fall back
// to a normal tx instead of failing the exit.
func TestJitoBundleFallsBackToNormalTx(t *testing.T) {
	pk := ed25519.NewKeyFromSeed(make([]byte, 32))
	kp, _ := solana.ParseKeypair(base58.Encode(pk))
	var bundles, plain atomic.Int32
	pp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		if b[0] == '[' {
			bundles.Add(1)
			if strings.Contains(string(b), `"sell"`) {
				http.Error(w, "Bad Request", 400)
				return
			}
			json.NewEncoder(w).Encode([]string{base58.Encode(unsignedTx(kp.Public))})
			return
		}
		plain.Add(1)
		w.Write(unsignedTx(kp.Public))
	}))
	defer pp.Close()
	l := &Live{Key: kp, TradeURL: pp.URL, JitoTipSol: 0.0001, PriorityFee: 0.0005, HTTP: http.DefaultClient}
	ctx := context.Background()
	if _, jito, err := l.build(ctx, Order{Mint: "M", Side: "buy", SolIn: 0.1}, kp.PublicBase58()); err != nil || !jito {
		t.Fatalf("buy: jito %v err %v", jito, err)
	}
	if _, jito, err := l.build(ctx, Order{Mint: "M", Side: "sell", SellPct: 100}, kp.PublicBase58()); err != nil || jito {
		t.Fatalf("sell: jito %v err %v", jito, err)
	}
	if bundles.Load() != 2 || plain.Load() != 1 {
		t.Fatalf("bundle requests %d, plain %d", bundles.Load(), plain.Load())
	}
}

// A buy that sat in the queue past the limit is rejected without sending anything.
func TestExecutorRejectsLateBuy(t *testing.T) {
	ex, st := setup(t, Risk{SizeUSD: 25, MaxOpenPositions: 5, BuyMaxLag: 10 * time.Second})
	ex.Sync = false
	rec := &recorder{Paper: ex.Trader.(Paper)}
	ex.Trader = rec
	ctx := context.Background()
	tr := trd("S1", "LIVE", "MINT", "buy", 30, 0, true, 1e6, 1e6)
	tr.ReceivedAt = time.Now().Add(-15 * time.Second)
	ex.mu.Lock()
	ex.curves["MINT"] = curve.State{VSol: 30, VTokens: 32.19e9 / 30}
	ex.mu.Unlock()
	ex.buy(ctx, tr)
	fs, _ := st.ListLiveFills(ctx, store.LiveFilter{})
	if len(rec.orders) != 0 || len(fs) != 1 || fs[0].Status != "rejected" || !strings.Contains(fs[0].Error, "too late") {
		t.Fatalf("orders %d fills %+v", len(rec.orders), fs)
	}
	tr.Signature, tr.ReceivedAt = "S2", time.Now()
	ex.buy(ctx, tr)
	if len(rec.orders) != 1 || rec.orders[0].Deadline.IsZero() {
		t.Fatalf("fresh buy should go out with a deadline: %+v", rec.orders)
	}
}

// The daily stop counts copies still open at their latest price, stays on after a
// restart, alerts once (as the alerter classifies it), and lifts at local midnight.
func TestDailyStopCountsOpenSurvivesRestartAndAlerts(t *testing.T) {
	east, _ := time.LoadLocation("America/Toronto")
	r := Risk{SizeUSD: 25, MaxOpenPositions: 5, DailyLossUSD: 10, DayLoc: east}
	ex, st := setup(t, r)
	var events []model.Event
	ex.Bus.(*bus.Memory).Register(bus.StreamEvents, func(_ context.Context, b []byte) error {
		var ev model.Event
		json.Unmarshal(b, &ev)
		events = append(events, ev)
		return nil
	})
	ctx := context.Background()
	ex.HandleTrade(ctx, trd("a", "LIVE", "M1", "buy", 50, 0, true, 1e6, 1e6))
	// Someone else dumps M1: our open copy is down well over $10, nothing sold yet.
	ex.HandleTrade(ctx, trd("x", "OTHER", "M1", "sell", 35, time.Second, false, 1e6, 0))
	var hits []model.Event
	for _, ev := range events {
		if d := alert.Classify(ev); d.Now && strings.HasPrefix(d.Key, "dayloss:") {
			hits = append(hits, ev)
		}
	}
	if len(hits) != 1 || alert.Classify(hits[0]).Key != "dayloss:2026-09-01" {
		t.Fatalf("want one daily-stop alert keyed by the day, got %+v", hits)
	}
	ex.HandleTrade(ctx, trd("y", "OTHER", "M1", "buy", 60, 2*time.Second, false, 1e6, 0)) // price recovers
	ex.HandleTrade(ctx, trd("c", "LIVE", "M2", "buy", 31, time.Minute, true, 1e6, 1e6))
	if f := fills(st); f["buy:rejected"] != 1 {
		t.Fatalf("stop holds for the day even after a recovery: %v", f)
	}
	// Restart: a new executor on the same store keeps the stop.
	ex2 := New(ex.Trader, st, bus.NewMemory(), price.Fixed(150), r)
	ex2.Sync = true
	if err := ex2.Load(ctx, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	ex2.HandleTrade(ctx, trd("d", "LIVE", "M3", "buy", 31, time.Hour, true, 1e6, 1e6))
	if f := fills(st); f["buy:rejected"] != 2 {
		t.Fatalf("restart must keep the stop: %v", f)
	}
	// t0 is 08:00 Eastern; 16h later is the next Eastern day.
	ex2.HandleTrade(ctx, trd("e", "LIVE", "M4", "buy", 31, 16*time.Hour+time.Minute, true, 1e6, 1e6))
	if f := fills(st); f["buy:filled"] != 2 {
		t.Fatalf("next day buys again: %v", f)
	}
}

// A copy still exits with the wallet after the wallet is demoted.
func TestSellsFollowDemotedWallet(t *testing.T) {
	ex, st := setup(t, Risk{SizeUSD: 25, MaxOpenPositions: 5})
	ctx := context.Background()
	ex.HandleTrade(ctx, trd("a", "LIVE", "M1", "buy", 40, 0, true, 1e6, 1e6))
	st.SetWalletStatus(ctx, "LIVE", model.StatusBench, "demoted", t0)
	if err := ex.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	ex.HandleTrade(ctx, trd("b", "LIVE", "M1", "sell", 45, time.Minute, true, 1e6, 0))
	if f := fills(st); f["sell:filled"] != 1 || ex.OpenCount() != 0 {
		t.Fatalf("demoted wallet's sell not copied: %v", f)
	}
	ex.HandleTrade(ctx, trd("c", "LIVE", "M2", "buy", 40, 2*time.Minute, true, 1e6, 1e6))
	if f := fills(st); f["buy:filled"] != 1 {
		t.Fatalf("but its buys are not: %v", f)
	}
}

// They sell while our buy is still landing: the sell waits for the buy and then
// sells what it got.
func TestSellDuringInflightBuy(t *testing.T) {
	ex, st := setup(t, Risk{SizeUSD: 25, MaxOpenPositions: 5})
	ex.Sync = false
	ctx := context.Background()
	gate := make(chan struct{})
	ex.Trader = &gated{Paper: ex.Trader.(Paper), gate: gate}
	buy := trd("a", "LIVE", "M1", "buy", 40, 0, true, 1e6, 1e6)
	buy.ReceivedAt = time.Now()
	ex.HandleTrade(ctx, buy) // buy blocks in the trader until the gate opens
	sell := trd("b", "LIVE", "M1", "sell", 45, 0, true, 1e6, 0)
	sell.ReceivedAt = time.Now()
	ex.HandleTrade(ctx, sell)
	close(gate)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && fills(st)["sell:filled"] == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if f := fills(st); f["buy:filled"] != 1 || f["sell:filled"] != 1 || ex.OpenCount() != 0 {
		t.Fatalf("want buy then sell, got %v (open %d)", f, ex.OpenCount())
	}
}

type gated struct {
	Paper
	gate chan struct{}
}

func (g *gated) Execute(ctx context.Context, o Order) (Result, error) {
	if o.Side == model.SideBuy {
		<-g.gate
	}
	return g.Paper.Execute(ctx, o)
}

// While the wallet is short of SOL, buys are refused with the reason.
func TestLowSOLPausesBuys(t *testing.T) {
	ex, st := setup(t, Risk{SizeUSD: 25, MaxOpenPositions: 5})
	ctx := context.Background()
	ex.SetLowBalance("low SOL balance: 0.0100 SOL")
	ex.HandleTrade(ctx, trd("a", "LIVE", "M1", "buy", 40, 0, true, 1e6, 1e6))
	fs, _ := st.ListLiveFills(ctx, store.LiveFilter{})
	if len(fs) != 1 || fs[0].Status != "rejected" || !strings.Contains(fs[0].Error, "low SOL") {
		t.Fatalf("got %+v", fs)
	}
	ex.SetLowBalance("")
	ex.HandleTrade(ctx, trd("b", "LIVE", "M2", "buy", 40, time.Second, true, 1e6, 1e6))
	if f := fills(st); f["buy:filled"] != 1 {
		t.Fatalf("resumes when topped up: %v", f)
	}
}

// If the short-lived blockhash can't be fetched, the buy is not sent: with a
// normal blockhash it could land up to a minute late.
func TestBuyNotSentWithoutShortBlockhash(t *testing.T) {
	pk := ed25519.NewKeyFromSeed(make([]byte, 32))
	kp, _ := solana.ParseKeypair(base58.Encode(pk))
	pp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(unsignedTx(kp.Public)) }))
	defer pp.Close()
	var sends atomic.Int32
	rpc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Method string }
		json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "getSlot":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": 1000})
		case "getBlock":
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "error": map[string]any{"code": -32007, "message": "Slot was skipped"}})
		case "sendTransaction":
			sends.Add(1)
		}
	}))
	defer rpc.Close()
	l := &Live{Key: kp, RPC: solana.NewRPC(rpc.URL), TradeURL: pp.URL, MaxAttempts: 3, HTTP: http.DefaultClient}
	now := time.Now()
	_, err := l.Execute(context.Background(), Order{Mint: "M", Side: "buy", SolIn: 0.1, At: now, Deadline: now.Add(1500 * time.Millisecond)})
	if err == nil || sends.Load() != 0 {
		t.Fatalf("sent %d without a short-lived blockhash (err %v)", sends.Load(), err)
	}
}

// A bundle buy also goes through our RPC and is re-sent every second until it
// lands or expires; if it never lands, Jito's verdict on the bundle is in the error.
func TestBundleAlsoSentByRPCAndResent(t *testing.T) {
	pk := ed25519.NewKeyFromSeed(make([]byte, 32))
	kp, _ := solana.ParseKeypair(base58.Encode(pk))
	pp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]string{base58.Encode(unsignedTx(kp.Public))})
	}))
	defer pp.Close()
	var bundles atomic.Int32
	jito := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Method string }
		json.NewDecoder(r.Body).Decode(&req)
		reply := func(v any) { json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": v}) }
		switch req.Method {
		case "sendBundle":
			bundles.Add(1)
			reply("BUNDLE1")
		case "getInflightBundleStatuses":
			reply(map[string]any{"value": []any{map[string]any{"bundle_id": "BUNDLE1", "status": "Failed"}}})
		}
	}))
	defer jito.Close()
	var sends atomic.Int32
	sigs := map[string]bool{}
	var mu sync.Mutex
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
			tx, _ := solana.ParseTx(raw)
			mu.Lock()
			sigs[tx.Signature()] = true
			mu.Unlock()
			reply(tx.Signature())
		case "getSignatureStatuses":
			reply(map[string]any{"value": []any{nil}})
		case "isBlockhashValid":
			reply(map[string]any{"value": false})
		}
	}))
	defer rpc.Close()
	l := &Live{Key: kp, RPC: solana.NewRPC(rpc.URL), TradeURL: pp.URL, JitoURL: jito.URL, JitoTipSol: 0.0001,
		MaxAttempts: 1, ConfirmWait: 2500 * time.Millisecond, HTTP: http.DefaultClient}
	res, err := l.Execute(context.Background(), Order{Mint: "M", Side: "buy", SolIn: 0.1})
	if err == nil || !strings.Contains(err.Error(), "jito bundle BUNDLE1: Failed") {
		t.Fatalf("want the bundle's status in the error, got %v", err)
	}
	if bundles.Load() != 1 || sends.Load() < 3 || len(sigs) != 1 || res.Signature == "" {
		t.Fatalf("bundles %d, rpc sends %d (want first + resends), distinct sigs %d (want 1), sig %q",
			bundles.Load(), sends.Load(), len(sigs), res.Signature)
	}
}

// A copy bought on the bonding curve that migrated since is sold on PumpSwap,
// even when no PumpSwap trade has been seen since start (Nov 18: 6005 x3).
func TestSellAfterMigrationTargetsPumpSwap(t *testing.T) {
	ex, st := setup(t, Risk{SizeUSD: 25, MaxOpenPositions: 5})
	rec := &recorder{Paper: ex.Trader.(Paper)}
	ex.Trader = rec
	ctx := context.Background()
	ex.HandleTrade(ctx, trd("a", "LIVE", "M1", "buy", 40, 0, true, 1e6, 1e6))
	st.UpsertTokenFromTrade(ctx, model.Trade{Mint: "M1", Pool: model.PoolPumpSwap, VSol: 90, VTokens: 2e8, ReceivedAt: t0.Add(time.Minute)})
	ps, _ := st.ListLivePositions(ctx, store.LiveFilter{OpenOnly: true})
	if len(ps) != 1 {
		t.Fatalf("open %d", len(ps))
	}
	if err := ex.ManualSell(ctx, ps[0].ID, 100, t0.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}
	last := rec.orders[len(rec.orders)-1]
	if last.Side != model.SideSell || !last.State.AMM {
		t.Fatalf("sell not aimed at PumpSwap: %+v", last)
	}
	if r := (&Live{}).request(last); r.Pool != "pump-amm" {
		t.Fatalf("PumpPortal pool %q", r.Pool)
	}
}
