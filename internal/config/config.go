// Package config loads settings from the environment, reading .env if present.
package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

type Config struct {
	DatabaseURL string
	RedisURL    string

	// Ingest
	PumpPortalAPIKey   string
	PumpPortalWSURL    string
	WalletsFile        string
	TokenUnsubCooldown time.Duration
	TokenMaxWatch      time.Duration
	WalletReload       time.Duration

	// Simulation
	CopySizeUSD    float64
	PumpFeePct     float64
	BotFeeEnabled  bool
	BotFeePct      float64
	PriorityFeeSol float64
	BaseFeeSol     float64
	SolUSDFallback float64
	SolUSDFixed    float64 // >0 disables price fetching (replay, tests)

	// Scoring + rotation
	ScoreInterval    time.Duration
	RotationDelayS   int
	LiveMax          int
	BenchMax         int
	BankrollUSD      float64
	MaxDrawdown      float64
	PromoteMinDays   float64
	PromoteMinTrades int
	DropInactiveDays float64
	DropBadPnlUSD    float64
	DropBadMinTrades int

	// Discovery
	DiscoveryInterval  time.Duration
	DiscoveryMult      float64
	DiscoveryEarlyMult float64
	DiscoveryExitMult  float64
	DiscoveryMinHits   int

	// API
	APIAddr string

	// Executor
	LiveTrading           bool
	TraderPrivateKey      string
	SolanaRPCURL          string
	ExecSizeUSD           float64
	ExecSlippagePct       float64
	ExecPriorityFee       float64
	JitoTipSol            float64
	JitoURL               string
	MaxOpenPositions      int
	StopLossPct           float64
	DailyLossLimitUSD     float64
	ExecMaxAttempts       int
	PumpPortalTradeURL    string
	PumpPortalLocalFeePct float64

	// Alerts
	DiscordWebhookURL string
	TelegramBotToken  string
	TelegramChatID    string
}

// Load reads .env (searching up from the working directory) then the environment.
func Load() Config {
	loadDotEnv()
	return Config{
		DatabaseURL: str("DATABASE_URL", "postgres://copytrade:copytrade@localhost:5432/copytrade?sslmode=disable"),
		RedisURL:    str("REDIS_URL", "redis://localhost:6379"),

		PumpPortalAPIKey:   str("PUMPPORTAL_API_KEY", ""),
		PumpPortalWSURL:    str("PUMPPORTAL_WS_URL", "wss://pumpportal.fun/api/data"),
		WalletsFile:        str("WALLETS_FILE", "wallets.txt"),
		TokenUnsubCooldown: dur("TOKEN_UNSUB_COOLDOWN", 5*time.Minute),
		TokenMaxWatch:      dur("TOKEN_MAX_WATCH", 24*time.Hour),
		WalletReload:       dur("WALLET_RELOAD_INTERVAL", 30*time.Second),

		CopySizeUSD:    flt("COPY_SIZE_USD", 25),
		PumpFeePct:     flt("PUMP_FEE_PCT", 1.25),
		BotFeeEnabled:  boolean("BOT_FEE_ENABLED", false),
		BotFeePct:      flt("BOT_FEE_PCT", 1.0),
		PriorityFeeSol: flt("PRIORITY_FEE_SOL", 0.0005),
		BaseFeeSol:     flt("BASE_FEE_SOL", 0.000005),
		SolUSDFallback: flt("SOL_USD_FALLBACK", 150),
		SolUSDFixed:    flt("SOL_USD_FIXED", 0),

		ScoreInterval:    dur("SCORE_INTERVAL", 5*time.Minute),
		RotationDelayS:   integer("ROTATION_DELAY_S", 2),
		LiveMax:          integer("LIVE_MAX", 3),
		BenchMax:         integer("BENCH_MAX", 50),
		BankrollUSD:      flt("BANKROLL_USD", 500),
		MaxDrawdown:      flt("MAX_DRAWDOWN", 0.25),
		PromoteMinDays:   flt("PROMOTE_MIN_DAYS", 14),
		PromoteMinTrades: integer("PROMOTE_MIN_TRADES", 10),
		DropInactiveDays: flt("DROP_INACTIVE_DAYS", 7),
		DropBadPnlUSD:    flt("DROP_BAD_PNL_USD", -50),
		DropBadMinTrades: integer("DROP_BAD_MIN_TRADES", 15),

		DiscoveryInterval:  dur("DISCOVERY_INTERVAL", 10*time.Minute),
		DiscoveryMult:      flt("DISCOVERY_MULT", 5),
		DiscoveryEarlyMult: flt("DISCOVERY_EARLY_MULT", 1.5),
		DiscoveryExitMult:  flt("DISCOVERY_EXIT_MULT", 2),
		DiscoveryMinHits:   integer("DISCOVERY_MIN_HITS", 2),

		APIAddr: str("API_ADDR", ":8080"),

		LiveTrading:           boolean("LIVE_TRADING", false),
		TraderPrivateKey:      str("TRADER_PRIVATE_KEY", ""),
		SolanaRPCURL:          str("SOLANA_RPC_URL", "https://api.mainnet-beta.solana.com"),
		ExecSizeUSD:           flt("EXEC_SIZE_USD", 25),
		ExecSlippagePct:       flt("EXEC_SLIPPAGE_PCT", 15),
		ExecPriorityFee:       flt("EXEC_PRIORITY_FEE_SOL", 0.0005),
		JitoTipSol:            flt("JITO_TIP_SOL", 0),
		JitoURL:               str("JITO_URL", "https://mainnet.block-engine.jito.wtf/api/v1/bundles"),
		MaxOpenPositions:      integer("MAX_OPEN_POSITIONS", 5),
		StopLossPct:           flt("STOP_LOSS_PCT", 40),
		DailyLossLimitUSD:     flt("DAILY_LOSS_LIMIT_USD", 75),
		ExecMaxAttempts:       integer("EXEC_MAX_ATTEMPTS", 3),
		PumpPortalTradeURL:    str("PUMPPORTAL_TRADE_URL", "https://pumpportal.fun/api/trade-local"),
		PumpPortalLocalFeePct: flt("PUMPPORTAL_LOCAL_FEE_PCT", 0.5),

		DiscordWebhookURL: str("DISCORD_WEBHOOK_URL", ""),
		TelegramBotToken:  str("TELEGRAM_BOT_TOKEN", ""),
		TelegramChatID:    str("TELEGRAM_CHAT_ID", ""),
	}
}

func loadDotEnv() {
	dir, err := os.Getwd()
	if err != nil {
		return
	}
	for i := 0; i < 5; i++ {
		p := filepath.Join(dir, ".env")
		if _, err := os.Stat(p); err == nil {
			_ = godotenv.Load(p) // never overrides real env vars
			return
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return
		}
		dir = parent
	}
}

func str(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func flt(k string, def float64) float64 {
	if f, err := strconv.ParseFloat(str(k, ""), 64); err == nil {
		return f
	}
	return def
}

func integer(k string, def int) int {
	if n, err := strconv.Atoi(str(k, "")); err == nil {
		return n
	}
	return def
}

func boolean(k string, def bool) bool {
	if b, err := strconv.ParseBool(str(k, "")); err == nil {
		return b
	}
	return def
}

func dur(k string, def time.Duration) time.Duration {
	if d, err := time.ParseDuration(str(k, "")); err == nil {
		return d
	}
	return def
}
