package nonsolsim

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestJupiterQuote(t *testing.T) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("outputMint") {
		case "BAD":
			w.WriteHeader(400)
			w.Write([]byte(`{"error":"The token BAD is not tradable","errorCode":"TOKEN_NOT_TRADABLE"}`))
		default:
			if n.Add(1) == 1 {
				w.WriteHeader(429)
				return
			}
			w.Write([]byte(`{"inAmount":"140000000","outAmount":"1569943113128","routePlan":[{"swapInfo":{"label":"Raydium CLMM"}},{"swapInfo":{"label":"Pump.fun"}}]}`))
		}
	}))
	defer srv.Close()
	j := NewJupiter(srv.URL + "/")
	j.Gap = time.Millisecond
	q, err := j.Quote(context.Background(), WSOL, "GOOD", 140000000)
	if err != nil || q.OutAmount != 1569943113128 || q.Route != "Raydium CLMM > Pump.fun" || n.Load() != 2 {
		t.Fatalf("got %+v err %v calls %d", q, err, n.Load())
	}
	_, err = j.Quote(context.Background(), WSOL, "BAD", 1)
	var nr *NoRouteError
	if !errors.As(err, &nr) || ReasonOf(err) != "TOKEN_NOT_TRADABLE" {
		t.Fatalf("want TOKEN_NOT_TRADABLE, got %v", err)
	}
}
