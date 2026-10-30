// Package solana has the minimum needed to sign PumpPortal-built transactions and
// talk to an RPC node: keypair parsing, versioned-tx signing, and a few JSON-RPC calls.
package solana

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mr-tron/base58"
)

const LamportsPerSol = 1e9

type Keypair struct {
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
}

func (k Keypair) PublicBase58() string { return base58.Encode(k.Public) }

// ParseKeypair accepts a base58 64-byte secret key (Phantom/Solflare export) or a
// JSON byte array (solana-keygen file contents).
func ParseKeypair(s string) (Keypair, error) {
	s = strings.TrimSpace(s)
	var raw []byte
	if strings.HasPrefix(s, "[") {
		var arr []int
		if err := json.Unmarshal([]byte(s), &arr); err != nil {
			return Keypair{}, fmt.Errorf("keypair json: %w", err)
		}
		raw = make([]byte, len(arr))
		for i, v := range arr {
			raw[i] = byte(v)
		}
	} else {
		var err error
		if raw, err = base58.Decode(s); err != nil {
			return Keypair{}, fmt.Errorf("keypair base58: %w", err)
		}
	}
	switch len(raw) {
	case 64:
		pk := ed25519.NewKeyFromSeed(raw[:32])
		pub := pk.Public().(ed25519.PublicKey)
		if !bytes.Equal(pub, raw[32:]) {
			return Keypair{}, errors.New("keypair: public half does not match secret")
		}
		return Keypair{Private: pk, Public: pub}, nil
	case 32:
		pk := ed25519.NewKeyFromSeed(raw)
		return Keypair{Private: pk, Public: pk.Public().(ed25519.PublicKey)}, nil
	}
	return Keypair{}, fmt.Errorf("keypair: expected 64 bytes, got %d", len(raw))
}

// Tx is a parsed wire transaction (legacy or v0).
type Tx struct {
	Raw          []byte
	NumSigs      int
	sigOffset    int // offset of first signature
	msgOffset    int
	RequiredSigs int
	AccountKeys  [][]byte
	Blockhash    []byte
}

func readShortVec(b []byte, off int) (int, int, error) {
	v, shift := 0, 0
	for i := 0; i < 3; i++ {
		if off >= len(b) {
			return 0, 0, errors.New("short_vec: truncated")
		}
		c := b[off]
		off++
		v |= int(c&0x7f) << shift
		if c&0x80 == 0 {
			return v, off, nil
		}
		shift += 7
	}
	return 0, 0, errors.New("short_vec: too long")
}

// ParseTx reads the signature slots, header, account keys and blockhash.
func ParseTx(raw []byte) (*Tx, error) {
	// Transaction v1 (seen on mainnet since 2026) starts with a version byte 0x81 and
	// uses a different layout. We only sign legacy and v0; refuse rather than mis-sign.
	if len(raw) > 0 && raw[0]&0x80 != 0 {
		return nil, fmt.Errorf("unsupported transaction version %d (only legacy and v0 can be signed)", raw[0]&0x7f)
	}
	n, off, err := readShortVec(raw, 0)
	if err != nil {
		return nil, err
	}
	t := &Tx{Raw: append([]byte(nil), raw...), NumSigs: n, sigOffset: off}
	off += 64 * n
	t.msgOffset = off
	if off >= len(raw) {
		return nil, errors.New("tx: truncated before message")
	}
	if raw[off]&0x80 != 0 { // versioned message prefix
		off++
	}
	if off+3 > len(raw) {
		return nil, errors.New("tx: truncated header")
	}
	t.RequiredSigs = int(raw[off])
	off += 3
	nk, off2, err := readShortVec(raw, off)
	if err != nil {
		return nil, err
	}
	off = off2
	if off+32*nk+32 > len(raw) {
		return nil, errors.New("tx: truncated keys")
	}
	for i := 0; i < nk; i++ {
		t.AccountKeys = append(t.AccountKeys, raw[off:off+32])
		off += 32
	}
	t.Blockhash = raw[off : off+32]
	if t.RequiredSigs > t.NumSigs {
		return nil, errors.New("tx: fewer signature slots than required signers")
	}
	return t, nil
}

