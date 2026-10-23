package store

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"copytrade/internal/model"
)

// Memory is an in-process Store used by tests and replay.
type Memory struct {
	mu          sync.RWMutex
	wallets     map[string]model.Wallet
	rotations   []model.Rotation
	tokenTrades []model.Trade
	tokenSeen   map[string]bool
	targets     []model.Trade
	targetSeen  map[string]bool
	tokens      map[string]model.Token
	simTrades   []model.SimTrade
	simSeen     map[string]bool
	positions   map[string]model.SimPosition
	scores      []model.Score
	hits        []model.DiscoveryHit
	hitSeen     map[string]bool
	liveFills   []model.LiveFill
	livePos     map[string]model.LivePosition
	settings    map[string]string
	health      map[string]Health
}

func NewMemory() *Memory {
	return &Memory{
		wallets:    map[string]model.Wallet{},
		tokenSeen:  map[string]bool{},
		targetSeen: map[string]bool{},
		tokens:     map[string]model.Token{},
		simSeen:    map[string]bool{},
		positions:  map[string]model.SimPosition{},
		hitSeen:    map[string]bool{},
		livePos:    map[string]model.LivePosition{},
		settings:   map[string]string{},
		health:     map[string]Health{},
	}
}

func (m *Memory) Close() {}

func (m *Memory) AddWallet(_ context.Context, w model.Wallet) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.wallets[w.Address]; ok {
		return false, nil
	}
	if w.AddedAt.IsZero() {
		w.AddedAt = time.Now()
	}
	if w.StatusAt.IsZero() {
		w.StatusAt = w.AddedAt
	}
	m.wallets[w.Address] = w
	return true, nil
}

func (m *Memory) ListWallets(_ context.Context) ([]model.Wallet, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]model.Wallet, 0, len(m.wallets))
	for _, w := range m.wallets {
		out = append(out, w)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out, nil
}

func (m *Memory) GetWallet(_ context.Context, addr string) (model.Wallet, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	w, ok := m.wallets[addr]
	return w, ok, nil
}

func (m *Memory) SetWalletStatus(_ context.Context, addr, status, reason string, at time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, ok := m.wallets[addr]
	if !ok || w.Status == status {
		return false, nil
	}
	m.rotations = append(m.rotations, model.Rotation{
		ID: int64(len(m.rotations) + 1), Wallet: addr, FromStatus: w.Status, ToStatus: status, Reason: reason, At: at,
	})
	w.Status, w.Reason, w.StatusAt = status, reason, at
	m.wallets[addr] = w
	return true, nil
}

func (m *Memory) ListRotations(_ context.Context, limit int) ([]model.Rotation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]model.Rotation, 0, len(m.rotations))
	for i := len(m.rotations) - 1; i >= 0; i-- {
		out = append(out, m.rotations[i])
	}
	return capN(out, limit), nil
}

func (m *Memory) InsertTokenTrade(_ context.Context, t model.Trade) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := t.Signature + "|" + t.Trader
	if m.tokenSeen[k] {
		return false, nil
	}
	m.tokenSeen[k] = true
	m.tokenTrades = append(m.tokenTrades, t)
	return true, nil
}

func (m *Memory) InsertTargetTrade(_ context.Context, t model.Trade) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := t.Signature + "|" + t.Trader
	if m.targetSeen[k] {
		return false, nil
	}
	m.targetSeen[k] = true
	m.targets = append(m.targets, t)
	if w, ok := m.wallets[t.Trader]; ok {
		at := t.ReceivedAt
		if w.LastTradeAt == nil || at.After(*w.LastTradeAt) {
			w.LastTradeAt = &at
			m.wallets[t.Trader] = w
		}
	}
	return true, nil
}

func filterTrades(src []model.Trade, f TradeFilter) []model.Trade {
	var out []model.Trade
	for i := len(src) - 1; i >= 0; i-- { // newest first
		t := src[i]
		if f.Wallet != "" && t.Trader != f.Wallet {
			continue
		}
		if f.Mint != "" && t.Mint != f.Mint {
			continue
		}
		if !f.Since.IsZero() && t.ReceivedAt.Before(f.Since) {
			continue
		}
		out = append(out, t)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ReceivedAt.After(out[j].ReceivedAt) })
	return capN(out, f.Limit)
}

func (m *Memory) ListTargetTrades(_ context.Context, f TradeFilter) ([]model.Trade, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return filterTrades(m.targets, f), nil
}

