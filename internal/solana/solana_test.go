package solana

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"testing"
	"time"

	"github.com/mr-tron/base58"
)

func testKey(t *testing.T) Keypair {
	seed := make([]byte, 32)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	pk := ed25519.NewKeyFromSeed(seed)
	kp, err := ParseKeypair(base58.Encode(pk))
	if err != nil {
		t.Fatal(err)
	}
	return kp
}

func TestParseKeypairFormats(t *testing.T) {
	kp := testKey(t)
	arr := make([]int, 64)
	for i, b := range kp.Private {
		arr[i] = int(b)
	}
	js, _ := json.Marshal(arr)
	kp2, err := ParseKeypair(string(js))
	if err != nil || kp2.PublicBase58() != kp.PublicBase58() {
		t.Fatalf("json array form: %v", err)
	}
	bad := append([]byte(nil), kp.Private...)
	bad[40] ^= 1
	if _, err := ParseKeypair(base58.Encode(bad)); err == nil {
		t.Fatal("mismatched public half should fail")
	}
	if _, err := ParseKeypair("notbase58!!"); err == nil {
		t.Fatal("garbage should fail")
	}
}

// buildTx makes an unsigned v0 tx with signer as fee payer plus one other key.
func buildTx(signer []byte, versioned bool) []byte {
	var msg []byte
	if versioned {
		msg = append(msg, 0x80)
	}
	msg = append(msg, 1, 0, 1) // header: 1 required sig
	msg = append(msg, 2)       // 2 account keys
	msg = append(msg, signer...)
	other := make([]byte, 32)
	other[0] = 9
	msg = append(msg, other...)
	bh := make([]byte, 32)
	bh[5] = 7
	msg = append(msg, bh...)
	msg = append(msg, 0) // 0 instructions
	if versioned {
		msg = append(msg, 0) // 0 address table lookups
	}
	tx := []byte{1}
	tx = append(tx, make([]byte, 64)...)
	return append(tx, msg...)
}

func TestSignVersionedAndLegacy(t *testing.T) {
	kp := testKey(t)
	for _, v := range []bool{true, false} {
		raw := buildTx(kp.Public, v)
		tx, err := ParseTx(raw)
		if err != nil {
			t.Fatal(err)
		}
		if tx.RequiredSigs != 1 || len(tx.AccountKeys) != 2 || tx.Blockhash[5] != 7 {
			t.Fatalf("parse v=%v: %+v", v, tx)
		}
		if err := tx.Sign(kp); err != nil {
			t.Fatal(err)
		}
		sig, _ := base58.Decode(tx.Signature())
		if !ed25519.Verify(kp.Public, tx.Message(), sig) {
			t.Fatal("signature does not verify")
		}
		if len(tx.Raw) != len(raw) {
			t.Fatal("signing must not change length")
		}
	}
}

func TestSignRejectsForeignKey(t *testing.T) {
	kp := testKey(t)
	other := make([]byte, 32)
	tx, _ := ParseTx(buildTx(other, true))
	if err := tx.Sign(kp); err == nil {
		t.Fatal("must refuse to sign when not a required signer")
	}
}

func TestParseTruncated(t *testing.T) {
	raw := buildTx(make([]byte, 32), true)
	if _, err := ParseTx(raw[:80]); err == nil {
		t.Fatal("truncated tx should fail")
	}
}

func TestRateLimitAndCount(t *testing.T) {
	r := &RPC{RPS: 20}
	start := time.Now()
	for i := 0; i < 5; i++ {
		if err := r.wait(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// 5 calls at 20/s: the last waits ~200ms
	if el := time.Since(start); el < 180*time.Millisecond || el > 600*time.Millisecond {
		t.Fatalf("elapsed %v", el)
	}
	if today, total := r.Calls(); today != 5 || total != 5 {
		t.Fatalf("calls %d %d", today, total)
	}
}

func TestRefusesV1Transactions(t *testing.T) {
	raw := append([]byte{0x81, 0x01, 0x00, 0x31}, make([]byte, 200)...)
	if _, err := ParseTx(raw); err == nil {
		t.Fatal("v1 transactions must be refused, not mis-signed")
	}
}
