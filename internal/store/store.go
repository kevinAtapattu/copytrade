// Package store defines persistence. Postgres is used in production; Memory
// backs tests and replay so the whole pipeline runs without Docker.
package store

import (
	"context"
	"encoding/json"
	"time"

	"copytrade/internal/model"
)

type TradeFilter struct {
	Wallet string
	Mint   string
	Since  time.Time
	Limit  int
}

type SimFilter struct {
	Wallet   string
	Mint     string
	DelayS   int // 0 = all
	Since    time.Time
	OpenOnly bool
	Limit    int
}

type LiveFilter struct {
	Mode     string
	Wallet   string
	Since    time.Time
	OpenOnly bool
	Limit    int
}

type Store interface {
	// Wallets
	AddWallet(ctx context.Context, w model.Wallet) (bool, error) // false if it already existed
	ListWallets(ctx context.Context) ([]model.Wallet, error)
	GetWallet(ctx context.Context, addr string) (model.Wallet, bool, error)
	// SetWalletStatus updates status and logs a rotation if it changed.
	SetWalletStatus(ctx context.Context, addr, status, reason string, at time.Time) (changed bool, err error)
	ListRotations(ctx context.Context, limit int) ([]model.Rotation, error)

	// Trades
	InsertTokenTrade(ctx context.Context, t model.Trade) (bool, error)
	InsertTargetTrade(ctx context.Context, t model.Trade) (bool, error)
	ListTargetTrades(ctx context.Context, f TradeFilter) ([]model.Trade, error)
	ListTokenTrades(ctx context.Context, f TradeFilter) ([]model.Trade, error)
	CountTokenTradesSince(ctx context.Context, since time.Time) (int64, error)

	// Tokens
	UpsertTokenFromTrade(ctx context.Context, t model.Trade) error
	GetToken(ctx context.Context, mint string) (model.Token, bool, error)
	ListTokens(ctx context.Context, minMaxMult float64, undiscoveredOnly bool) ([]model.Token, error)
	MarkTokenDiscovered(ctx context.Context, mint string) error
	// SeedMigratedToken records a token that completed its curve so discovery scans it.
	// New tokens get the launch and completion prices; existing ones are marked migrated.
	SeedMigratedToken(ctx context.Context, tok model.Token) (bool, error)

	// Simulation
	InsertSimTrade(ctx context.Context, s model.SimTrade) error
	UpsertSimPosition(ctx context.Context, p model.SimPosition) error
	ListSimPositions(ctx context.Context, f SimFilter) ([]model.SimPosition, error)
	ListSimTrades(ctx context.Context, f SimFilter) ([]model.SimTrade, error)
	SimTradesForTarget(ctx context.Context, targetSig string) ([]model.SimTrade, error)

	// Scores
	InsertScores(ctx context.Context, s []model.Score) error
	LatestScores(ctx context.Context) ([]model.Score, error)
	ScoreHistory(ctx context.Context, wallet string, delayS, windowDays int, since time.Time) ([]model.Score, error)

	// Discovery
	InsertDiscoveryHit(ctx context.Context, h model.DiscoveryHit) (bool, error)
	ListDiscoveryHits(ctx context.Context, limit int) ([]model.DiscoveryHit, error)

	// Executor
	InsertLiveFill(ctx context.Context, f model.LiveFill) error
	ListLiveFills(ctx context.Context, f LiveFilter) ([]model.LiveFill, error)
	UpsertLivePosition(ctx context.Context, p model.LivePosition) error
	ListLivePositions(ctx context.Context, f LiveFilter) ([]model.LivePosition, error)

	// Settings + health
	GetSetting(ctx context.Context, key string) (string, bool, error)
	SetSetting(ctx context.Context, key, value string) error
	PutHealth(ctx context.Context, service string, data json.RawMessage, at time.Time) error
	ListHealth(ctx context.Context) (map[string]Health, error)

	Close()
}

type Health struct {
	Data      json.RawMessage `json:"data"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// Setting keys.
const (
	SettingKillSwitch = "kill_switch"
	// SettingDayStop is "<mode>:<YYYY-MM-DD>" for the day the daily loss limit was hit.
	SettingDayStop = "exec_day_stop"
)
