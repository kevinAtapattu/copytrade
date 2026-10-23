package source

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"copytrade/internal/model"
)

func TestPumpPortalSingleConnResubscribe(t *testing.T) {
	var open, maxOpen, conns atomic.Int32
	var mu sync.Mutex
	var cmds []ppCommand
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api-key") != "k" {
			t.Errorf("api key not sent")
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		n := open.Add(1)
		defer open.Add(-1)
		if n > maxOpen.Load() {
			maxOpen.Store(n)
		}
		id := conns.Add(1)
		c.WriteMessage(websocket.TextMessage, []byte(`{"message":"Successfully subscribed to keys."}`))
		for i := 0; ; i++ {
			c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
			_, data, err := c.ReadMessage()
			if err != nil {
				if id == 1 { // first connection: push a trade, then drop
					c.WriteMessage(websocket.TextMessage, []byte(`{"signature":"s1","mint":"M","traderPublicKey":"W","txType":"buy","solAmount":1,"tokenAmount":5,"vSolInBondingCurve":31,"vTokensInBondingCurve":1e9}`))
					time.Sleep(50 * time.Millisecond)
					return
				}
				if strings.Contains(err.Error(), "timeout") {
					continue
				}
				return
			}
			var cmd ppCommand
			json.Unmarshal(data, &cmd)
			mu.Lock()
			cmds = append(cmds, cmd)
			mu.Unlock()
		}
	}))
	defer srv.Close()

	p := NewPumpPortal("ws"+strings.TrimPrefix(srv.URL, "http"), "k")
	p.MinBackoff, p.MaxBackoff = 50*time.Millisecond, 100*time.Millisecond
	p.SubscribeAccounts([]string{"W"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan model.Trade, 4)
	go p.Run(ctx, func(tr model.Trade) {
		got <- tr
		p.SubscribeTokens([]string{tr.Mint}) // from inside the handler, must not deadlock
	})
	select {
	case tr := <-got:
		if tr.Signature != "s1" || tr.ReceivedAt.IsZero() {
			t.Fatalf("trade %+v", tr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no trade received")
	}
	deadline := time.Now().Add(3 * time.Second)
	for conns.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	if conns.Load() < 2 {
		t.Fatal("did not reconnect")
	}
	if maxOpen.Load() != 1 {
		t.Fatalf("parallel connections: %d", maxOpen.Load())
	}
	mu.Lock()
	defer mu.Unlock()
	acct, tok := 0, 0
	for _, c := range cmds {
		if c.Method == "subscribeAccountTrade" && len(c.Keys) == 1 && c.Keys[0] == "W" {
			acct++
		}
		if c.Method == "subscribeTokenTrade" && len(c.Keys) == 1 && c.Keys[0] == "M" {
			tok++
		}
	}
	// account sub on both connections; token sub (added on conn 1) re-sent on conn 2
	if acct < 2 || tok < 1 {
		t.Fatalf("subscriptions not resent after reconnect: %+v", cmds)
	}
	if s := p.Stats(); s.Accounts != 1 || s.Tokens != 1 || s.Messages != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestPumpPortalIdleWithoutKey(t *testing.T) {
	p := NewPumpPortal("ws://127.0.0.1:1", "")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	p.Run(ctx, func(model.Trade) {})
	if !strings.Contains(p.Stats().LastError, "PUMPPORTAL_API_KEY") || p.Stats().Reconnects != 0 {
		t.Fatalf("should idle without key: %+v", p.Stats())
	}
}

func TestPumpPortalPauseClosesConnectionUntilResume(t *testing.T) {
	var open, conns atomic.Int32
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		conns.Add(1)
		open.Add(1)
		defer open.Add(-1)
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	p := NewPumpPortal("ws"+strings.TrimPrefix(srv.URL, "http"), "k")
	p.MinBackoff, p.MaxBackoff = 20*time.Millisecond, 40*time.Millisecond
	p.SubscribeAccounts([]string{"W"})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go p.Run(ctx, func(model.Trade) {})
	waitFor := func(cond func() bool, what string) {
		t.Helper()
		for i := 0; i < 200 && !cond(); i++ {
			time.Sleep(10 * time.Millisecond)
		}
		if !cond() {
			t.Fatalf("timed out waiting for %s (open=%d conns=%d)", what, open.Load(), conns.Load())
		}
	}
	waitFor(func() bool { return open.Load() == 1 }, "first connection")
	p.Pause()
	waitFor(func() bool { return open.Load() == 0 }, "connection closed on pause")
	time.Sleep(300 * time.Millisecond) // many backoff periods: must NOT reconnect
	if open.Load() != 0 || conns.Load() != 1 {
		t.Fatalf("reconnected while paused: open=%d conns=%d", open.Load(), conns.Load())
	}
	p.Resume()
	waitFor(func() bool { return open.Load() == 1 && conns.Load() == 2 }, "one reconnect after resume")
}
