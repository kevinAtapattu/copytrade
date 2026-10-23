// Package seed loads wallets.txt into the wallets table.
package seed

import (
	"bufio"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"copytrade/internal/model"
	"copytrade/internal/store"
)

// Parse reads lines of "ADDRESS [label...]". Blank lines and # comments are skipped.
func Parse(r io.Reader) []model.Wallet {
	var out []model.Wallet
	seen := map[string]bool{}
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(strings.ReplaceAll(line, ",", " "))
		if len(fields) == 0 || !ValidAddress(fields[0]) || seen[fields[0]] {
			continue
		}
		seen[fields[0]] = true
		out = append(out, model.Wallet{Address: fields[0], Label: strings.Join(fields[1:], " ")})
	}
	return out
}

// ValidAddress is a cheap base58 + length check for Solana pubkeys.
func ValidAddress(s string) bool {
	if len(s) < 32 || len(s) > 44 {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz", c) {
			return false
		}
	}
	return true
}

// Wallets adds wallets from path to the bench. A missing file is not an error.
// Existing wallets keep their status.
func Wallets(ctx context.Context, s store.Store, path string) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		slog.Warn("wallets file not found, skipping seed", "path", path)
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()
	n := 0
	now := time.Now()
	for _, w := range Parse(f) {
		w.Status, w.Source, w.Reason, w.AddedAt = model.StatusBench, model.SourceSeed, "seeded from wallets file", now
		added, err := s.AddWallet(ctx, w)
		if err != nil {
			return n, err
		}
		if added {
			n++
		}
	}
	return n, nil
}
