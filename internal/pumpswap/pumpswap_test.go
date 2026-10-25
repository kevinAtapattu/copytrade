package pumpswap

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"testing"

	"copytrade/internal/solana"
)

// mainnet.json holds a real SellEvent tx (bust selling Hiog5y), a real BuyEvent tx
// on the same pool, and that pool's account, captured October 2026.
type fixture struct {
	Sell, Buy struct {
		Signature string   `json:"signature"`
		Logs      []string `json:"logs"`
		Inner     []struct {
			Program string `json:"program"`
			DataHex string `json:"data_hex"`
		} `json:"inner"`
	}
	PoolB64 string `json:"pool_b64"`
}

const (
	poolAddr = "4xMCDt7EuE1HDA6h4AF2F9xQWSBk1BXduj3y7eukkVby"
	mint     = "Hiog5ybjwPKa2bENQTyK9yNMEjUdUX9SStdRVRHLpump"
	bust     = "FzVQSzj8JJr6WMGqbUHzx2XH1KkrfxRrRPv6WcbbZmND"
)

func load(t *testing.T) fixture {
	t.Helper()
	b, err := os.ReadFile("testdata/mainnet.json")
	if err != nil {
		t.Fatal(err)
	}
	var f fixture
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

func near(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestSellEventFromLogs(t *testing.T) {
	f := load(t)
	evs := EventsFromLogs(f.Sell.Logs)
	if len(evs) != 1 {
		t.Fatalf("events %d", len(evs))
	}
	e := evs[0]
	// Matches the pool vault balances in the tx: base 214,352,076.465754 -> +5,740,740.015716.
	if e.IsBuy || e.Pool != poolAddr || e.User != bust || !near(e.Base, 5740740.015716, 1e-6) ||
		!near(e.PoolBase, 214352076.465754, 1e-6) || !near(e.PoolQuote, 68.744602993, 1e-9) ||
		!near(e.Quote, 2.198207738, 1e-9) || !near(e.UserQuote, 2.17073014, 1e-9) {
		t.Fatalf("sell %+v", e)
	}
	// Post-trade SOL vault in the tx was 66.563651187; our estimate is within 0.03%.
	after := e.After(0)
	if !near(after.VSol, 66.563651187, 0.02) || !near(after.VTokens, 220092816.48147, 1e-3) {
		t.Fatalf("after %+v", after)
	}
	if fee := e.FeePct(); fee < 1 || fee > 1.5 {
		t.Fatalf("fee %.3f%%", fee)
	}
}

func TestBuyEventFromInnerInstructions(t *testing.T) {
	f := load(t)
	tx := solana.TxEvents{} // logs dropped: as if truncated
	for _, in := range f.Buy.Inner {
		b, _ := hex.DecodeString(in.DataHex)
		tx.InnerData = append(tx.InnerData, b)
		tx.InnerPrograms = append(tx.InnerPrograms, in.Program)
	}
	evs := EventsFromTx(tx)
	if len(evs) != 1 {
		t.Fatalf("events %d", len(evs))
	}
	e := evs[0]
	// Vault deltas in the tx: base -601,544.396779, SOL +0.017034347.
	if !e.IsBuy || e.Pool != poolAddr || !near(e.Base, 601544.396779, 1e-6) || !near(e.Quote, 0.016901667, 1e-9) ||
		!near(e.PoolBase, 802946948.479501, 1e-6) || !near(e.PoolQuote, 4.959271438, 1e-9) {
		t.Fatalf("buy %+v", e)
	}
	// Logs and inner instructions carry the same event once.
	tx.Logs = f.Buy.Logs
	if n := len(EventsFromTx(tx)); n != 1 {
		t.Fatalf("deduped events %d", n)
	}
}

func TestPoolAccountAndVirtualReserve(t *testing.T) {
	f := load(t)
	d, _ := base64.StdEncoding.DecodeString(f.PoolB64)
	p, err := ParsePool(d)
	if err != nil {
		t.Fatal(err)
	}
	if p.BaseMint != mint || p.QuoteMint != WSOL || !p.SOLQuoted() ||
		p.BaseVault != "2TGBhPbrxYP5XFhDYnRdPSxdBAtnqWJNxbjvUnP6WT5N" || p.QuoteVault != "2nDPiv8FZfGr9z8ryUshaMK623SyJQRbdoiRarCcE9oa" {
		t.Fatalf("pool %+v", p)
	}
	if !near(p.VirtualQuote, 17.58, 0.05) {
		t.Fatalf("virtual quote %.4f", p.VirtualQuote)
	}
	// The buy only fits constant product with the virtual reserve added:
	// (quote + V) * base_out / (base - base_out) = quote_in.
	var buy Event
	for _, in := range f.Buy.Inner {
		b, _ := hex.DecodeString(in.DataHex)
		if in.Program == Program && len(b) > 16 && string(b[:8]) == string(cpiTag) {
			buy, _ = DecodeEvent(b[8:])
		}
	}
	got := (buy.PoolQuote + p.VirtualQuote) * buy.Base / (buy.PoolBase - buy.Base)
	if !near(got, buy.Quote, buy.Quote*0.001) {
		t.Fatalf("constant product with V: %.9f SOL, event %.9f", got, buy.Quote)
	}
}

func TestDerivePool(t *testing.T) {
	got, err := DerivePool(mint)
	if err != nil || got != poolAddr {
		t.Fatalf("derived %s %v", got, err)
	}
}

func TestStateFromVaults(t *testing.T) {
	p := Pool{VirtualQuote: 17.5}
	vault := func(amount uint64) []byte {
		b := make([]byte, 165)
		for i := 0; i < 8; i++ {
			b[64+i] = byte(amount >> (8 * i))
		}
		return b
	}
	st, err := p.State(vault(500_000_000_000_000), vault(12_500_000_000))
	if err != nil || !st.AMM || st.VTokens != 500_000_000 || st.VSol != 30 {
		t.Fatalf("state %+v %v", st, err)
	}
}
