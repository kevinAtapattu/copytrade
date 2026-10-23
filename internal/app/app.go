// Package app holds process bootstrap shared by every cmd.
package app

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/config"
	"copytrade/internal/model"
	"copytrade/internal/store"
)

type App struct {
	Name  string
	Cfg   config.Config
	Store store.Store
	PG    *store.Postgres
	Bus   bus.Bus
	Redis *bus.Redis
	Ctx   context.Context
	stop  context.CancelFunc
}

// Start loads config, sets up logging and connects to Postgres + Redis.
func Start(name string) *App {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})).With("svc", name))
	cfg := config.Load()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	pg, err := store.OpenPostgres(ctx, cfg.DatabaseURL)
	if err != nil {
		Fatal("postgres", err)
	}
	if _, err := pg.Migrate(ctx); err != nil {
		Fatal("migrate", err)
	}
	rb, err := bus.OpenRedis(ctx, cfg.RedisURL)
	if err != nil {
		Fatal("redis", err)
	}
	slog.Info("started")
	return &App{Name: name, Cfg: cfg, Store: pg, PG: pg, Bus: rb, Redis: rb, Ctx: ctx, stop: stop}
}

func (a *App) Close() {
	a.stop()
	a.Bus.Close()
	a.Store.Close()
	slog.Info("stopped")
}

// Heartbeat writes fn() as this service's health row every interval until ctx ends.
func (a *App) Heartbeat(every time.Duration, fn func() any) {
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			data, _ := json.Marshal(fn())
			if err := a.Store.PutHealth(a.Ctx, a.Name, data, time.Now()); err != nil && a.Ctx.Err() == nil {
				slog.Warn("heartbeat", "err", err)
			}
			select {
			case <-a.Ctx.Done():
				return
			case <-t.C:
			}
		}
	}()
}

// Emit publishes an event on the events stream, logging on failure.
func Emit(ctx context.Context, b bus.Bus, svc, kind, msg string, data any) {
	ev := model.Event{Kind: kind, At: time.Now(), Service: svc, Message: msg, Data: data}
	if err := bus.PublishJSON(ctx, b, bus.StreamEvents, ev); err != nil {
		slog.Warn("emit event", "err", err)
	}
}

func Fatal(what string, err error) {
	slog.Error(what, "err", err)
	os.Exit(1)
}
