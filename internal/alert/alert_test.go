package alert

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"copytrade/internal/model"
)

func TestDiscordSend(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusNoContent) // what Discord returns
	}))
	defer srv.Close()
	d := NewDiscord(srv.URL)
	if err := d.Send(context.Background(), strings.Repeat("x", 3000)); err != nil {
		t.Fatal(err)
	}
	if c, _ := got["content"].(string); len(c) > 2000 || !strings.HasSuffix(c, "...") {
		t.Fatalf("content not truncated: %d", len(c))
	}
	if NewDiscord("").Enabled() {
		t.Fatal("empty webhook must be disabled")
	}
}

func TestFormat(t *testing.T) {
	if Format(model.Event{Kind: "sim_fill", At: time.Now()}) != "" {
		t.Fatal("sim fills must not alert")
	}
	if s := Format(model.Event{Kind: "rotation", Service: "scorer", Message: "x -> live"}); s != "[ROTATION] x -> live (scorer)" {
		t.Fatalf("got %q", s)
	}
}