func (m *Memory) ListTokenTrades(_ context.Context, f TradeFilter) ([]model.Trade, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return filterTrades(m.tokenTrades, f), nil
}

func (m *Memory) CountTokenTradesSince(_ context.Context, since time.Time) (int64, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var n int64
	for _, t := range m.tokenTrades {
		if !t.ReceivedAt.Before(since) {
			n++
		}
	}
	return n, nil
}

// ApplyTradeToToken folds a trade into a token's running stats. Shared with Postgres
// so both stores agree on the rules.
func ApplyTradeToToken(tok model.Token, exists bool, t model.Trade) model.Token {
	p := t.Price()
	if !exists {
		tok = model.Token{Mint: t.Mint, FirstSeen: t.ReceivedAt, FirstPrice: p, MaxPrice: p}
	}
	if t.Pool != "" && t.Pool != "pump" {
		tok.Migrated = true
		tok.Pool = t.Pool
		tok.LastTradeAt = t.ReceivedAt
		return tok
	}
	if p > 0 {
		if tok.FirstPrice <= 0 {
			tok.FirstPrice = p
		}
		if p > tok.MaxPrice {
			tok.MaxPrice = p
		}
		tok.LastPrice = p
		tok.VSol, tok.VTokens = t.VSol, t.VTokens
	}
	tok.Pool = "pump"
	if t.ReceivedAt.After(tok.LastTradeAt) {
		tok.LastTradeAt = t.ReceivedAt
	}
	return tok
}

func (m *Memory) UpsertTokenFromTrade(_ context.Context, t model.Trade) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	tok, ok := m.tokens[t.Mint]
	m.tokens[t.Mint] = ApplyTradeToToken(tok, ok, t)
	return nil
}

func (m *Memory) GetToken(_ context.Context, mint string) (model.Token, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	t, ok := m.tokens[mint]
	return t, ok, nil
}

func (m *Memory) ListTokens(_ context.Context, minMult float64, undiscovered bool) ([]model.Token, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []model.Token
	for _, t := range m.tokens {
		if undiscovered && t.Discovered {
			continue
		}
		if minMult > 0 && (t.FirstPrice <= 0 || t.MaxPrice/t.FirstPrice < minMult) {
			continue
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeen.Before(out[j].FirstSeen) })
	return out, nil
}

func (m *Memory) MarkTokenDiscovered(_ context.Context, mint string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.tokens[mint]; ok {
		t.Discovered = true
		m.tokens[mint] = t
	}
	return nil
}

func (m *Memory) InsertSimTrade(_ context.Context, s model.SimTrade) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := s.TargetSig + "|" + s.Wallet + "|" + itoa(s.DelayS)
	if m.simSeen[k] {
		return nil
	}
	m.simSeen[k] = true
	m.simTrades = append(m.simTrades, s)
	return nil
}

func (m *Memory) UpsertSimPosition(_ context.Context, p model.SimPosition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.positions[p.ID] = p
	return nil
}

func (m *Memory) ListSimPositions(_ context.Context, f SimFilter) ([]model.SimPosition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []model.SimPosition
	for _, p := range m.positions {
		if f.Wallet != "" && p.Wallet != f.Wallet {
			continue
		}
		if f.Mint != "" && p.Mint != f.Mint {
			continue
		}
		if f.DelayS != 0 && p.DelayS != f.DelayS {
			continue
		}
		if !f.Since.IsZero() && p.OpenedAt.Before(f.Since) {
			continue
		}
		if f.OpenOnly && p.Status != model.PosOpen {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].OpenedAt.Equal(out[j].OpenedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].OpenedAt.Before(out[j].OpenedAt)
	})
	return capN(out, f.Limit), nil
}

func (m *Memory) ListSimTrades(_ context.Context, f SimFilter) ([]model.SimTrade, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []model.SimTrade
	for i := len(m.simTrades) - 1; i >= 0; i-- {
		s := m.simTrades[i]
		if f.Wallet != "" && s.Wallet != f.Wallet {
			continue
		}
		if f.Mint != "" && s.Mint != f.Mint {
			continue
		}
		if f.DelayS != 0 && s.DelayS != f.DelayS {
			continue
		}
		if !f.Since.IsZero() && s.FillAt.Before(f.Since) {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].FillAt.After(out[j].FillAt) })
	return capN(out, f.Limit), nil
}

