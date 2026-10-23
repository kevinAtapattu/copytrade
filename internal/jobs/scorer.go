// Package jobs holds the periodic jobs (scoring + rotation, discovery) so the
// services and the replay driver run identical code.
package jobs

import (
	"context"
	"log/slog"
	"time"

	"copytrade/internal/bus"
	"copytrade/internal/config"
	"copytrade/internal/model"
	"copytrade/internal/rotation"
	"copytrade/internal/score"
	"copytrade/internal/sim"
	"copytrade/internal/store"
)

// ScoreAndRotate computes a score snapshot, then applies promotion/demotion rules.
func ScoreAndRotate(ctx context.Context, st store.Store, b bus.Bus, c config.Config, now time.Time) ([]model.Score, []rotation.Change, error) {
	sc := score.Scorer{Store: st, P: score.Params{SizeUSD: c.CopySizeUSD, BankrollUSD: c.BankrollUSD, Fees: sim.FeesFrom(c)}}
	scores, err := sc.Run(ctx, now)
	if err != nil {
		return nil, nil, err
	}
	wallets, err := st.ListWallets(ctx)
	if err != nil {
		return scores, nil, err
	}
	rules := rotation.FromConfig(c)
	done, err := rotation.Apply(ctx, st, rules.Decide(wallets, rotation.Index(scores)), now)
	EmitRotations(ctx, b, "scorer", done, now)
	return scores, done, err
}

// EmitRotations publishes rotation events (dashboard + Telegram).
func EmitRotations(ctx context.Context, b bus.Bus, svc string, cs []rotation.Change, now time.Time) {
	for _, ch := range cs {
		slog.Info("rotation", "wallet", ch.Wallet, "from", ch.From, "to", ch.To, "reason", ch.Reason)
		ev := model.Event{Kind: "rotation", At: now, Service: svc,
			Message: short(ch.Wallet) + " " + ch.From + " -> " + ch.To + ": " + ch.Reason, Data: ch}
		if err := bus.PublishJSON(ctx, b, bus.StreamEvents, ev); err != nil {
			slog.Warn("emit rotation", "err", err)
		}
	}
}

func short(a string) string {
	if len(a) <= 10 {
		return a
	}
	return a[:4] + ".." + a[len(a)-4:]
}

// EmitError publishes an error event (alerts).
func EmitError(ctx context.Context, b bus.Bus, svc string, err error) {
	_ = bus.PublishJSON(ctx, b, bus.StreamEvents, model.Event{Kind: "error", At: time.Now(), Service: svc, Message: err.Error()})
}
