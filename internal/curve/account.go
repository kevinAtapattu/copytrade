package curve

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"filippo.io/edwards25519"
	"github.com/mr-tron/base58"
)

// PumpProgram is the pump.fun bonding-curve program.
const PumpProgram = "6EF8rrecthR5Dkzon8Nwu78hRvfCKubJ14M5uBEwF6P"

// Account is the on-chain BondingCurve account, converted to UI units.
type Account struct {
	State      State // virtual reserves: SOL and tokens (6 decimals)
	RealTokens float64
	RealSol    float64
	Complete   bool // true once the curve has migrated
	// QuoteMint is set when the curve is priced in a token other than SOL (pump.fun
	// started launching these in 2026). Our SOL math does not apply to them.
	QuoteMint string
}

// ParseAccount decodes BondingCurve account data:
// 8-byte discriminator, then u64 virtual_token_reserves, virtual_sol_reserves,
// real_token_reserves, real_sol_reserves, token_total_supply, then bool complete.
// Newer versions append more fields, which are ignored.
func ParseAccount(data []byte) (Account, error) {
	if len(data) < 8+5*8+1 {
		return Account{}, errors.New("bonding curve account too short")
	}
	u := func(i int) float64 { return float64(binary.LittleEndian.Uint64(data[8+8*i:])) }
	a := Account{
		State:      State{VTokens: u(0) / 1e6, VSol: u(1) / 1e9},
		RealTokens: u(2) / 1e6,
		RealSol:    u(3) / 1e9,
		Complete:   data[48] != 0,
	}
	// Layout after complete: creator pubkey [49:81], two flag bytes [81:83], then the
	// quote mint [83:115] (all zero for SOL). Observed on mainnet, October 2026.
	if len(data) >= 115 {
		q := data[83:115]
		for _, c := range q {
			if c != 0 {
				a.QuoteMint = base58.Encode(q)
				break
			}
		}
	}
	return a, nil
}

// DeriveBondingCurve finds the bonding curve PDA for a mint: seeds ["bonding-curve", mint].
// Used only when the stream did not tell us the curve address.
func DeriveBondingCurve(mint string) (string, error) {
	m, err := base58.Decode(mint)
	if err != nil || len(m) != 32 {
		return "", errors.New("bad mint address")
	}
	prog, _ := base58.Decode(PumpProgram)
	addr, err := findProgramAddress([][]byte{[]byte("bonding-curve"), m}, prog)
	if err != nil {
		return "", err
	}
	return base58.Encode(addr), nil
}

// FindPDA derives a program address from seeds (base58 program id).
func FindPDA(seeds [][]byte, program string) ([]byte, error) {
	prog, err := base58.Decode(program)
	if err != nil || len(prog) != 32 {
		return nil, errors.New("bad program id")
	}
	return findProgramAddress(seeds, prog)
}

func findProgramAddress(seeds [][]byte, program []byte) ([]byte, error) {
	for bump := 255; bump >= 0; bump-- {
		h := sha256.New()
		for _, s := range seeds {
			h.Write(s)
		}
		h.Write([]byte{byte(bump)})
		h.Write(program)
		h.Write([]byte("ProgramDerivedAddress"))
		sum := h.Sum(nil)
		if _, err := new(edwards25519.Point).SetBytes(sum); err != nil { // off the curve: valid PDA
			return sum, nil
		}
	}
	return nil, errors.New("no viable bump seed")
}