func (m *Memory) SimTradesForTarget(_ context.Context, sig string) ([]model.SimTrade, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []model.SimTrade
	for _, s := range m.simTrades {
		if s.TargetSig == sig {
			out = append(out, s)
		}
	}
	return out, nil
}

func (m *Memory) InsertScores(_ context.Context, s []model.Score) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.scores = append(m.scores, s...)
	return nil
}

func (m *Memory) LatestScores(_ context.Context) ([]model.Score, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	type key struct {
		w    string
		d, n int
	}
	latest := map[key]model.Score{}
	for _, s := range m.scores {
		k := key{s.Wallet, s.DelayS, s.WindowDays}
		if cur, ok := latest[k]; !ok || s.ComputedAt.After(cur.ComputedAt) {
			latest[k] = s
		}
	}
	out := make([]model.Score, 0, len(latest))
	for _, s := range latest {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Wallet != out[j].Wallet {
			return out[i].Wallet < out[j].Wallet
		}
		if out[i].DelayS != out[j].DelayS {
			return out[i].DelayS < out[j].DelayS
		}
		return out[i].WindowDays < out[j].WindowDays
	})
	return out, nil
}

func (m *Memory) ScoreHistory(_ context.Context, wallet string, delayS, window int, since time.Time) ([]model.Score, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []model.Score
	for _, s := range m.scores {
		if s.Wallet == wallet && s.DelayS == delayS && s.WindowDays == window && !s.ComputedAt.Before(since) {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ComputedAt.Before(out[j].ComputedAt) })
	return out, nil
}

func (m *Memory) InsertDiscoveryHit(_ context.Context, h model.DiscoveryHit) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := h.Wallet + "|" + h.Mint
	if m.hitSeen[k] {
		return false, nil
	}
	m.hitSeen[k] = true
	h.ID = int64(len(m.hits) + 1)
	m.hits = append(m.hits, h)
	return true, nil
}

func (m *Memory) ListDiscoveryHits(_ context.Context, limit int) ([]model.DiscoveryHit, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]model.DiscoveryHit, 0, len(m.hits))
	for i := len(m.hits) - 1; i >= 0; i-- {
		out = append(out, m.hits[i])
	}
	return capN(out, limit), nil
}

func (m *Memory) InsertLiveFill(_ context.Context, f model.LiveFill) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.liveFills = append(m.liveFills, f)
	return nil
}

func (m *Memory) ListLiveFills(_ context.Context, f LiveFilter) ([]model.LiveFill, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []model.LiveFill
	for i := len(m.liveFills) - 1; i >= 0; i-- {
		x := m.liveFills[i]
		if f.Mode != "" && x.Mode != f.Mode {
			continue
		}
		if f.Wallet != "" && x.Wallet != f.Wallet {
			continue
		}
		if !f.Since.IsZero() && x.SentAt.Before(f.Since) {
			continue
		}
		out = append(out, x)
	}
	return capN(out, f.Limit), nil
}

func (m *Memory) UpsertLivePosition(_ context.Context, p model.LivePosition) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.livePos[p.ID] = p
	return nil
}

func (m *Memory) ListLivePositions(_ context.Context, f LiveFilter) ([]model.LivePosition, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []model.LivePosition
	for _, p := range m.livePos {
		if f.Mode != "" && p.Mode != f.Mode {
			continue
		}
		if f.Wallet != "" && p.Wallet != f.Wallet {
			continue
		}
		if f.OpenOnly && p.Status != model.PosOpen {
			continue
		}
		if !f.Since.IsZero() && p.OpenedAt.Before(f.Since) {
			continue
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].OpenedAt.Before(out[j].OpenedAt) })
	return capN(out, f.Limit), nil
}

func (m *Memory) GetSetting(_ context.Context, key string) (string, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.settings[key]
	return v, ok, nil
}

func (m *Memory) SetSetting(_ context.Context, key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.settings[key] = value
	return nil
}

func (m *Memory) PutHealth(_ context.Context, service string, data json.RawMessage, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.health[service] = Health{Data: data, UpdatedAt: at}
	return nil
}

func (m *Memory) ListHealth(_ context.Context) (map[string]Health, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make(map[string]Health, len(m.health))
	for k, v := range m.health {
		out[k] = v
	}
	return out, nil
}

func capN[T any](s []T, n int) []T {
	if n > 0 && len(s) > n {
		return s[:n]
	}
	return s
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
