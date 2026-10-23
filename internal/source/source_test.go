package source

import (
	"strings"
	"testing"
	"time"

	"copytrade/internal/model"
)

func TestParsePumpPortal(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	raw := `{"signature":"5abc","mint":"Mint111pump","traderPublicKey":"Trader1","txType":"buy","tokenAmount":3511232.5,
	"solAmount":"0.1","newTokenBalance":3511232.5,"bondingCurveKey":"BC","vTokensInBondingCurve":1069488767.5,
	"vSolInBondingCurve":30.098,"marketCapSol":28.14,"pool":"pump"}`
	tr, ok := ParsePumpPortal([]byte(raw), at)
	if !ok {
		t.Fatal("should parse")
	}
	if tr.Side != "buy" || tr.SolAmount != 0.1 || tr.VSol != 30.098 || tr.Trader != "Trader1" || !tr.ReceivedAt.Equal(at) {
		t.Fatalf("parsed %+v", tr)
	}
	for _, s := range []string{`{"message":"Successfully subscribed"}`, `{"errors":"bad key"}`, `not json`,
		strings.Replace(raw, `"txType":"buy"`, `"txType":"create"`, 1)} {
		if _, ok := ParsePumpPortal([]byte(s), at); ok {
			t.Fatalf("should not parse as trade: %s", s)
		}
	}
	// no pool defaults to pump; encode round-trips
	tr2, _ := ParsePumpPortal([]byte(strings.Replace(raw, `,"pool":"pump"`, "", 1)), at)
	if tr2.Pool != "pump" {
		t.Fatal("default pool")
	}
	back, ok := ParsePumpPortal(EncodePumpPortal(tr), at)
	if !ok || back != tr {
		t.Fatalf("round trip %+v vs %+v", back, tr)
	}
}

func TestReplayHonorsSubscriptions(t *testing.T) {
	lines := `{"at":"2026-09-01T00:00:00Z","msg":{"signature":"a","mint":"M1","traderPublicKey":"W","txType":"buy","vSolInBondingCurve":31,"vTokensInBondingCurve":1e9}}
{"at":"2026-09-01T00:00:01Z","msg":{"signature":"b","mint":"M1","traderPublicKey":"X","txType":"buy","vSolInBondingCurve":32,"vTokensInBondingCurve":9e8}}
{"at":"2026-09-01T00:00:02Z","msg":{"signature":"c","mint":"M2","traderPublicKey":"Y","txType":"buy","vSolInBondingCurve":32,"vTokensInBondingCurve":9e8}}`
	r, err := NewReplay(strings.NewReader(lines))
	if err != nil {
		t.Fatal(err)
	}
	r.SubscribeAccounts([]string{"W"})
	var got []string
	ticks := 0
	r.AfterEach = func(time.Time) { ticks++ }
	_ = r.Run(t.Context(), func(tr model.Trade) {
		got = append(got, tr.Signature)
		if tr.Trader == "W" {
			r.SubscribeTokens([]string{tr.Mint})
		}
	})
	if strings.Join(got, ",") != "a,b" || ticks != 3 {
		t.Fatalf("delivered %v ticks %d", got, ticks)
	}
}
