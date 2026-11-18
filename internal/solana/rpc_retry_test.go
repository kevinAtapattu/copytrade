package solana

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadsRetryOn429(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits < 3 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte("Too Many Requests"))
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": []any{}})
	}))
	defer srv.Close()
	r := NewRPC(srv.URL)
	if _, err := r.GetSignaturesForAddress(context.Background(), "ADDR", "", 10); err != nil || hits != 3 {
		t.Fatalf("want success on 3rd try, got err=%v hits=%d", err, hits)
	}
	hits = -100 // always 429
	if _, err := r.SendTransaction(context.Background(), []byte{1}); !errors.Is(err, ErrRateLimited) || hits != -99 {
		t.Fatalf("sendTransaction must not retry: err=%v hits=%d", err, hits)
	}
}

// A buyer paying from a wrapped-SOL account shows almost no native change; the
// WSOL it spent still counts as SOL out.
func TestBalanceChangeCountsWrappedSOL(t *testing.T) {
	tb := func(i int, mint, owner, amt string) map[string]any {
		return map[string]any{"accountIndex": i, "mint": mint, "owner": owner, "uiTokenAmount": map[string]any{"uiAmountString": amt}}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{
			"blockTime": 1,
			"meta": map[string]any{"fee": 5000, "preBalances": []uint64{1_000_000_000, 0, 0}, "postBalances": []uint64{998_500_000, 0, 0},
				"preTokenBalances":  []any{tb(1, wsolMint, "OWNER", "3"), tb(2, "MINT", "OWNER", "0")},
				"postTokenBalances": []any{tb(1, wsolMint, "OWNER", "0.25"), tb(2, "MINT", "OWNER", "1000")}},
			"transaction": map[string]any{"message": map[string]any{"accountKeys": []any{
				map[string]any{"pubkey": "OWNER"}, map[string]any{"pubkey": "WSOLACCT"}, map[string]any{"pubkey": "TOKACCT"}}}},
		}})
	}))
	defer srv.Close()
	bc, err := NewRPC(srv.URL).GetBalanceChange(context.Background(), "SIG", "OWNER", "MINT")
	if err != nil {
		t.Fatal(err)
	}
	if got := -bc.SolDelta; got < 2.7514 || got > 2.7516 || bc.TokenDelta != 1000 {
		t.Fatalf("SOL out %.4f (want 2.7515: 0.0015 native + 2.75 wrapped), tokens %v", got, bc.TokenDelta)
	}
}
