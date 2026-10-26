package agent

import (
	"math/big"
	"testing"
)

// Regression: float rounding at the wei level made a 3.2M mint fall a few wei short of
// paying 200k + 3M, so the lending seed transfer reverted for some pool depths.
func TestE18IsAdditive(t *testing.T) {
	for _, pool := range []float64{100, 150, 200, 400, 500} {
		a, b := pool*2000, 3_000_000.0
		if E18(a+b).Cmp(new(big.Int).Add(E18(a), E18(b))) != 0 {
			t.Fatalf("pool %v: E18(a+b) != E18(a)+E18(b)", pool)
		}
	}
	if F(E18(1234.567891)) != 1234.567891 {
		t.Fatalf("round trip: %v", F(E18(1234.567891)))
	}
}
