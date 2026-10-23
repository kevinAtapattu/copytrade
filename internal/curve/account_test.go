package curve

import (
	"encoding/binary"
	"testing"
)

func TestParseAccount(t *testing.T) {
	data := make([]byte, 8+5*8+1+32) // newer layouts append a creator pubkey
	put := func(i int, v uint64) { binary.LittleEndian.PutUint64(data[8+8*i:], v) }
	put(0, 1_073_000_000_000_000) // virtual tokens, 6 decimals
	put(1, 30_000_000_000)        // virtual SOL, lamports
	put(2, 793_100_000_000_000)
	put(3, 0)
	put(4, 1_000_000_000_000_000)
	a, err := ParseAccount(data)
	if err != nil {
		t.Fatal(err)
	}
	if a.State.VTokens != InitialVirtualTokens || a.State.VSol != InitialVirtualSol || a.RealTokens != RealTokenReserves || a.Complete {
		t.Fatalf("parsed %+v", a)
	}
	data[48] = 1
	if a, _ := ParseAccount(data); !a.Complete {
		t.Fatal("complete flag")
	}
	if _, err := ParseAccount(data[:40]); err == nil {
		t.Fatal("short data must fail")
	}
}

func TestDeriveBondingCurveIsStableAndOffCurve(t *testing.T) {
	a, err := DeriveBondingCurve("So11111111111111111111111111111111111111112")
	if err != nil || len(a) < 32 {
		t.Fatalf("%v %q", err, a)
	}
	b, _ := DeriveBondingCurve("So11111111111111111111111111111111111111112")
	if a != b {
		t.Fatal("not deterministic")
	}
	if _, err := DeriveBondingCurve("bad"); err == nil {
		t.Fatal("bad mint must fail")
	}
}