// Message returns the bytes that get signed.
func (t *Tx) Message() []byte { return t.Raw[t.msgOffset:] }

// Sign places kp's signature in its slot. kp must be one of the required signers.
func (t *Tx) Sign(kp Keypair) error {
	for i := 0; i < t.RequiredSigs && i < len(t.AccountKeys); i++ {
		if bytes.Equal(t.AccountKeys[i], kp.Public) {
			sig := ed25519.Sign(kp.Private, t.Message())
			copy(t.Raw[t.sigOffset+64*i:], sig)
			return nil
		}
	}
	return errors.New("tx: keypair is not a required signer")
}

// Signature is the first signature (the tx id), base58.
func (t *Tx) Signature() string { return base58.Encode(t.Raw[t.sigOffset : t.sigOffset+64]) }

func (t *Tx) BlockhashBase58() string { return base58.Encode(t.Blockhash) }

// ---- RPC

type RPC struct {
	URL    string
	Client *http.Client
	// RPS caps request rate (0 = unlimited). Helius free tier allows 10/s.
	RPS float64

	mu       sync.Mutex
	next     time.Time
	day      time.Time
	calls    int64
	callsAll int64
}

// Calls returns requests made today (UTC) and since start.
func (r *RPC) Calls() (today, total int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.day.Equal(time.Now().UTC().Truncate(24 * time.Hour)) {
		return 0, r.callsAll
	}
	return r.calls, r.callsAll
}

// wait blocks until the rate limit allows another request, and counts it.
func (r *RPC) wait(ctx context.Context) error {
	r.mu.Lock()
	now := time.Now()
	if d := now.UTC().Truncate(24 * time.Hour); !d.Equal(r.day) {
		r.day, r.calls = d, 0
	}
	r.calls++
	r.callsAll++
	var sleep time.Duration
	if r.RPS > 0 {
		gap := time.Duration(float64(time.Second) / r.RPS)
		if r.next.Before(now) {
			r.next = now
		}
		sleep = r.next.Sub(now)
		r.next = r.next.Add(gap)
	}
	r.mu.Unlock()
	if sleep <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(sleep):
		return nil
	}
}

func NewRPC(url string) *RPC { return &RPC{URL: url, Client: &http.Client{Timeout: 20 * time.Second}} }

// RPCError is a definite JSON-RPC error response (as opposed to a transport failure).
type RPCError struct {
	Method  string
	Code    int
	Message string
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("%s: rpc error %d: %s", e.Method, e.Code, e.Message)
}

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ErrRateLimited is an HTTP 429 from the RPC provider.
var ErrRateLimited = errors.New("rpc rate limited (429)")

