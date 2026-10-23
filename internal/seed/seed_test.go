package seed

import (
	"context"
	"os"
	"strings"
	"testing"

	"copytrade/internal/model"
	"copytrade/internal/store"
)

func TestParse(t *testing.T) {
	in := `# comment
7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU  big whale  # trailing
7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU dupe
not-an-address
9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM,comma-label
`
	ws := Parse(strings.NewReader(in))
	if len(ws) != 2 || ws[0].Label != "big whale" || ws[1].Label != "comma-label" {
		t.Fatalf("parsed %+v", ws)
	}
}

func TestMissingFileIsOK(t *testing.T) {
	n, err := Wallets(context.Background(), store.NewMemory(), "/nonexistent/wallets.txt")
	if n != 0 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestSeedKeepsExistingStatus(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	st.AddWallet(ctx, model.Wallet{Address: "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU", Status: model.StatusLive})
	p := t.TempDir() + "/w.txt"
	writeFile(t, p, "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU\n9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM\n")
	n, err := Wallets(ctx, st, p)
	w, _, _ := st.GetWallet(ctx, "7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU")
	if err != nil || n != 1 || w.Status != model.StatusLive {
		t.Fatalf("n=%d err=%v status=%s", n, err, w.Status)
	}
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}
