package ingest

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/mr-tron/base58"

	"copytrade/internal/curve"
	"copytrade/internal/source"
	"copytrade/internal/store"
)

func TestSeedMigrationQueuesTokenForDiscovery(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	mint := base58.Encode(bytes.Repeat([]byte{7}, 32))
	m := source.Migration{Mint: mint, At: time.Unix(1700000000, 0).UTC()}
	if err := SeedMigration(ctx, st, m); err != nil {
		t.Fatal(err)
	}
	if err := SeedMigration(ctx, st, m); err != nil { // duplicate event is harmless
		t.Fatal(err)
	}
	toks, err := st.ListTokens(ctx, 5, true)
	if err != nil || len(toks) != 1 {
		t.Fatalf("want 1 undiscovered 5x token, got %d (%v)", len(toks), err)
	}
	tok := toks[0]
	want, _ := curve.DeriveBondingCurve(mint)
	if !tok.Migrated || tok.CurveKey != want || tok.FirstPrice != curve.LaunchPrice {
		t.Fatalf("bad token %+v", tok)
	}
	if mult := tok.MaxPrice / tok.FirstPrice; mult < 14 || mult > 15.5 {
		t.Fatalf("completion multiple %.2f, want ~14.7", mult)
	}
}
