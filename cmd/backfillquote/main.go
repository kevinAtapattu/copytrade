// Command backfillquote fills in sol_amount on a wallet's stored trades on
// curves priced in another token (pool "nonsol"). Those were recorded with 0
// before the TradeEvent's quote fields were decoded. It re-reads each tx and
// writes the quote amount the stream now records (raw quote units / 1e9, see
// history.Event.QuoteAmount). Only rows still at 0 are touched, so it can be
// re-run safely. Pool, reserves and everything else stay as they are.
//
//	go run ./cmd/backfillquote [-wallet ADDR] [-dry]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"copytrade/internal/config"
	"copytrade/internal/solana"
	"copytrade/internal/source"
)

func main() {
	wallet := flag.String("wallet", "363sqMFaxZgvCoGzxKjXe1BqMGYkSVoCwmghZUndXuaT", "wallet whose trades to fill in")
	dry := flag.Bool("dry", false, "parse and report, write nothing")
	rps := flag.Float64("rps", 2, "RPC calls per second")
	flag.Parse()
	c := config.Load()
	ctx := context.Background()

	db, err := pgx.Connect(ctx, c.DatabaseURL)
	if err != nil {
		log.Fatal("database: ", err)
	}
	defer db.Close(ctx)
	rpc := solana.NewRPC(c.SolanaRPCURL)
	rpc.RPS = *rps

	rows, err := db.Query(ctx, `SELECT signature, token_amount, received_at FROM target_trades
		WHERE wallet=$1 AND pool='nonsol' AND v_sol=0 AND sol_amount=0 ORDER BY received_at`, *wallet)
	if err != nil {
		log.Fatal(err)
	}
	type row struct {
		sig    string
		tokens float64
		at     time.Time
	}
	bySig := map[string][]row{} // base tx signature -> stored rows
	var order []string
	n := 0
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.sig, &r.tokens, &r.at); err != nil {
			log.Fatal(err)
		}
		base, _, _ := strings.Cut(r.sig, "#")
		if bySig[base] == nil {
			order = append(order, base)
		}
		bySig[base] = append(bySig[base], r)
		n++
	}
	if err := rows.Err(); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%d rows at 0 in %d txs\n", n, len(order))

	var fetchErr, noQuote, unmatched, target, token int
	for i, base := range order {
		tx, err := rpc.GetTxEvents(ctx, base)
		if err != nil || tx.Failed {
			fetchErr++
			continue
		}
		parsed := map[string]float64{} // signature -> quote amount
		tok := map[string]float64{}
		for _, t := range source.CurveTradesFromTx(base, *wallet, tx, tx.BlockTime) {
			if t.Pool == "nonsol" && t.VSol == 0 && t.SolAmount > 0 {
				parsed[t.Signature], tok[t.Signature] = t.SolAmount, t.TokenAmount
			}
		}
		for _, r := range bySig[base] {
			q, ok := parsed[r.sig]
			if !ok {
				noQuote++
				continue
			}
			if math.Abs(tok[r.sig]-r.tokens) > 1e-6*math.Max(r.tokens, 1) {
				unmatched++
				continue
			}
			if *dry {
				target++
				continue
			}
			const guard = ` AND pool='nonsol' AND v_sol=0 AND sol_amount=0`
			tag, err := db.Exec(ctx, `UPDATE target_trades SET sol_amount=$1 WHERE signature=$2 AND wallet=$3`+guard, q, r.sig, *wallet)
			if err != nil {
				log.Fatal(err)
			}
			target += int(tag.RowsAffected())
			tag, err = db.Exec(ctx, `UPDATE token_trades SET sol_amount=$1 WHERE signature=$2 AND trader=$3`+guard, q, r.sig, *wallet)
			if err != nil {
				log.Fatal(err)
			}
			token += int(tag.RowsAffected())
		}
		if (i+1)%25 == 0 {
			fmt.Printf("  %d/%d txs\n", i+1, len(order))
		}
	}
	verb := "updated"
	if *dry {
		verb = "would update"
	}
	fmt.Printf("%s %d target_trades rows, %d token_trades rows; no quote in tx %d, token amount mismatch %d, tx fetch errors %d\n",
		verb, target, token, noQuote, unmatched, fetchErr)
}
