package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

const maxLen = 200000 // approximate cap per stream

type Redis struct {
	c *redis.Client
}

func OpenRedis(ctx context.Context, url string) (*Redis, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, err
	}
	c := redis.NewClient(opt)
	var lastErr error
	for i := 0; i < 30; i++ {
		if lastErr = c.Ping(ctx).Err(); lastErr == nil {
			return &Redis{c: c}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return nil, fmt.Errorf("connect redis: %w", lastErr)
}

func (r *Redis) Client() *redis.Client { return r.c }

func (r *Redis) Close() error { return r.c.Close() }

func (r *Redis) Publish(ctx context.Context, stream string, payload []byte) error {
	return r.c.XAdd(ctx, &redis.XAddArgs{
		Stream: stream, MaxLen: maxLen, Approx: true, Values: map[string]any{"d": payload},
	}).Err()
}

func (r *Redis) Subscribe(ctx context.Context, stream, group, consumer string, h Handler) error {
	if group == "" {
		return r.tail(ctx, stream, h)
	}
	err := r.c.XGroupCreateMkStream(ctx, stream, group, "$").Err()
	if err != nil && !strings.Contains(err.Error(), "BUSYGROUP") {
		return err
	}
	// First drain our pending entries (crash recovery), then new ones.
	start := "0"
	for ctx.Err() == nil {
		res, err := r.c.XReadGroup(ctx, &redis.XReadGroupArgs{
			Group: group, Consumer: consumer, Streams: []string{stream, start}, Count: 200, Block: 2 * time.Second,
		}).Result()
		if errors.Is(err, redis.Nil) {
			if start == "0" {
				start = ">"
			}
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("xreadgroup", "stream", stream, "err", err)
			time.Sleep(time.Second)
			continue
		}
		n := 0
		for _, s := range res {
			for _, m := range s.Messages {
				n++
				if err := h(ctx, payloadOf(m)); err != nil {
					slog.Warn("handler error", "stream", stream, "id", m.ID, "err", err)
				}
				// Ack even on handler error so a poison message cannot wedge the group.
				r.c.XAck(ctx, stream, group, m.ID)
			}
		}
		if n == 0 && start == "0" {
			start = ">"
		}
	}
	return ctx.Err()
}

func (r *Redis) tail(ctx context.Context, stream string, h Handler) error {
	last := "$"
	for ctx.Err() == nil {
		res, err := r.c.XRead(ctx, &redis.XReadArgs{Streams: []string{stream, last}, Count: 200, Block: 2 * time.Second}).Result()
		if errors.Is(err, redis.Nil) {
			continue
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			time.Sleep(time.Second)
			continue
		}
		for _, s := range res {
			for _, m := range s.Messages {
				last = m.ID
				_ = h(ctx, payloadOf(m))
			}
		}
	}
	return ctx.Err()
}

// Lag returns how many entries in stream the group has not yet delivered, plus pending.
func (r *Redis) Lag(ctx context.Context, stream, group string) (int64, error) {
	groups, err := r.c.XInfoGroups(ctx, stream).Result()
	if err != nil {
		return 0, err
	}
	for _, g := range groups {
		if g.Name == group {
			return g.Lag + g.Pending, nil
		}
	}
	return 0, nil
}

func payloadOf(m redis.XMessage) []byte {
	switch v := m.Values["d"].(type) {
	case string:
		return []byte(v)
	case []byte:
		return v
	}
	return nil
}
