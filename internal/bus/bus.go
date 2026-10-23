// Package bus is the event bus between services. Redis Streams in production,
// a synchronous in-memory fan-out for tests and replay.
package bus

import (
	"context"
	"encoding/json"
	"sync"
)

// Streams.
const (
	StreamTrades = "trades"  // every trade (model.Trade, IsTarget set for tracked wallets)
	StreamEvents = "events"  // model.Event notifications for UI + alerts
)

type Handler func(ctx context.Context, payload []byte) error

type Bus interface {
	Publish(ctx context.Context, stream string, payload []byte) error
	// Subscribe blocks, delivering messages to h. Consumers in the same group share
	// work; each group sees every message. Group "" means read new messages only, no ack.
	Subscribe(ctx context.Context, stream, group, consumer string, h Handler) error
	Close() error
}

// PublishJSON marshals v and publishes it.
func PublishJSON(ctx context.Context, b Bus, stream string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Publish(ctx, stream, data)
}

// Memory delivers synchronously, in publish order, to every registered handler.
type Memory struct {
	mu       sync.RWMutex
	handlers map[string][]Handler
}

func NewMemory() *Memory { return &Memory{handlers: map[string][]Handler{}} }

// Register adds a handler without blocking (used to wire replay deterministically).
func (m *Memory) Register(stream string, h Handler) {
	m.mu.Lock()
	m.handlers[stream] = append(m.handlers[stream], h)
	m.mu.Unlock()
}

func (m *Memory) Publish(ctx context.Context, stream string, payload []byte) error {
	m.mu.RLock()
	hs := append([]Handler(nil), m.handlers[stream]...)
	m.mu.RUnlock()
	for _, h := range hs {
		if err := h(ctx, payload); err != nil {
			return err
		}
	}
	return nil
}

func (m *Memory) Subscribe(ctx context.Context, stream, _, _ string, h Handler) error {
	m.Register(stream, h)
	<-ctx.Done()
	return ctx.Err()
}

func (m *Memory) Close() error { return nil }
