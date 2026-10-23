package exec

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
	if reqBody["action"] != "buy" || reqBody["denominatedInSol"] != "true" || reqBody["publicKey"] != owner || reqBody["pool"] != "pump" {
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
