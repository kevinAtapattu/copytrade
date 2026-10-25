// Package pumpswap decodes trades on PumpSwap, the AMM pump.fun tokens move to
// when they graduate, and prices its pools.
//
// Layouts were checked against mainnet in October 2026 (pool 4xMCDt7E..., a
// SellEvent and a BuyEvent): event reserves are the pool's vault balances before
// the trade, and the pool account carries a virtual SOL reserve (~17.58 SOL) that
// the constant-product math adds to the SOL vault.
package pumpswap

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/mr-tron/base58"

	"copytrade/internal/curve"
	"copytrade/internal/history"
	"copytrade/internal/solana"
)

const (
	Program = "pAMMBay6oceH9fJKBRHGP5D4bD4sWpmSwMn52FMfXEA"
	WSOL    = "So11111111111111111111111111111111111111112"
	// TokenDecimals: pump.fun tokens use 6 decimals; SOL uses 9.
	tokenUnit = 1e6
	solUnit   = 1e9
)

var (
	sellDisc = disc("SellEvent")
	buyDisc  = disc("BuyEvent")
	// emit_cpi wraps events in a self-invocation prefixed with this tag.
	cpiTag = []byte{0xe4, 0x45, 0xa5, 0x2e, 0x51, 0xcb, 0x9a, 0x1d}
)

func disc(name string) string {
	h := sha256.Sum256([]byte("event:" + name))
	return string(h[:8])
}

// Event is one PumpSwap trade. Amounts are UI units (tokens, SOL).
type Event struct {
	Pool, User string
	IsBuy      bool
	Timestamp  int64
	Base       float64 // tokens bought or sold
	Quote      float64 // SOL into (buy) or out of (sell) the pool, before the user-side fees
	UserQuote  float64 // SOL the user actually paid (buy) or received (sell)
	PoolBase   float64 // pool token vault before the trade
	PoolQuote  float64 // pool SOL vault before the trade
	LPFee      float64 // SOL that stays in the pool
}

const fixedLen = 14*8 + 2*32 // 14 u64 fields, then pool and user

// DecodeEvent decodes a SellEvent or BuyEvent (discriminator included).
func DecodeEvent(b []byte) (Event, error) {
	if len(b) < 8+fixedLen {
		return Event{}, errors.New("pumpswap event too short")
	}
	d := string(b[:8])
	if d != sellDisc && d != buyDisc {
		return Event{}, errors.New("not a pumpswap trade event")
	}
	u := func(i int) uint64 { return binary.LittleEndian.Uint64(b[8+i*8:]) }
	e := Event{IsBuy: d == buyDisc, Timestamp: int64(u(0))}
	e.Base = float64(u(1)) / tokenUnit
	e.PoolBase = float64(u(5)) / tokenUnit
	e.PoolQuote = float64(u(6)) / solUnit
	e.Quote = float64(u(7)) / solUnit // quote_amount_in / quote_amount_out
	e.LPFee = float64(u(9)) / solUnit
	e.UserQuote = float64(u(13)) / solUnit // user_quote_amount_in / user_quote_amount_out
	e.Pool = base58.Encode(b[8+112 : 8+144])
	e.User = base58.Encode(b[8+144 : 8+176])
	return e, nil
}

// FeePct is the all-in fee the user paid on this trade, percent of the SOL side.
func (e Event) FeePct() float64 {
	if e.Quote <= 0 {
		return 0
	}
	return math.Abs(e.UserQuote-e.Quote) / e.Quote * 100
}

// After is the pool state right after the trade, given the pool's virtual SOL.
func (e Event) After(virtualQuote float64) curve.State {
	base, quote := e.PoolBase, e.PoolQuote
	if e.IsBuy {
		base -= e.Base
		quote += e.Quote + e.LPFee
	} else {
		base += e.Base
		quote -= e.Quote - e.LPFee
	}
	return curve.State{VSol: quote + virtualQuote, VTokens: base, AMM: true}
}

// EventsFromLogs decodes events from "Program data:" lines emitted by PumpSwap.
func EventsFromLogs(logs []string) []Event {
	var out []Event
	for _, b := range history.ProgramLogData(logs, Program) {
		if e, err := DecodeEvent(b); err == nil {
			out = append(out, e)
		}
	}
	return out
}

