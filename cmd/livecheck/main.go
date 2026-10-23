// Command livecheck verifies the mainnet integrations without spending anything:
// it opens ONE short PumpPortal connection on the free new-token stream, then checks
// our bonding-curve address derivation, account parsing and TradeEvent decoding
// against the real chain via SOLANA_RPC_URL.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"os"
	"time"

	"github.com/gorilla/websocket"

	"copytrade/internal/config"
	"copytrade/internal/curve"
	"copytrade/internal/history"
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

func main() {
	c := config.Load()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// 1. One connection, free stream, first token creation.
	u, _ := url.Parse(c.PumpPortalWSURL)
	if c.PumpPortalAPIKey != "" {
		q := u.Query()
		q.Set("api-key", c.PumpPortalAPIKey)
		u.RawQuery = q.Encode()
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, u.String(), nil)
	if err != nil {
		check("pumpportal connect", false, err.Error())
		os.Exit(1)
	}
	conn.WriteJSON(map[string]any{"method": "subscribeNewToken"})
	var tokens []map[string]any
	var firstRaw []byte
	conn.SetReadDeadline(time.Now().Add(120 * time.Second))
	for len(tokens) < 6 {
		_, raw, err := conn.ReadMessage()
		if err != nil {
			check("pumpportal new tokens", false, err.Error())
			os.Exit(1)
		}
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil && m["mint"] != nil {
			if firstRaw == nil {
				firstRaw = raw
			}
			tokens = append(tokens, m)
		}
	}
	conn.WriteJSON(map[string]any{"method": "unsubscribeNewToken"})
	conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	conn.Close()
	fmt.Printf("\nfirst new-token message:\n%s\n\n", firstRaw)
	var missing []string
	for _, k := range []string{"signature", "mint", "traderPublicKey", "txType", "bondingCurveKey", "vSolInBondingCurve", "vTokensInBondingCurve", "marketCapSol", "pool"} {
		if _, ok := tokens[0][k]; !ok {
			missing = append(missing, k)
		}
	}
	check("message fields", len(missing) == 0, fmt.Sprintf("missing %v", missing))

	fmt.Println("waiting 30s for the tokens to get some trades...")
	time.Sleep(30 * time.Second)
	rpc := solana.NewRPC(c.SolanaRPCURL)
	rpc.RPS = 2
	pdaOK, parsedSOL, nonSOL, decoded, consistent := 0, 0, 0, 0, 0
	for _, tk := range tokens {
		mint, _ := tk["mint"].(string)
		key, _ := tk["bondingCurveKey"].(string)
		if d, err := curve.DeriveBondingCurve(mint); err == nil && d == key {
			pdaOK++
		}
		datas, err := rpc.GetMultipleAccounts(ctx, []string{key})
		if err != nil || len(datas) == 0 || datas[0] == nil {
			fmt.Printf("  %s: curve read failed: %v\n", mint[:6], err)
			continue
		}
		acc, err := curve.ParseAccount(datas[0])
		if err != nil {
			continue
		}
		if acc.QuoteMint != "" {
			nonSOL++
			fmt.Printf("  %s: non-SOL quote %s (would be skipped)\n", mint[:6], acc.QuoteMint)
			continue
		}
		k := acc.State.VSol * acc.State.VTokens
		if acc.State.VSol >= 29 && acc.State.VSol < 120 && math.Abs(k/32.19e9-1) < 0.02 {
			parsedSOL++
		}
		sigs, err := rpc.GetSignaturesForAddress(ctx, key, "", 5)
		if err != nil {
			continue
		}
		for i := len(sigs) - 1; i >= 0; i-- {
			if sigs[i].Err != nil {
				continue
			}
			tx, err := rpc.GetTxEvents(ctx, sigs[i].Signature)
			if err != nil {
				fmt.Printf("  %s: getTransaction: %v\n", mint[:6], err)
				continue
			}
			for _, t := range history.ToTrades(tx, mint, key) {
				decoded++
				// a decoded event's post-trade reserves must sit on the same constant-product curve
				if math.Abs(t.VSol*t.VTokens/32.19e9-1) < 0.02 {
					consistent++
				}
			}
		}
		fmt.Printf("  %s: vSol=%.3f vTokens=%.0f k/k0=%.4f decoded-so-far=%d\n", mint[:6], acc.State.VSol, acc.State.VTokens, k/32.19e9, decoded)
	}
	n := len(tokens)
	check("bonding curve PDA derivation", pdaOK == n, fmt.Sprintf("%d/%d match the stream", pdaOK, n))
	check("curve account parse (SOL curves)", parsedSOL+nonSOL == n && parsedSOL > 0, fmt.Sprintf("%d SOL curves on the expected k, %d non-SOL detected", parsedSOL, nonSOL))
	check("TradeEvent decoding", decoded > 0 && consistent == decoded, fmt.Sprintf("%d trades decoded, %d on the expected curve", decoded, consistent))
	calls, _ := rpc.Calls()
	fmt.Printf("\nRPC calls used: %d\n", calls)
	if failed {
		os.Exit(1)
	}
}
