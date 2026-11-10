// Package exec copies live wallets: paper by default, real transactions only
// when LIVE_TRADING=true.
package exec

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/mr-tron/base58"

	"copytrade/internal/curve"
	"copytrade/internal/solana"
)

// Result is one executed trade.
type Result struct {
	Signature string
	SolAmount float64 // buy: SOL spent (all-in); sell: SOL received (net)
	Tokens    float64
	FeesSol   float64
	FilledAt  time.Time
	Attempts  int
}

// Order is what the executor wants done.
type Order struct {
	Mint    string
	Side    string  // buy | sell
	SolIn   float64 // buys
	Tokens  float64 // sells: token amount we expect to sell
	SellPct float64 // sells: percent of our balance (0-100); 0 sells exactly Tokens
	State   curve.State
	At      time.Time // event time (paper fills use this)
}

type Trader interface {
	Execute(ctx context.Context, o Order) (Result, error)
	Mode() string
}

// Paper fills against the current curve state with the same fees a live trade pays.
type Paper struct {
	Fees curve.Fees
}

func (p Paper) Mode() string { return "paper" }

func (p Paper) Execute(_ context.Context, o Order) (Result, error) {
	var f curve.Fill
	var err error
	if o.Side == "buy" {
		f, err = curve.Buy(o.State, o.SolIn, p.Fees)
	} else {
		f, err = curve.Sell(o.State, o.Tokens, p.Fees)
	}
	if err != nil {
		return Result{Attempts: 1}, err
	}
	return Result{SolAmount: f.SolAmount, Tokens: f.Tokens, FeesSol: f.PumpFeeSol + f.BotFeeSol + f.NetworkSol,
		FilledAt: o.At, Attempts: 1}, nil
}

// Live builds the tx with PumpPortal's local trade API, signs it locally, sends it
// through our RPC (or Jito), waits for confirmation, and reads the real fill.
// Buys on graduated (PumpSwap) tokens are built with Jupiter instead: PumpPortal
// prices them without the pool's virtual SOL reserve, so on a fresh pool its max
// spend is far too low and the buy fails on-chain (see docs/DECISIONS.md).
type Live struct {
	Key         solana.Keypair
	RPC         *solana.RPC
	TradeURL    string
	SlippagePct float64
	PriorityFee float64
	JitoTipSol  float64
	JitoURL     string
	JupiterURL  string // swap API base, e.g. https://lite-api.jup.ag/swap/v1; "" = PumpPortal for everything
	MaxAttempts int
	HTTP        *http.Client
	ConfirmWait time.Duration
}

func (l *Live) Mode() string { return "live" }

type tradeReq struct {
	PublicKey        string  `json:"publicKey"`
	Action           string  `json:"action"`
	Mint             string  `json:"mint"`
	Amount           any     `json:"amount"`
	DenominatedInSol string  `json:"denominatedInSol"`
	Slippage         float64 `json:"slippage"`
	PriorityFee      float64 `json:"priorityFee"`
	Pool             string  `json:"pool"`
}

func (l *Live) request(o Order) tradeReq {
	r := tradeReq{PublicKey: l.Key.PublicBase58(), Action: o.Side, Mint: o.Mint, Slippage: l.SlippagePct,
		PriorityFee: l.PriorityFee, Pool: "auto"} // auto: still works after the token migrates off the curve
	if o.Side == "buy" {
		r.Amount, r.DenominatedInSol = round(o.SolIn, 6), "true"
	} else if o.SellPct <= 0 {
		// Exact count, rounded down to pump.fun's 6 decimals so we never ask for more than we hold.
		r.Amount, r.DenominatedInSol = strconv.FormatFloat(math.Floor(o.Tokens*1e6)/1e6, 'f', 6, 64), "false"
	} else {
		pct := o.SellPct
		if pct >= 99.9 {
			pct = 100
		}
		r.Amount, r.DenominatedInSol = strconv.FormatFloat(pct, 'f', 2, 64)+"%", "false"
	}
	if l.JitoTipSol > 0 {
		r.PriorityFee = l.JitoTipSol // for bundles the first tx's priorityFee is the Jito tip
	}
	return r
}

