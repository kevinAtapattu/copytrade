// Package history fetches a token's past trades straight from the chain, by
// decoding pump.fun's own TradeEvent from each transaction. Discovery uses it in
// snapshot mode, where we no longer stream every trade.
package history

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mr-tron/base58"

	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/solana"
)

var tradeEventDisc = func() []byte {
	h := sha256.Sum256([]byte("event:TradeEvent"))
	return h[:8]
}()

// Event is a decoded pump.fun TradeEvent.
type Event struct {
	Mint        string
	SolAmount   float64
	TokenAmount float64
	IsBuy       bool
	User        string
	Timestamp   int64
	VSol        float64
	VTokens     float64
	// Quote fields, from the extended event (2026). On curves priced in another
	// token (pool "nonsol") SolAmount and VSol are 0 and the trade's size is here.
	// QuoteAmount and VQuote are raw quote-token units / 1e9, whatever the quote
	// token's real decimals: the same scale as pumpswap.Event.Quote, so a coin's
	// curve and PumpSwap trades compare. On SOL curves they equal SolAmount/VSol
	// and QuoteMint is "". Zero when the event is too short to carry them.
	QuoteMint   string
	QuoteAmount float64
	VQuote      float64
}

const eventLen = 32 + 8 + 8 + 1 + 32 + 8 + 8 + 8 // fields we read; newer versions append more

// Extended layout after virtual_token_reserves (observed on mainnet, November 2026):
// real reserves, fee recipient, fees, creator, creator fees, volume tracking, then
// ix_name (u32 length + bytes) at 250. After the name: 37 bytes we don't use, the
// quote mint (32), quote_amount, virtual_quote_reserves, real_quote_reserves (u64).
const (
	ixNameAt     = 250
	quoteMintOff = 37 // from the end of ix_name
)

// DecodeEvent decodes a TradeEvent body that starts right after the 8-byte discriminator.
func DecodeEvent(b []byte) (Event, error) {
	if len(b) < eventLen {
		return Event{}, errors.New("trade event too short")
	}
	u := func(o int) uint64 { return binary.LittleEndian.Uint64(b[o:]) }
	e := Event{
		Mint:        base58.Encode(b[0:32]),
		SolAmount:   float64(u(32)) / 1e9,
		TokenAmount: float64(u(40)) / 1e6,
		IsBuy:       b[48] != 0,
		User:        base58.Encode(b[49:81]),
		Timestamp:   int64(u(81)),
		VSol:        float64(u(89)) / 1e9,
		VTokens:     float64(u(97)) / 1e6,
	}
	if len(b) >= ixNameAt+4 {
		n := int(binary.LittleEndian.Uint32(b[ixNameAt:]))
		q := ixNameAt + 4 + n + quoteMintOff
		if n <= 64 && len(b) >= q+32+3*8 {
			e.QuoteAmount = float64(u(q+32)) / 1e9
			e.VQuote = float64(u(q+40)) / 1e9
			for _, c := range b[q : q+32] {
				if c != 0 {
					e.QuoteMint = base58.Encode(b[q : q+32])
					break
				}
			}
		}
	}
	return e, nil
}

// EncodeEvent is the inverse of DecodeEvent including the discriminator (tests, fixtures).
func EncodeEvent(e Event) []byte {
	b := make([]byte, 8+eventLen)
	copy(b, tradeEventDisc)
	m, _ := base58.Decode(e.Mint)
	copy(b[8:40], m)
	binary.LittleEndian.PutUint64(b[40:], uint64(e.SolAmount*1e9+0.5))
	binary.LittleEndian.PutUint64(b[48:], uint64(e.TokenAmount*1e6+0.5))
	if e.IsBuy {
		b[56] = 1
	}
	u, _ := base58.Decode(e.User)
	copy(b[57:89], u)
	binary.LittleEndian.PutUint64(b[89:], uint64(e.Timestamp))
	binary.LittleEndian.PutUint64(b[97:], uint64(e.VSol*1e9+0.5))
	binary.LittleEndian.PutUint64(b[105:], uint64(e.VTokens*1e6+0.5))
	return b
}

// EventsFromTx finds pump.fun TradeEvents in program logs ("Program data: <base64>")
// and in inner instructions (Anchor emit_cpi: 8-byte event tag + discriminator + body).
// Only data emitted by the pump program counts: trading terminals define their own
// "TradeEvent" with the same Anchor discriminator, and decoding theirs as pump's
// yields junk (zero SOL, zero reserves).
func EventsFromTx(tx solana.TxEvents) []Event {
	var blobs [][]byte
	blobs = append(blobs, PumpLogData(tx.Logs)...)
	scoped := len(tx.InnerPrograms) == len(tx.InnerData)
	for i, b := range tx.InnerData {
		if !scoped || tx.InnerPrograms[i] == curve.PumpProgram {
			blobs = append(blobs, b)
		}
	}
	var out []Event
	seen := map[string]bool{}
	for _, b := range blobs {
		for _, off := range []int{0, 8} {
			if len(b) < off+8 || string(b[off:off+8]) != string(tradeEventDisc) {
				continue
			}
			ev, err := DecodeEvent(b[off+8:])
			if err != nil {
				continue
			}
			k := fmt.Sprintf("%s|%v|%v|%v|%d", ev.User, ev.SolAmount, ev.TokenAmount, ev.IsBuy, ev.Timestamp)
			if !seen[k] { // the same event can appear in both logs and inner instructions
				seen[k] = true
				out = append(out, ev)
			}
		}
	}
	return out
}

