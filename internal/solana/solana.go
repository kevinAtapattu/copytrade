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
	"strings"
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

func (r *RPC) call(ctx context.Context, method string, params []any, out any) error {
	return postJSONRPC(ctx, r.Client, r.URL, method, params, out)
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
	FeeSol     float64
	BlockTime  time.Time
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
		"commitment": "confirmed", "maxSupportedTransactionVersion": 0}}, &tx)
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
	bc.TokenDelta = sumTok(tx.Meta.PostToken, owner, mint) - sumTok(tx.Meta.PreToken, owner, mint)
	return bc, nil
}

type tokBal struct {
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
