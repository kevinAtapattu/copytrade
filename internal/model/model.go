// Package model holds the shared domain types passed between services.
package model

import "time"

// Wallet statuses.
const (
	StatusLive    = "live"
	StatusBench   = "bench"
	StatusDropped = "dropped"
	// StatusPaused: temporarily not tracked (over the daily trade limit). Stays
	// visible and returns to bench automatically at the next UTC midnight.
	StatusPaused = "paused"
)

// Wallet sources.
const (
	SourceSeed      = "seed"
	SourceManual    = "manual"
	SourceDiscovery = "discovery"
)

// Trade sides.
const (
	SideBuy  = "buy"
	SideSell = "sell"
)

// Delays (seconds) we simulate copies at.
var Delays = []int{2, 10, 60}

type Wallet struct {
	Address     string     `json:"address"`
	Label       string     `json:"label"`
	Status      string     `json:"status"`
	Source      string     `json:"source"`
	Reason      string     `json:"reason"`
	AddedAt     time.Time  `json:"added_at"`
	StatusAt    time.Time  `json:"status_at"`
	LastTradeAt *time.Time `json:"last_trade_at,omitempty"`
}

// Trade is one pump.fun trade as seen on the stream. Reserves are the
// virtual reserves of the bonding curve after the trade.
type Trade struct {
	Signature       string    `json:"signature"`
	Mint            string    `json:"mint"`
	Trader          string    `json:"trader"`
	Side            string    `json:"side"`
	SolAmount       float64   `json:"sol_amount"`
	TokenAmount     float64   `json:"token_amount"`
	NewTokenBalance float64   `json:"new_token_balance"`
	VSol            float64   `json:"v_sol"`
	VTokens         float64   `json:"v_tokens"`
	MarketCapSol    float64   `json:"market_cap_sol"`
	Pool            string    `json:"pool"`
	BondingCurveKey string    `json:"bonding_curve_key"`
	ReceivedAt      time.Time `json:"received_at"`
	IsTarget        bool      `json:"is_target"`
}

// Price is SOL per token implied by the post-trade reserves.
func (t Trade) Price() float64 {
	if t.VTokens <= 0 {
		return 0
	}
	return t.VSol / t.VTokens
}

// Token is our running view of a mint we watch.
type Token struct {
	Mint        string    `json:"mint"`
	FirstSeen   time.Time `json:"first_seen"`
	FirstPrice  float64   `json:"first_price"`
	MaxPrice    float64   `json:"max_price"`
	LastPrice   float64   `json:"last_price"`
	VSol        float64   `json:"v_sol"`
	VTokens     float64   `json:"v_tokens"`
	Pool        string    `json:"pool"`
	LastTradeAt time.Time `json:"last_trade_at"`
	Migrated    bool      `json:"migrated"`
	Discovered  bool      `json:"discovered"`
	CurveKey    string    `json:"bonding_curve_key"`
}

// PoolPumpSwap marks a trade or price on PumpSwap, the AMM pump.fun tokens move
// to when they graduate. Priced like the curve (constant product), see curve.State.AMM.
const PoolPumpSwap = "pumpswap"

// Priced reports whether our SOL math can price trades on this pool.
func Priced(pool string) bool { return pool == "" || pool == "pump" || pool == PoolPumpSwap }

// SimTrade is one simulated copy fill.
type SimTrade struct {
	ID            string    `json:"id"`
	PositionID    string    `json:"position_id"`
	TargetSig     string    `json:"target_sig"`
	Wallet        string    `json:"wallet"`
	Mint          string    `json:"mint"`
	DelayS        int       `json:"delay_s"`
	Side          string    `json:"side"`
	TargetAt      time.Time `json:"target_at"`
	FillAt        time.Time `json:"fill_at"`
	SolAmount     float64   `json:"sol_amount"` // SOL spent (buy) or received (sell), net of all fees
	TokenAmount   float64   `json:"token_amount"`
	SpotPrice     float64   `json:"spot_price"`
	FillPrice     float64   `json:"fill_price"`
	TargetPrice   float64   `json:"target_price"`
	SlippagePct   float64   `json:"slippage_pct"`
	PumpFeeSol    float64   `json:"pump_fee_sol"`
	BotFeeSol     float64   `json:"bot_fee_sol"`
	NetworkFeeSol float64   `json:"network_fee_sol"`
	SolUSD        float64   `json:"sol_usd"`
	SellFraction  float64   `json:"sell_fraction,omitempty"`
	Skipped       bool      `json:"skipped"`
	SkipReason    string    `json:"skip_reason,omitempty"`
}

// Position statuses.
const (
	PosOpen     = "open"
	PosClosed   = "closed"
	PosMigrated = "migrated"
)

// SimPosition aggregates fills for one (wallet, mint, delay).
type SimPosition struct {
	ID           string     `json:"id"`
	Wallet       string     `json:"wallet"`
	Mint         string     `json:"mint"`
	DelayS       int        `json:"delay_s"`
	OpenedAt     time.Time  `json:"opened_at"`
	ClosedAt     *time.Time `json:"closed_at,omitempty"`
	Status       string     `json:"status"`
	CostSol      float64    `json:"cost_sol"`
	ProceedsSol  float64    `json:"proceeds_sol"`
	TokensHeld   float64    `json:"tokens_held"`
	TokensBought float64    `json:"tokens_bought"`
	SolUSD       float64    `json:"sol_usd"` // SOL/USD at entry, used to report USD PnL
}

