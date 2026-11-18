// keygen makes a fresh Solana hot wallet for live trading and writes its secret
// key straight into .env (TRADER_PRIVATE_KEY), so the key is never printed,
// logged or copied through a chat. It prints only the wallet address. It refuses
// to run if TRADER_PRIVATE_KEY already has a value, so it can't overwrite a
// funded wallet's key.
//
// To see the wallet in Phantom: Add / Connect Wallet, Import Private Key, and
// paste the TRADER_PRIVATE_KEY value from .env.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"os"
	"strings"

	"github.com/mr-tron/base58"
)

func main() {
	const path, name = ".env", "TRADER_PRIVATE_KEY="
	b, err := os.ReadFile(path)
	if err != nil {
		fail("read .env (run from the repo root): %v", err)
	}
	lines := strings.Split(string(b), "\n")
	at := -1
	for i, l := range lines {
		if strings.HasPrefix(l, name) {
			if strings.TrimSpace(strings.TrimPrefix(l, name)) != "" {
				fail("TRADER_PRIVATE_KEY is already set in .env; not overwriting it (it may hold funds)")
			}
			at = i
		}
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		fail("generate key: %v", err)
	}
	line := name + base58.Encode(priv) // 64 bytes: secret then public, Phantom's format
	if at >= 0 {
		lines[at] = line
	} else {
		lines = append(lines, line)
	}
	info, _ := os.Stat(path)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), info.Mode().Perm()); err != nil {
		fail("write .env: %v", err)
	}
	fmt.Println("New trading wallet written to .env (TRADER_PRIVATE_KEY).")
	fmt.Println("Address (send SOL here):", base58.Encode(pub))
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, "keygen: "+f+"\n", a...)
	os.Exit(1)
}
