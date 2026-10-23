// Command genfixtures writes the deterministic replay fixture.
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"copytrade/internal/fixture"
)

func main() {
	out := flag.String("out", "fixtures/replay.jsonl", "output JSONL path (meta goes to .meta.json next to it)")
	days := flag.Int("days", 21, "days of data")
	seed := flag.Uint64("seed", 42, "random seed")
	flag.Parse()
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	d := fixture.Generate(*seed, start, *days)
	lf, err := os.Create(*out)
	check(err)
	defer lf.Close()
	mf, err := os.Create(MetaPath(*out))
	check(err)
	defer mf.Close()
	check(d.Write(lf, mf))
	fmt.Printf("wrote %d trades to %s\n", len(d.Lines), *out)
}

func MetaPath(p string) string { return strings.TrimSuffix(p, ".jsonl") + ".meta.json" }

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