// RealizedSol is proceeds minus cost. Only meaningful once closed.
func (p SimPosition) RealizedSol() float64 { return p.ProceedsSol - p.CostSol }

// MarkSol values the position at a given price (SOL/token), ignoring exit fees.
func (p SimPosition) MarkSol(price float64) float64 {
	return p.ProceedsSol + p.TokensHeld*price - p.CostSol
}

// Score is one copyability snapshot for a wallet, delay and window.
type Score struct {
	Wallet       string    `json:"wallet"`
	DelayS       int       `json:"delay_s"`
	WindowDays   int       `json:"window_days"`
	ComputedAt   time.Time `json:"computed_at"`
	Positions    int       `json:"positions"`
	ClosedCount  int       `json:"closed"`
	PnlUSD       float64   `json:"pnl_usd"`
	PnlExBestUSD float64   `json:"pnl_ex_best_usd"`
	BestTradeUSD float64   `json:"best_trade_usd"`
	WinRate      float64   `json:"win_rate"`
	MaxDrawdown  float64   `json:"max_drawdown"` // fraction of peak equity, 0.25 = -25%
	AvgHoldS     float64   `json:"avg_hold_s"`
	TradesPerDay float64   `json:"trades_per_day"`
	ActiveDays   float64   `json:"active_days"` // span between first and last observed position
	LuckyTrade   bool      `json:"lucky_trade"`
	Score        float64   `json:"score"`
}

// Rotation records a wallet status change.
type Rotation struct {
	ID         int64     `json:"id"`
	Wallet     string    `json:"wallet"`
	FromStatus string    `json:"from_status"`
	ToStatus   string    `json:"to_status"`
	Reason     string    `json:"reason"`
	At         time.Time `json:"at"`
}

// DiscoveryHit is an early buyer who exited well on a token that ran 5x+.
type DiscoveryHit struct {
	ID         int64     `json:"id"`
	Wallet     string    `json:"wallet"`
	Mint       string    `json:"mint"`
	TokenMult  float64   `json:"token_mult"`
	EntryPrice float64   `json:"entry_price"`
	ExitPrice  float64   `json:"exit_price"`
	ExitMult   float64   `json:"exit_mult"`
	ProfitSol  float64   `json:"profit_sol"`
	FoundAt    time.Time `json:"found_at"`
}

// LiveFill is an executor fill, paper or live.
type LiveFill struct {
	ID          string     `json:"id"`
	PositionID  string     `json:"position_id"`
	Mode        string     `json:"mode"` // paper | live
	TargetSig   string     `json:"target_sig"`
	Wallet      string     `json:"wallet"`
	Mint        string     `json:"mint"`
	Side        string     `json:"side"`
	Reason      string     `json:"reason"` // copy | stop_loss
	TargetAt    time.Time  `json:"target_at"`
	SentAt      time.Time  `json:"sent_at"`
	FilledAt    *time.Time `json:"filled_at,omitempty"`
	Signature   string     `json:"signature,omitempty"`
	Status      string     `json:"status"` // filled | failed | rejected
	Error       string     `json:"error,omitempty"`
	Attempts    int        `json:"attempts"`
	SolAmount   float64    `json:"sol_amount"`
	TokenAmount float64    `json:"token_amount"`
	FillPrice   float64    `json:"fill_price"`
	FeesSol     float64    `json:"fees_sol"`
	SolUSD      float64    `json:"sol_usd"`
	// What the simulator predicted for the same trade at the closest delay.
	SimFillPrice float64 `json:"sim_fill_price,omitempty"`
}

// LivePosition is an executor position.
type LivePosition struct {
	ID          string     `json:"id"`
	Mode        string     `json:"mode"`
	Wallet      string     `json:"wallet"`
	Mint        string     `json:"mint"`
	OpenedAt    time.Time  `json:"opened_at"`
	ClosedAt    *time.Time `json:"closed_at,omitempty"`
	Status      string     `json:"status"`
	CostSol     float64    `json:"cost_sol"`
	ProceedsSol float64    `json:"proceeds_sol"`
	TokensHeld  float64    `json:"tokens_held"`
	EntryPrice  float64    `json:"entry_price"`
	SolUSD      float64    `json:"sol_usd"`
}

// Command is a manual action from the dashboard, on the commands stream.
type Command struct {
	Kind       string    `json:"kind"` // CmdSellPosition
	PositionID string    `json:"position_id"`
	Pct        float64   `json:"pct"` // share of what is held, 0-100
	At         time.Time `json:"at"`
}

// CmdSellPosition asks the executor to sell part or all of an open position now.
const CmdSellPosition = "sell_position"

// Event is a notification on the events stream (UI + alerts).
type Event struct {
	Kind    string    `json:"kind"` // target_trade | sim_fill | live_fill | rotation | discovery | error | info
	At      time.Time `json:"at"`
	Service string    `json:"service"`
	Message string    `json:"message"`
	Data    any       `json:"data,omitempty"`
}