// call retries reads a few times on 429 with backoff. sendTransaction is never
// retried here: the executor confirms and retries sends itself.
func (r *RPC) call(ctx context.Context, method string, params []any, out any) error {
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		if err := r.wait(ctx); err != nil {
			return err
		}
		err := postJSONRPC(ctx, r.Client, r.URL, method, params, out)
		if !errors.Is(err, ErrRateLimited) || method == "sendTransaction" || attempt == 3 {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
}

// GetMultipleAccounts returns raw account data for up to 100 addresses (nil where
// the account does not exist). Commitment "processed" for the freshest state.
func (r *RPC) GetMultipleAccounts(ctx context.Context, addrs []string) ([][]byte, error) {
	var res struct {
		Value []*struct {
			Data []string `json:"data"` // [base64, "base64"]
		} `json:"value"`
	}
	err := r.call(ctx, "getMultipleAccounts", []any{addrs, map[string]any{"encoding": "base64", "commitment": "processed"}}, &res)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(addrs))
	for i, v := range res.Value {
		if i >= len(out) || v == nil || len(v.Data) == 0 {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(v.Data[0])
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

type SigInfo struct {
	Signature string `json:"signature"`
	Slot      uint64 `json:"slot"`
	BlockTime int64  `json:"blockTime"`
	Err       any    `json:"err"`
}

// GetSignaturesForAddress lists signatures newest first, paging with before.
func (r *RPC) GetSignaturesForAddress(ctx context.Context, addr, before string, limit int) ([]SigInfo, error) {
	opts := map[string]any{"limit": limit, "commitment": "confirmed"}
	if before != "" {
		opts["before"] = before
	}
	var out []SigInfo
	err := r.call(ctx, "getSignaturesForAddress", []any{addr, opts}, &out)
	return out, err
}

// OwnerTokenBalance sums owner's balance of mint across all its token accounts
// (any token program), in UI units. found=false when it has no account at all.
func (r *RPC) OwnerTokenBalance(ctx context.Context, owner, mint string) (balance float64, found bool, err error) {
	accts, err := r.OwnerTokenAccounts(ctx, owner, mint)
	for _, v := range accts {
		balance += v
	}
	return balance, len(accts) > 0, err
}

// OwnerTokenAccounts returns owner's balance of mint in each of its token
// accounts, by account address, in UI units.
func (r *RPC) OwnerTokenAccounts(ctx context.Context, owner, mint string) (map[string]float64, error) {
	var out struct {
		Value []struct {
			Pubkey  string `json:"pubkey"`
			Account struct {
				Data struct {
					Parsed struct {
						Info struct {
							TokenAmount struct {
								UIAmountString string `json:"uiAmountString"`
							} `json:"tokenAmount"`
						} `json:"info"`
					} `json:"parsed"`
				} `json:"data"`
			} `json:"account"`
		} `json:"value"`
	}
	err := r.call(ctx, "getTokenAccountsByOwner", []any{owner, map[string]any{"mint": mint},
		map[string]any{"encoding": "jsonParsed", "commitment": "confirmed"}}, &out)
	if err != nil {
		return nil, err
	}
	accts := map[string]float64{}
	for _, v := range out.Value {
		f, _ := strconv.ParseFloat(v.Account.Data.Parsed.Info.TokenAmount.UIAmountString, 64)
		accts[v.Pubkey] = f
	}
	return accts, nil
}

// TxEvents is the raw material for decoding program events from one transaction:
// log lines and inner-instruction data (where emit_cpi events live).
type TxEvents struct {
	Signature string
	BlockTime time.Time
	Logs      []string
	InnerData [][]byte
	// InnerPrograms[i] is the program that ran InnerData[i] (same length when known).
	InnerPrograms []string
	Failed        bool
}

func (r *RPC) GetTxEvents(ctx context.Context, sig string) (TxEvents, error) {
	var tx struct {
		BlockTime int64 `json:"blockTime"`
		Meta      struct {
			Err               any      `json:"err"`
			LogMessages       []string `json:"logMessages"`
			InnerInstructions []struct {
				Instructions []struct {
					ProgramIDIndex int    `json:"programIdIndex"`
					Data           string `json:"data"`
				} `json:"instructions"`
			} `json:"innerInstructions"`
		} `json:"meta"`
		Transaction struct {
			Message struct {
				AccountKeys []string `json:"accountKeys"`
			} `json:"message"`
		} `json:"transaction"`
	}
	err := r.call(ctx, "getTransaction", []any{sig, map[string]any{"encoding": "json", "commitment": "confirmed",
		"maxSupportedTransactionVersion": 1}}, &tx)
	if err != nil {
		return TxEvents{}, err
	}
	ev := TxEvents{Signature: sig, BlockTime: time.Unix(tx.BlockTime, 0).UTC(), Logs: tx.Meta.LogMessages, Failed: tx.Meta.Err != nil}
	keys := tx.Transaction.Message.AccountKeys // invoked programs are always static keys
	for _, ii := range tx.Meta.InnerInstructions {
		for _, in := range ii.Instructions {
			if b, err := base58.Decode(in.Data); err == nil {
				prog := ""
				if in.ProgramIDIndex >= 0 && in.ProgramIDIndex < len(keys) {
					prog = keys[in.ProgramIDIndex]
				}
				ev.InnerData = append(ev.InnerData, b)
				ev.InnerPrograms = append(ev.InnerPrograms, prog)
			}
		}
	}
	return ev, nil
}

func postJSONRPC(ctx context.Context, c *http.Client, url, method string, params []any, out any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return fmt.Errorf("%s: %w", method, ErrRateLimited)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *rpcErr         `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("%s: decode (%s): %w", method, resp.Status, err)
	}
	if env.Error != nil {
		return &RPCError{Method: method, Code: env.Error.Code, Message: env.Error.Message}
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// SendTransaction submits a signed tx. We retry ourselves, so maxRetries is 0.
func (r *RPC) SendTransaction(ctx context.Context, raw []byte) (string, error) {
	var sig string
	err := r.call(ctx, "sendTransaction", []any{base64.StdEncoding.EncodeToString(raw),
		map[string]any{"encoding": "base64", "skipPreflight": true, "maxRetries": 0}}, &sig)
	return sig, err
}

// SimResult is what the node says a transaction would do. Nothing is sent.
type SimResult struct {
	Err   string // "" when it would succeed
	Logs  []string
	Units uint64
}

// SimulateTransaction runs raw against current chain state without sending it.
// With sigVerify the signatures must be valid (our own signed tx); without it the
// node swaps in a fresh blockhash, so an unsigned tx built for another wallet works.
func (r *RPC) SimulateTransaction(ctx context.Context, raw []byte, sigVerify bool) (SimResult, error) {
	var res struct {
		Value struct {
			Err   any      `json:"err"`
			Logs  []string `json:"logs"`
			Units uint64   `json:"unitsConsumed"`
		} `json:"value"`
	}
	err := r.call(ctx, "simulateTransaction", []any{base64.StdEncoding.EncodeToString(raw),
		map[string]any{"encoding": "base64", "sigVerify": sigVerify, "replaceRecentBlockhash": !sigVerify, "commitment": "processed"}}, &res)
	if err != nil {
		return SimResult{}, err
	}
	out := SimResult{Logs: res.Value.Logs, Units: res.Value.Units}
	if res.Value.Err != nil {
		b, _ := json.Marshal(res.Value.Err)
		out.Err = string(b)
	}
	return out, nil
}

type SigStatus struct {
	Found              bool
	Err                string
	ConfirmationStatus string
}

func (r *RPC) SignatureStatus(ctx context.Context, sig string) (SigStatus, error) {
	var res struct {
		Value []*struct {
			Err                any    `json:"err"`
			ConfirmationStatus string `json:"confirmationStatus"`
		} `json:"value"`
	}
	if err := r.call(ctx, "getSignatureStatuses", []any{[]string{sig}, map[string]any{"searchTransactionHistory": false}}, &res); err != nil {
		return SigStatus{}, err
	}
	if len(res.Value) == 0 || res.Value[0] == nil {
		return SigStatus{}, nil
	}
	v := res.Value[0]
	st := SigStatus{Found: true, ConfirmationStatus: v.ConfirmationStatus}
	if v.Err != nil {
		b, _ := json.Marshal(v.Err)
		st.Err = string(b)
	}
	return st, nil
}

func (r *RPC) IsBlockhashValid(ctx context.Context, blockhash string) (bool, error) {
	var res struct {
		Value bool `json:"value"`
	}
	err := r.call(ctx, "isBlockhashValid", []any{blockhash, map[string]any{"commitment": "processed"}}, &res)
	return res.Value, err
}

// BalanceChange is what a confirmed tx did to our wallet.
type BalanceChange struct {
	SolDelta   float64 // post - pre, includes network fee (negative on buys)
	TokenDelta float64 // post - pre for the mint, UI units
	TokenAfter float64 // owner's balance of the mint after the tx, UI units
	// TokenAccounts are the owner's token accounts for the mint that the tx touched.
	TokenAccounts []string
	FeeSol        float64
	BlockTime     time.Time
}

// GetBalanceChange reads a confirmed tx and returns our SOL and token deltas.
func (r *RPC) GetBalanceChange(ctx context.Context, sig, owner, mint string) (BalanceChange, error) {
	var tx struct {
		BlockTime int64 `json:"blockTime"`
		Meta      struct {
			Fee          uint64   `json:"fee"`
			PreBalances  []uint64 `json:"preBalances"`
			PostBalances []uint64 `json:"postBalances"`
			PreToken     []tokBal `json:"preTokenBalances"`
			PostToken    []tokBal `json:"postTokenBalances"`
		} `json:"meta"`
		Transaction struct {
			Message struct {
				AccountKeys []struct {
					Pubkey string `json:"pubkey"`
				} `json:"accountKeys"`
			} `json:"message"`
		} `json:"transaction"`
	}
	err := r.call(ctx, "getTransaction", []any{sig, map[string]any{"encoding": "jsonParsed",
		"commitment": "confirmed", "maxSupportedTransactionVersion": 1}}, &tx)
	if err != nil {
		return BalanceChange{}, err
	}
	idx := -1
	for i, k := range tx.Transaction.Message.AccountKeys {
		if k.Pubkey == owner {
			idx = i
			break
		}
	}
	if idx < 0 || idx >= len(tx.Meta.PreBalances) || idx >= len(tx.Meta.PostBalances) {
		return BalanceChange{}, errors.New("owner not in transaction")
	}
	bc := BalanceChange{
		SolDelta:  (float64(tx.Meta.PostBalances[idx]) - float64(tx.Meta.PreBalances[idx])) / LamportsPerSol,
		FeeSol:    float64(tx.Meta.Fee) / LamportsPerSol,
		BlockTime: time.Unix(tx.BlockTime, 0),
	}
	bc.TokenAfter = sumTok(tx.Meta.PostToken, owner, mint)
	bc.TokenDelta = bc.TokenAfter - sumTok(tx.Meta.PreToken, owner, mint)
	seen := map[string]bool{}
	for _, b := range append(tx.Meta.PreToken, tx.Meta.PostToken...) {
		if b.Owner == owner && b.Mint == mint && b.AccountIndex < len(tx.Transaction.Message.AccountKeys) {
			if k := tx.Transaction.Message.AccountKeys[b.AccountIndex].Pubkey; !seen[k] {
				seen[k] = true
				bc.TokenAccounts = append(bc.TokenAccounts, k)
			}
		}
	}
	return bc, nil
}

type tokBal struct {
	AccountIndex  int    `json:"accountIndex"`
	Mint          string `json:"mint"`
	Owner         string `json:"owner"`
	UITokenAmount struct {
		UIAmountString string `json:"uiAmountString"`
	} `json:"uiTokenAmount"`
}

func sumTok(bs []tokBal, owner, mint string) float64 {
	t := 0.0
	for _, b := range bs {
		if b.Owner == owner && b.Mint == mint {
			var f float64
			fmt.Sscanf(b.UITokenAmount.UIAmountString, "%g", &f)
			t += f
		}
	}
	return t
}

// SendJitoBundle submits base58-encoded signed txs as one bundle.
func SendJitoBundle(ctx context.Context, c *http.Client, url string, txsBase58 []string) (string, error) {
	var id string
	err := postJSONRPC(ctx, c, url, "sendBundle", []any{txsBase58}, &id)
	return id, err
}
