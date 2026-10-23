// Command replay feeds recorded or fixture trades through the full pipeline
// in-process (ingest, simulator, scorer, discovery, executor in paper mode).
// No API key and no Docker needed. With -db it writes to Postgres so the
// dashboard can show the results.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"copytrade/internal/config"
	"copytrade/internal/fixture"
	"copytrade/internal/model"
	"copytrade/internal/pipeline"
	"copytrade/internal/seed"
	"copytrade/internal/source"
	"copytrade/internal/store"
)

func main() {
	path := flag.String("fixture", "fixtures/replay.jsonl", "JSONL of {at, msg} lines (fixture or ingest RECORD_FILE)")
	useDB := flag.Bool("db", false, "write to Postgres at DATABASE_URL instead of memory")
	reset := flag.Bool("reset", false, "with -db: truncate all tables first")
	walletsFile := flag.String("wallets", "", "wallets file to seed when there is no .meta.json (default WALLETS_FILE)")
	flag.Parse()
	ctx := context.Background()
	c := config.Load()

	f, err := os.Open(*path)
	check(err)
	src, err := source.NewReplay(f)
	f.Close()
	check(err)

	var st store.Store = store.NewMemory()
	if *useDB {
		pg, err := store.OpenPostgres(ctx, c.DatabaseURL)
		check(err)
		_, err = pg.Migrate(ctx)
		check(err)
		if *reset {
			check(pg.Reset(ctx))
		}
		st = pg
	}
	defer st.Close()

	var meta *fixture.Meta
	if b, err := os.ReadFile(strings.TrimSuffix(*path, ".jsonl") + ".meta.json"); err == nil {
		meta = &fixture.Meta{}
		check(json.Unmarshal(b, meta))
		for _, w := range meta.Wallets {
			_, err := st.AddWallet(ctx, w)
			check(err)
		}
	} else {
		wf := *walletsFile
		if wf == "" {
			wf = c.WalletsFile
		}
		n, err := seed.Wallets(ctx, st, wf)
		check(err)
		fmt.Printf("seeded %d wallets from %s\n", n, wf)
	}

	t0 := time.Now()
	rep, err := pipeline.Run(ctx, c, st, src, pipeline.Options{SolUSD: 150})
	check(err)
	fmt.Printf("replayed %d messages (%d delivered after subscription filter) from %s to %s in %s\n",
		rep.Messages, rep.Delivered, rep.Start.Format(time.DateTime), rep.End.Format(time.DateTime), time.Since(t0).Round(time.Millisecond))
	fmt.Printf("target trades %d, sim fills %d (expect %d), job runs %d, events %v\n\n",
		rep.TargetTrades, rep.SimTrades, rep.TargetTrades*len(model.Delays), rep.JobRuns, rep.Events)
	printSummary(ctx, st, meta)
}

func printSummary(ctx context.Context, st store.Store, meta *fixture.Meta) {
	ws, _ := st.ListWallets(ctx)
	scores, _ := st.LatestScores(ctx)
	label := map[string]string{}
	for _, w := range ws {
		label[w.Address] = w.Label
	}
	if meta != nil {
		r := meta.Roles
		for a, l := range map[string]string{r.Early1: "early1", r.Early2: "early2"} {
			label[a] = l
		}
	}
	sort.Slice(ws, func(i, j int) bool { return ws[i].Status < ws[j].Status })
	fmt.Printf("%-8s %-8s %-10s %10s %10s %10s %10s %6s  %s\n", "wallet", "label", "status", "pnl+2s", "exbest+2s", "pnl+10s", "pnl+60s", "dd", "reason")
	get := func(w string, d int) model.Score {
		for _, s := range scores {
			if s.Wallet == w && s.DelayS == d && s.WindowDays == 14 {
				return s
			}
		}
		return model.Score{}
	}
	for _, w := range ws {
		s2, s10, s60 := get(w.Address, 2), get(w.Address, 10), get(w.Address, 60)
		fmt.Printf("%-8s %-8s %-10s %10.2f %10.2f %10.2f %10.2f %5.0f%%  %s\n", w.Address[:6], label[w.Address], w.Status,
			s2.PnlUSD, s2.PnlExBestUSD, s10.PnlUSD, s60.PnlUSD, s2.MaxDrawdown*100, w.Reason)
	}
	rots, _ := st.ListRotations(ctx, 50)
	fmt.Printf("\nrotations (%d):\n", len(rots))
	for i := len(rots) - 1; i >= 0; i-- {
		r := rots[i]
		fmt.Printf("  %s %-6s %s -> %s: %s\n", r.At.Format(time.DateTime), label[r.Wallet], r.FromStatus, r.ToStatus, r.Reason)
	}
	fills, _ := st.ListLiveFills(ctx, store.LiveFilter{})
	by := map[string]int{}
	for _, f := range fills {
		by[f.Status]++
	}
	pos, _ := st.ListLivePositions(ctx, store.LiveFilter{})
	pnl := 0.0
	for _, p := range pos {
		if p.Status != model.PosOpen {
			pnl += (p.ProceedsSol - p.CostSol) * p.SolUSD
		}
	}
	fmt.Printf("\nexecutor (paper): fills %v, positions %d, realized %+.2f USD\n", by, len(pos), pnl)
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