// build asks for a fresh transaction (fresh blockhash every call) for owner.
func (l *Live) build(ctx context.Context, o Order, owner string) (*solana.Tx, error) {
	if l.jupiter(o) {
		return l.buildJupiter(ctx, o, owner)
	}
	req := l.request(o)
	req.PublicKey = owner
	var body []byte
	if l.JitoTipSol > 0 {
		body, _ = json.Marshal([]tradeReq{req})
	} else {
		body, _ = json.Marshal(req)
	}
	data, err := l.call(ctx, http.MethodPost, l.TradeURL, body)
	if err != nil {
		return nil, fmt.Errorf("trade-local %w", err)
	}
	if l.JitoTipSol > 0 {
		var txs []string
		if err := json.Unmarshal(data, &txs); err != nil || len(txs) == 0 {
			return nil, fmt.Errorf("trade-local bundle response: %v", err)
		}
		if data, err = base58.Decode(txs[0]); err != nil {
			return nil, err
		}
	}
	return solana.ParseTx(data)
}

func (l *Live) jupiter(o Order) bool { return o.Side == "buy" && o.State.AMM && l.JupiterURL != "" }

// buildJupiter gets a quote for SolIn SOL -> mint and the swap tx for it. Jupiter
// is sent through our RPC even when Jito is on (its tx carries no tip).
func (l *Live) buildJupiter(ctx context.Context, o Order, owner string) (*solana.Tx, error) {
	q := url.Values{"inputMint": {wsol}, "outputMint": {o.Mint}, "amount": {strconv.FormatInt(int64(o.SolIn*solana.LamportsPerSol), 10)},
		"slippageBps": {strconv.Itoa(int(l.SlippagePct * 100))}}
	quote, err := l.call(ctx, http.MethodGet, l.JupiterURL+"/quote?"+q.Encode(), nil)
	if err != nil {
		return nil, fmt.Errorf("jupiter quote: %w", err)
	}
	body, _ := json.Marshal(map[string]any{"quoteResponse": json.RawMessage(quote), "userPublicKey": owner, "wrapAndUnwrapSol": true,
		"dynamicComputeUnitLimit": true, "prioritizationFeeLamports": map[string]any{"priorityLevelWithMaxLamports": map[string]any{
			"maxLamports": int64(l.PriorityFee * solana.LamportsPerSol), "priorityLevel": "veryHigh"}}})
	data, err := l.call(ctx, http.MethodPost, l.JupiterURL+"/swap", body)
	if err != nil {
		return nil, fmt.Errorf("jupiter swap: %w", err)
	}
	var sr struct {
		SwapTransaction string `json:"swapTransaction"`
	}
	if err := json.Unmarshal(data, &sr); err != nil || sr.SwapTransaction == "" {
		return nil, fmt.Errorf("jupiter swap: no transaction: %s", truncate(string(data), 200))
	}
	raw, err := base64.StdEncoding.DecodeString(sr.SwapTransaction)
	if err != nil {
		return nil, err
	}
	return solana.ParseTx(raw)
}

const wsol = "So11111111111111111111111111111111111111112"

func (l *Live) call(ctx context.Context, method, u string, body []byte) ([]byte, error) {
	hr, _ := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	hr.Header.Set("Content-Type", "application/json")
	resp, err := l.HTTP.Do(hr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", resp.Status, truncate(string(data), 200))
	}
	return data, nil
}

func (l *Live) send(ctx context.Context, tx *solana.Tx, jito bool) error {
	if jito {
		_, err := solana.SendJitoBundle(ctx, l.HTTP, l.JitoURL, []string{base58.Encode(tx.Raw)})
		return err
	}
	_, err := l.RPC.SendTransaction(ctx, tx.Raw)
	return err
}

// Execute retries with a freshly built tx. A retry is only sent once the previous
// tx has failed on-chain or its blockhash has expired, so we never double-fill.
func (l *Live) Execute(ctx context.Context, o Order) (Result, error) {
	var lastErr error
	attempts := max(l.MaxAttempts, 1)
	for i := 1; i <= attempts; i++ {
		res, err := l.once(ctx, o)
		res.Attempts = i
		if err == nil {
			return res, nil
		}
		lastErr = err
		var fatal *fatalErr
		if errors.As(err, &fatal) || ctx.Err() != nil {
			break
		}
	}
	return Result{Attempts: attempts}, lastErr
}