// EventsFromTx decodes events from logs and, when logs were truncated, from
// PumpSwap's emit_cpi inner instructions. The same event can appear in both.
func EventsFromTx(tx solana.TxEvents) []Event {
	out := EventsFromLogs(tx.Logs)
	seen := map[string]bool{}
	for _, e := range out {
		seen[key(e)] = true
	}
	scoped := len(tx.InnerPrograms) == len(tx.InnerData)
	for i, b := range tx.InnerData {
		if !scoped || tx.InnerPrograms[i] != Program || len(b) < 16 || string(b[:8]) != string(cpiTag) {
			continue
		}
		if e, err := DecodeEvent(b[8:]); err == nil && !seen[key(e)] {
			seen[key(e)] = true
			out = append(out, e)
		}
	}
	return out
}

func key(e Event) string {
	return fmt.Sprintf("%s|%s|%v|%v|%v|%d", e.Pool, e.User, e.IsBuy, e.Base, e.Quote, e.Timestamp)
}

// Mentions reports whether logs show PumpSwap ran.
func Mentions(logs []string) bool {
	for _, l := range logs {
		if strings.Contains(l, Program) {
			return true
		}
	}
	return false
}

// Pool is the part of a pool account we use.
type Pool struct {
	BaseMint, QuoteMint   string
	BaseVault, QuoteVault string
	VirtualQuote          float64 // SOL the math adds to the SOL vault
}

// ParsePool parses a PumpSwap pool account.
// Layout: disc 8, bump 1, index 2, creator 32, base_mint 32, quote_mint 32,
// lp_mint 32, base vault 32, quote vault 32, lp_supply 8, coin_creator 32,
// two flag bytes, virtual quote reserves u64 at 245.
func ParsePool(d []byte) (Pool, error) {
	if len(d) < 203 {
		return Pool{}, errors.New("pumpswap pool account too short")
	}
	pk := func(off int) string { return base58.Encode(d[off : off+32]) }
	p := Pool{BaseMint: pk(43), QuoteMint: pk(75), BaseVault: pk(139), QuoteVault: pk(171)}
	if len(d) >= 253 {
		p.VirtualQuote = float64(binary.LittleEndian.Uint64(d[245:])) / solUnit
	}
	return p, nil
}

// SOLQuoted reports whether our SOL math can price this pool.
func (p Pool) SOLQuoted() bool { return p.QuoteMint == WSOL && p.BaseMint != WSOL }

// State prices the pool from its two vault token accounts.
func (p Pool) State(baseVault, quoteVault []byte) (curve.State, error) {
	b, err1 := tokenAmount(baseVault)
	q, err2 := tokenAmount(quoteVault)
	if err1 != nil || err2 != nil {
		return curve.State{}, errors.New("bad pumpswap vault account")
	}
	return curve.State{VSol: float64(q)/solUnit + p.VirtualQuote, VTokens: float64(b) / tokenUnit, AMM: true}, nil
}

// tokenAmount reads an SPL token account's amount (u64 at offset 64).
func tokenAmount(d []byte) (uint64, error) {
	if len(d) < 72 {
		return 0, errors.New("token account too short")
	}
	return binary.LittleEndian.Uint64(d[64:]), nil
}

// DerivePool is the canonical pool pump.fun creates when mint graduates:
// creator = pump PDA ["pool-authority", mint]; pool = PumpSwap PDA
// ["pool", u16 index 0, creator, mint, WSOL].
func DerivePool(mint string) (string, error) {
	m, err := base58.Decode(mint)
	if err != nil || len(m) != 32 {
		return "", errors.New("bad mint address")
	}
	auth, err := curve.FindPDA([][]byte{[]byte("pool-authority"), m}, curve.PumpProgram)
	if err != nil {
		return "", err
	}
	wsol, _ := base58.Decode(WSOL)
	pool, err := curve.FindPDA([][]byte{[]byte("pool"), {0, 0}, auth, m, wsol}, Program)
	if err != nil {
		return "", err
	}
	return base58.Encode(pool), nil
}
