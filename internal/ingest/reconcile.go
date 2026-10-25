package ingest

import (
	"context"
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/mr-tron/base58"

	"copytrade/internal/curve"
	"copytrade/internal/model"
	"copytrade/internal/store"
)

// Reconciler is the safety net for exits we can't see as trades: a wallet that
// moves its tokens to another account before selling (some terminals do), or sells
// on an exchange we don't decode. Every interval it checks whether each wallet
// with an open copy still holds the token. If not, it feeds a full-exit sell into
// the normal trade path, so paper copies close at the price 2/10/60s later and a
// live copy sells too.
type Reconciler struct {
	In       *Ingestor
	Store    store.Store
	Balances BalanceReader
	// MinAge skips copies opened very recently (the RPC may not show the buy yet).
	MinAge time.Duration
	Now    func() time.Time

	sent map[string]time.Time // wallet|mint -> when we last reported its exit
	// Checks counts RPC calls made; Exits counts exits reported (read by the heartbeat).
	Checks, Exits atomic.Int64
}

// BalanceReader is the slice of the RPC client the check needs.
type BalanceReader interface {
	GetMultipleAccounts(ctx context.Context, addrs []string) ([][]byte, error)
	OwnerTokenBalance(ctx context.Context, owner, mint string) (float64, bool, error)
}

// SignaturePrefix marks the synthetic sells in the trade log.
const SignaturePrefix = "exit-check:"

var tokenPrograms = []string{"TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA", "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"}

const ataProgram = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"

func ata(owner, mint, program string) (string, error) {
	o, err1 := base58.Decode(owner)
	m, err2 := base58.Decode(mint)
	p, err3 := base58.Decode(program)
	if err1 != nil || err2 != nil || err3 != nil || len(o) != 32 || len(m) != 32 {
		return "", fmt.Errorf("bad address")
	}
	a, err := curve.FindPDA([][]byte{o, p, m}, ataProgram)
	if err != nil {
		return "", err
	}
	return base58.Encode(a), nil
}

type pair struct{ wallet, mint string }

// Check runs one pass. Cost: one getMultipleAccounts per 50 open wallet+token
// pairs, plus one getTokenAccountsByOwner per pair that looks empty.
func (r *Reconciler) Check(ctx context.Context) error {
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	if r.sent == nil {
		r.sent = map[string]time.Time{}
	}
	sims, err := r.Store.ListSimPositions(ctx, store.SimFilter{OpenOnly: true})
	if err != nil {
		return err
	}
	lives, err := r.Store.ListLivePositions(ctx, store.LiveFilter{OpenOnly: true})
	if err != nil {
		return err
	}
	seen := map[pair]bool{}
	var pairs []pair
	add := func(w, m string, opened time.Time) {
		p := pair{w, m}
		if seen[p] || now.Sub(opened) < r.MinAge {
			return
		}
		if t, ok := r.sent[w+"|"+m]; ok && now.Sub(t) < 30*time.Minute {
			return // already reported; the copies are closing
		}
		seen[p] = true
		pairs = append(pairs, p)
	}
	for _, p := range sims {
		add(p.Wallet, p.Mint, p.OpenedAt)
	}
	for _, p := range lives {
		add(p.Wallet, p.Mint, p.OpenedAt)
	}
	for i := 0; i < len(pairs); i += 50 { // 2 accounts each, 100 per request
		batch := pairs[i:min(i+50, len(pairs))]
		var addrs []string
		for _, p := range batch {
			for _, prog := range tokenPrograms {
				a, _ := ata(p.wallet, p.mint, prog)
				addrs = append(addrs, a)
			}
		}
		datas, err := r.Balances.GetMultipleAccounts(ctx, addrs)
		r.Checks.Add(1)
		if err != nil {
			return err
		}
		for k, p := range batch {
			if amount(datas[2*k]) > 0 || amount(datas[2*k+1]) > 0 {
				continue // still holding
			}
			// Empty or closed at the usual address: confirm across all its token accounts.
			bal, _, err := r.Balances.OwnerTokenBalance(ctx, p.wallet, p.mint)
			r.Checks.Add(1)
			if err != nil || bal > Dust {
				continue
			}
			r.exit(ctx, p, now)
		}
	}
	return nil
}

func (r *Reconciler) exit(ctx context.Context, p pair, now time.Time) {
	tok, _, _ := r.Store.GetToken(ctx, p.mint)
	t := model.Trade{
		Signature: fmt.Sprintf("%s%s:%s:%d", SignaturePrefix, p.wallet, p.mint, now.Unix()),
		Mint:      p.mint, Trader: p.wallet, Side: model.SideSell, NewTokenBalance: 0,
		VSol: tok.VSol, VTokens: tok.VTokens, Pool: tok.Pool, BondingCurveKey: tok.CurveKey, ReceivedAt: now.UTC(),
	}
	if t.Pool == "" {
		t.Pool = "pump"
	}
	r.sent[p.wallet+"|"+p.mint] = now
	r.Exits.Add(1)
	slog.Info("exit check: wallet no longer holds the token, closing copies", "wallet", p.wallet, "mint", p.mint)
	r.In.HandleTrade(ctx, t)
}

// amount reads an SPL token account's amount (u64 at offset 64); 0 if missing.
func amount(d []byte) uint64 {
	if len(d) < 72 {
		return 0
	}
	return binary.LittleEndian.Uint64(d[64:])
}
