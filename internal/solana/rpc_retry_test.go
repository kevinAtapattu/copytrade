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
