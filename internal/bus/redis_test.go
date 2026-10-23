package bus

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
)

func TestRedisGroupsEachSeeEveryMessage(t *testing.T) {
	mr := miniredis.RunT(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := OpenRedis(ctx, "redis://"+mr.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	var mu sync.Mutex
	got := map[string][]string{}
	sub := func(group string) {
		go r.Subscribe(ctx, StreamTrades, group, group+"-1", func(_ context.Context, p []byte) error {
			mu.Lock()
			got[group] = append(got[group], string(p))
			mu.Unlock()
			return nil
		})
	}
	sub("simulator")
	sub("executor")
	time.Sleep(300 * time.Millisecond) // let groups get created at "$"
	for _, m := range []string{"a", "b", "c"} {
		if err := r.Publish(ctx, StreamTrades, []byte(m)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		done := len(got["simulator"]) == 3 && len(got["executor"]) == 3
		mu.Unlock()
		if done {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, g := range []string{"simulator", "executor"} {
		if len(got[g]) != 3 || got[g][0] != "a" || got[g][2] != "c" {
			t.Fatalf("group %s got %v", g, got[g])
		}
	}
}
