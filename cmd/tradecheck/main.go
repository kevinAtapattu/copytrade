// Command tradecheck verifies the live trading path without spending anything.
// It asks PumpPortal for the exact buy and sell transactions the executor would
// send, signs them like the executor does, and has the RPC node simulate them
// against current chain state. Nothing is ever sent.
//
// It runs each check on a token still on the bonding curve and on one that
// graduated to PumpSwap, picked from tokens a tracked wallet holds right now:
//   - our buy, signed by our key (with no TRADER_PRIVATE_KEY, a throwaway key that
//     is never saved or funded, so that check stops at "no SOL")
//   - the same buy built for the tracked wallet, which has SOL, so it must succeed
//   - a 100% sell built for that wallet, which holds the token, so it must succeed
//   - reading a real fill: the wallet's own buy tx, read the way the executor reads ours
package main

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"copytrade/internal/config"
	"copytrade/internal/curve"
	"copytrade/internal/exec"
	"copytrade/internal/model"
	"copytrade/internal/solana"
)

var failed bool

func check(name string, ok bool, detail string) {
	mark := "PASS"
	if !ok {
		mark, failed = "FAIL", true
	}
	fmt.Printf("[%s] %s: %s\n", mark, name, detail)
}

type pick struct {
	wallet, mint, pool, buySig string
	buySol, solUSD             float64
}

func main() {
	c := config.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	key, funded := c.TraderPrivateKey != "", true
	var kp solana.Keypair
	if key {
		var err error
		if kp, err = solana.ParseKeypair(c.TraderPrivateKey); err != nil {
			check("trading key", false, err.Error())
			os.Exit(1)
		}
	} else {
		pub, priv, _ := ed25519.GenerateKey(nil)
		kp, funded = solana.Keypair{Private: priv, Public: pub}, false
	}
	fmt.Printf("LIVE_TRADING=%v, size $%.0f, slippage %.0f%%, key: %s\n\n", c.LiveTrading, c.ExecSizeUSD, c.ExecSlippagePct,
		map[bool]string{true: "TRADER_PRIVATE_KEY " + kp.PublicBase58(), false: "none set, using a throwaway key (never saved or funded)"}[key])

	rpc := solana.NewRPC(c.SolanaRPCURL)
	rpc.RPS = 2
	l := &exec.Live{Key: kp, RPC: rpc, TradeURL: c.PumpPortalTradeURL, SlippagePct: c.ExecSlippagePct,
		PriorityFee: c.ExecPriorityFee, JitoTipSol: c.JitoTipSol, JitoURL: c.JitoURL, JupiterURL: c.JupiterURL, HTTP: solana.NewRPC("").Client}

	db, err := pgx.Connect(ctx, c.DatabaseURL)
	if err != nil {
		check("database", false, err.Error())
		os.Exit(1)
	}
	defer db.Close(ctx)
	for _, pool := range []string{"pump", model.PoolPumpSwap} {
		p, err := find(ctx, db, pool)
		if err != nil {
			check(pool+": pick a held token", false, err.Error())
			continue
		}
		fmt.Printf("%s: %s, held by %s\n", pool, p.mint, p.wallet)
		// AMM is set the way the executor sets it from the trade stream; it picks the builder.
		buy := exec.Order{Mint: p.mint, Side: model.SideBuy, SolIn: c.ExecSizeUSD / p.solUSD, State: curve.State{AMM: pool == model.PoolPumpSwap}}
		sell := exec.Order{Mint: p.mint, Side: model.SideSell, SellPct: 100}

		r, err := l.DryRun(ctx, buy, "")
		switch {
		case err != nil:
			check(pool+": our buy, built and signed", false, err.Error())
		case !funded:
			// The signature is checked before the fee payer, so a no-SOL error means it passed.
			check(pool+": our buy, built and signed", noFunds(r.Err), "signature valid; stops at no SOL, as expected for an unfunded key ("+r.Err+")")
		default:
			check(pool+": our buy, built and signed", r.Err == "", result(r))
		}
		r, err = l.DryRun(ctx, buy, p.wallet)
		check(pool+fmt.Sprintf(": $%.0f buy as a funded wallet", c.ExecSizeUSD), err == nil && r.Err == "", result2(r, err))
		r, err = l.DryRun(ctx, sell, p.wallet)
		check(pool+": sell 100% as a holder", err == nil && r.Err == "", result2(r, err))

		bc, err := rpc.GetBalanceChange(ctx, p.buySig, p.wallet, p.mint)
		ok := err == nil && math.Abs(-bc.SolDelta-p.buySol) <= p.buySol*0.05 && bc.TokenDelta > 0
		check(pool+": read a real fill", ok, fmt.Sprintf("their buy %s: %.4f SOL out, %.0f tokens in (recorded %.4f SOL) %v",
			p.buySig[:8], -bc.SolDelta, bc.TokenDelta, p.buySol, errStr(err)))
		fmt.Println()
	}
	calls, _ := rpc.Calls()
	fmt.Printf("RPC calls used: %d. Nothing was sent.\n", calls)
	if failed {
		os.Exit(1)
	}
}

// find picks the newest token on pool that a tracked wallet bought and still holds.
func find(ctx context.Context, db *pgx.Conn, pool string) (pick, error) {
	var p pick
	err := db.QueryRow(ctx, `select p.wallet, p.mint, t.pool, b.signature, b.sol_amount, p.sol_usd
		from sim_positions p join tokens t on t.mint = p.mint
		join lateral (select signature, sol_amount from target_trades where wallet = p.wallet and mint = p.mint and side = 'buy'
			and signature not like '%:%' order by received_at desc limit 1) b on true
		where p.status = 'open' and p.delay_s = 2 and t.pool = $1
		order by p.opened_at desc limit 1`, pool).Scan(&p.wallet, &p.mint, &p.pool, &p.buySig, &p.buySol, &p.solUSD)
	return p, err
}

func noFunds(e string) bool {
	return strings.Contains(e, "AccountNotFound") || strings.Contains(e, "InsufficientFunds")
}

func result(r solana.SimResult) string {
	if r.Err == "" {
		return fmt.Sprintf("would succeed (%d compute units)", r.Units)
	}
	return "would fail: " + r.Err + lastLogs(r.Logs)
}

func result2(r solana.SimResult, err error) string {
	if err != nil {
		return err.Error()
	}
	return result(r)
}

func lastLogs(logs []string) string {
	if len(logs) > 4 {
		logs = logs[len(logs)-4:]
	}
	if len(logs) == 0 {
		return ""
	}
	return "\n    " + strings.Join(logs, "\n    ")
}

func errStr(err error) string {
	if err != nil {
		return err.Error()
	}
	return ""
}