// PumpLogData returns the decoded "Program data:" payloads emitted while the pump
// program was the executing frame. Logs without invoke lines (fixtures) are unscoped.
func PumpLogData(logs []string) [][]byte { return ProgramLogData(logs, curve.PumpProgram) }

// ProgramLogData returns the "Program data:" payloads emitted while program was
// the innermost running program. Unframed logs (tests) are returned whole.
func ProgramLogData(logs []string, program string) [][]byte {
	framed := false
	for _, l := range logs {
		if strings.HasPrefix(l, "Program ") && strings.Contains(l, " invoke [") {
			framed = true
			break
		}
	}
	var stack []string
	var out [][]byte
	for _, l := range logs {
		switch {
		case strings.HasPrefix(l, "Program data: "):
			if framed && (len(stack) == 0 || stack[len(stack)-1] != program) {
				continue
			}
			if b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(l, "Program data: ")); err == nil {
				out = append(out, b)
			}
		case strings.HasPrefix(l, "Program ") && strings.Contains(l, " invoke ["):
			stack = append(stack, strings.Fields(l)[1])
		case strings.HasPrefix(l, "Program ") && (strings.HasSuffix(l, " success") || strings.Contains(l, " failed")):
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		}
	}
	return out
}

// Fetcher pulls a token's earliest trades from its bonding curve's history.
type Fetcher struct {
	RPC      *solana.RPC
	MaxPages int // getSignaturesForAddress pages of 1000 to walk back (cost: 1 call each)
	MaxTx    int // earliest transactions to decode (cost: 1 call each)
}

// TokenTrades returns up to MaxTx of the token's earliest trades, oldest first.
// For very busy tokens the walk back stops after MaxPages, so "earliest" is the
// earliest we reached.
func (f Fetcher) TokenTrades(ctx context.Context, tok model.Token) ([]model.Trade, error) {
	key := tok.CurveKey
	if key == "" {
		var err error
		if key, err = curve.DeriveBondingCurve(tok.Mint); err != nil {
			return nil, err
		}
	}
	var sigs []solana.SigInfo
	before := ""
	reachedStart := false
	for page := 0; page < max(f.MaxPages, 1); page++ {
		got, err := f.RPC.GetSignaturesForAddress(ctx, key, before, 1000)
		if err != nil {
			return nil, err
		}
		sigs = append(sigs, got...)
		if len(got) < 1000 {
			reachedStart = true
			break
		}
		before = got[len(got)-1].Signature
	}
	if tok.Migrated && !reachedStart {
		// A graduated token's "earliest" trades must really be its first ones; if the
		// walk can't reach launch, skip it rather than spend calls on mid-life trades.
		return nil, nil
	}
	// newest first -> oldest first, successful only
	var ordered []solana.SigInfo
	for i := len(sigs) - 1; i >= 0; i-- {
		if sigs[i].Err == nil {
			ordered = append(ordered, sigs[i])
		}
	}
	if f.MaxTx > 0 && len(ordered) > f.MaxTx {
		ordered = ordered[:f.MaxTx]
	}
	var trades []model.Trade
	for _, s := range ordered {
		tx, err := f.RPC.GetTxEvents(ctx, s.Signature)
		if err != nil {
			return trades, err
		}
		if tx.Failed {
			continue
		}
		trades = append(trades, ToTrades(tx, tok.Mint, key)...)
	}
	return trades, nil
}

// ToTrades converts a transaction's TradeEvents for mint into model.Trades.
func ToTrades(tx solana.TxEvents, mint, curveKey string) []model.Trade {
	var out []model.Trade
	for i, ev := range EventsFromTx(tx) {
		if ev.Mint != mint {
			continue
		}
		side := model.SideSell
		if ev.IsBuy {
			side = model.SideBuy
		}
		at := tx.BlockTime
		if ev.Timestamp > 0 {
			at = timeUnix(ev.Timestamp)
		}
		out = append(out, model.Trade{
			Signature: fmt.Sprintf("%s#%d", tx.Signature, i), Mint: mint, Trader: ev.User, Side: side,
			SolAmount: ev.SolAmount, TokenAmount: ev.TokenAmount, VSol: ev.VSol, VTokens: ev.VTokens,
			Pool: "pump", BondingCurveKey: curveKey, ReceivedAt: at,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ReceivedAt.Before(out[j].ReceivedAt) })
	return out
}

func timeUnix(s int64) time.Time { return time.Unix(s, 0).UTC() }

// Static serves token history from a recorded trade list (replay and tests).
type Static struct{ byMint map[string][]model.Trade }

func NewStatic(trades []model.Trade) *Static {
	s := &Static{byMint: map[string][]model.Trade{}}
	for _, t := range trades {
		s.byMint[t.Mint] = append(s.byMint[t.Mint], t)
	}
	return s
}

func (s *Static) TokenTrades(_ context.Context, tok model.Token) ([]model.Trade, error) {
	return s.byMint[tok.Mint], nil
}

// IsTradeEvent reports whether b (8 bytes) is pump's TradeEvent discriminator.
func IsTradeEvent(b []byte) bool { return len(b) == 8 && string(b) == string(tradeEventDisc) }