type fatalErr struct{ error }

func (l *Live) once(ctx context.Context, o Order) (Result, error) {
	tx, err := l.build(ctx, o, l.Key.PublicBase58())
	if err != nil {
		return Result{}, err
	}
	if err := tx.Sign(l.Key); err != nil {
		return Result{}, &fatalErr{err}
	}
	sig := tx.Signature()
	if err := l.send(ctx, tx, l.JitoTipSol > 0 && !l.jupiter(o)); err != nil {
		var rej *solana.RPCError
		if errors.As(err, &rej) {
			return Result{Signature: sig}, err // node definitely rejected it: safe to rebuild now
		}
		// Transport error: the tx may still have been forwarded. Wait until it
		// lands or its blockhash expires before allowing a retry.
		slog.Warn("send failed, waiting for landing or expiry before retry", "sig", sig, "err", err)
	}
	landed, err := l.confirm(ctx, sig, tx.BlockhashBase58())
	if err != nil {
		return Result{Signature: sig}, err
	}
	if !landed {
		return Result{Signature: sig}, errors.New("not confirmed before blockhash expired")
	}
	bc, err := l.RPC.GetBalanceChange(ctx, sig, l.Key.PublicBase58(), o.Mint)
	if err != nil {
		// Landed but unreadable: do not retry (that would double the trade).
		return Result{Signature: sig, FilledAt: time.Now()}, &fatalErr{fmt.Errorf("confirmed but could not read fill: %w", err)}
	}
	r := Result{Signature: sig, FeesSol: bc.FeeSol, FilledAt: time.Now()}
	if o.Side == "buy" {
		r.SolAmount, r.Tokens = -bc.SolDelta, bc.TokenDelta
	} else {
		r.SolAmount, r.Tokens = bc.SolDelta, -bc.TokenDelta
	}
	return r, nil
}

// DryRun builds the exact transaction Execute would send for o and simulates it
// instead of sending it, so nothing is spent. With as == "" it is built for and
// signed by our key (checks signing; fails on funds if the key has no SOL). With
// another funded wallet's address it is built for that wallet and left unsigned,
// which checks the trade itself would go through.
func (l *Live) DryRun(ctx context.Context, o Order, as string) (solana.SimResult, error) {
	owner := as
	if owner == "" {
		owner = l.Key.PublicBase58()
	}
	tx, err := l.build(ctx, o, owner)
	if err != nil {
		return solana.SimResult{}, err
	}
	if as == "" {
		if err := tx.Sign(l.Key); err != nil {
			return solana.SimResult{}, err
		}
	}
	return l.RPC.SimulateTransaction(ctx, tx.Raw, as == "")
}

// confirm polls until the tx is confirmed (true), failed on-chain (error), or
// its blockhash is no longer valid and it never landed (false).
func (l *Live) confirm(ctx context.Context, sig, blockhash string) (bool, error) {
	wait := l.ConfirmWait
	if wait <= 0 {
		wait = 30 * time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		st, err := l.RPC.SignatureStatus(ctx, sig)
		if err == nil && st.Found {
			if st.Err != "" {
				return false, fmt.Errorf("tx failed on-chain: %s", st.Err)
			}
			if st.ConfirmationStatus == "confirmed" || st.ConfirmationStatus == "finalized" {
				return true, nil
			}
		}
		if time.Now().After(deadline) {
			valid, verr := l.RPC.IsBlockhashValid(ctx, blockhash)
			if verr == nil && !valid {
				// One last status check: it may have landed right at expiry.
				if st, err := l.RPC.SignatureStatus(ctx, sig); err == nil && st.Found && st.Err == "" {
					return true, nil
				}
				return false, nil
			}
			if time.Now().After(deadline.Add(2 * time.Minute)) {
				return false, &fatalErr{errors.New("confirmation timed out; not retrying to avoid a double fill")}
			}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func round(f float64, d int) float64 {
	v, _ := strconv.ParseFloat(strconv.FormatFloat(f, 'f', d, 64), 64)
	return v
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
