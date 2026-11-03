package jobs

import (
	"context"
	"strings"
	"testing"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/model"
	"copytrade/internal/store"
)

const (
	addrA = "5YRgrP3mjGzrzirYYN5HAQH19cTYREYwGxW6XRJQUzij"
	addrB = "DAEdBmTPEKM6xkwfzC3d411QUe6coKpkND6UURa4CvHC"
	addrC = "8rvAsDKeAcEjEkiZMug9k8v1y8mW6gQQiMobd89Uy7qR"
	addrD = "9RrKUhRpbPDNxR7x88ZsCgdtqPHUfwYPjj4JdpV4FBj9"
)

// Shaped like the page: rows inside an escaped JSON string in a script tag.
func kolscanPage() []byte {
	rows := []string{
		`{"wallet_address":"` + addrA + `","name":"slingoor","profit":12.5,"wins":15,"losses":4,"timeframe":1}`,
		`{"wallet_address":"` + addrA + `","name":"slingoor","profit":-3,"wins":40,"losses":60,"timeframe":7}`,
		`{"wallet_address":"` + addrB + `","name":"b","profit":20,"wins":30,"losses":10,"timeframe":7}`,
		`{"wallet_address":"` + addrC + `","name":"c","telegram":null}`,
		`{"wallet_address":"` + addrD + `","name":"d","profit":50,"wins":10,"losses":10,"timeframe":30}`,
	}
	return []byte(`<script>self.__next_f.push([1,"` + strings.ReplaceAll(strings.Join(rows, ","), `"`, `\"`) + `"])</script>`)
}

func TestParseKolscanKeepsSevenDayStats(t *testing.T) {
	ls := ParseKolscan(kolscanPage())
	if len(ls) != 4 {
		t.Fatalf("leaders %+v", ls)
	}
	a := ls[0]
	if a.Address != addrA || a.Days != 7 || a.Profit != -3 || a.Wins != 40 || a.Name != "slingoor" {
		t.Fatalf("slingoor %+v", a)
	}
	if c := ls[2]; c.Address != addrC || c.Days != 0 {
		t.Fatalf("listed-only %+v", c)
	}
}

func TestRefillAddsBestUntriedAndRetriesOldDrops(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemory()
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	st.AddWallet(ctx, model.Wallet{Address: "watched1", Status: model.StatusBench, AddedAt: now})
	// D was dropped 40 days ago and is profitable on the board again.
	st.AddWallet(ctx, model.Wallet{Address: addrD, Status: model.StatusDropped, AddedAt: now.AddDate(0, 0, -60), StatusAt: now.AddDate(0, 0, -40)})
	ls := ParseKolscan(kolscanPage())

	added, err := Refill(ctx, st, bus.NewMemory(), ls, 3, 30*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	// Profitable with stats first, by win rate: B (75%) then D (50%). A lost money over 7d.
	if len(added) != 2 || added[0] != addrB || added[1] != addrD {
		t.Fatalf("added %v", added)
	}
	ws, _ := st.ListWallets(ctx)
	for _, w := range ws {
		if w.Address == addrB && (w.Status != model.StatusBench || w.Source != model.SourceLeaderboard || w.Label != "b") {
			t.Fatalf("B %+v", w)
		}
		if w.Address == addrD && w.Status != model.StatusBench {
			t.Fatalf("D not retried: %+v", w)
		}
	}
	// Full: nothing more.
	if added, _ := Refill(ctx, st, bus.NewMemory(), ls, 3, 30*24*time.Hour, now); len(added) != 0 {
		t.Fatalf("over target: %v", added)
	}
	// With room, the losing and stat-less traders fill in, in page order.
	if added, _ := Refill(ctx, st, bus.NewMemory(), ls, 5, 30*24*time.Hour, now); len(added) != 2 || added[0] != addrA || added[1] != addrC {
		t.Fatalf("rest %v", added)
	}
}
