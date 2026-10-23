// Package source abstracts where trades come from. PumpPortal websocket today;
// a Helius gRPC / LaserStream source can implement the same interface later.
package source

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"copytrade/internal/model"
)

// Handler receives each trade. It is called from the source's own goroutine and
// may call Subscribe/Unsubscribe methods without blocking.
type Handler func(t model.Trade)

type Source interface {
	Run(ctx context.Context, h Handler) error
	SubscribeAccounts(keys []string)
	UnsubscribeAccounts(keys []string)
	SubscribeTokens(keys []string)
	UnsubscribeTokens(keys []string)
	// Pause drops the connection until Resume (spending guard).
	Pause()
	Resume()
	Stats() Stats
}

type Stats struct {
	Connected     bool      `json:"connected"`
	Reconnects    int       `json:"reconnects"`
	LastMessageAt time.Time `json:"last_message_at"`
	LastError     string    `json:"last_error,omitempty"`
	Accounts      int       `json:"accounts"`
	Tokens        int       `json:"tokens"`
	Messages      int64     `json:"messages"` // metered trade messages received
}

// flexFloat accepts JSON numbers or numeric strings.
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return nil // tolerate junk, field stays 0
	}
	*f = flexFloat(v)
	return nil
}

type ppTrade struct {
	Signature             string    `json:"signature"`
	Mint                  string    `json:"mint"`
	TraderPublicKey       string    `json:"traderPublicKey"`
	TxType                string    `json:"txType"`
	TokenAmount           flexFloat `json:"tokenAmount"`
	SolAmount             flexFloat `json:"solAmount"`
	NewTokenBalance       flexFloat `json:"newTokenBalance"`
	BondingCurveKey       string    `json:"bondingCurveKey"`
	VTokensInBondingCurve flexFloat `json:"vTokensInBondingCurve"`
	VSolInBondingCurve    flexFloat `json:"vSolInBondingCurve"`
	MarketCapSol          flexFloat `json:"marketCapSol"`
	Pool                  string    `json:"pool"`
	Timestamp             flexFloat `json:"timestamp"` // not documented; used if present (ms)
}

// ParsePumpPortal turns one websocket message into a Trade. ok is false for
// non-trade messages (subscription acks, errors, token creations).
func ParsePumpPortal(raw []byte, receivedAt time.Time) (model.Trade, bool) {
	var p ppTrade
	if err := json.Unmarshal(raw, &p); err != nil {
		return model.Trade{}, false
	}
	side := strings.ToLower(p.TxType)
	if p.Signature == "" || p.Mint == "" || p.TraderPublicKey == "" || (side != model.SideBuy && side != model.SideSell) {
		return model.Trade{}, false
	}
	pool := p.Pool
	if pool == "" {
		pool = "pump"
	}
	t := model.Trade{
		Signature: p.Signature, Mint: p.Mint, Trader: p.TraderPublicKey, Side: side,
		SolAmount: float64(p.SolAmount), TokenAmount: float64(p.TokenAmount), NewTokenBalance: float64(p.NewTokenBalance),
		VSol: float64(p.VSolInBondingCurve), VTokens: float64(p.VTokensInBondingCurve), MarketCapSol: float64(p.MarketCapSol),
		Pool: pool, BondingCurveKey: p.BondingCurveKey, ReceivedAt: receivedAt.UTC(),
	}
	// SOL amount is sometimes absent; derive from market cap is not possible, so leave it.
	return t, true
}

// EncodePumpPortal is the inverse of ParsePumpPortal, used to build fixtures.
func EncodePumpPortal(t model.Trade) []byte {
	b, _ := json.Marshal(map[string]any{
		"signature": t.Signature, "mint": t.Mint, "traderPublicKey": t.Trader, "txType": t.Side,
		"tokenAmount": t.TokenAmount, "solAmount": t.SolAmount, "newTokenBalance": t.NewTokenBalance,
		"bondingCurveKey": t.BondingCurveKey, "vTokensInBondingCurve": t.VTokens, "vSolInBondingCurve": t.VSol,
		"marketCapSol": t.MarketCapSol, "pool": t.Pool,
	})
	return b
}
