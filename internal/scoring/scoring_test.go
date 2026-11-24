package scoring

import (
	"math/big"
	"testing"
)

func TestBestBaseScore(t *testing.T) {
	got := BestBaseScore([]*big.Rat{big.NewRat(2, 1), big.NewRat(9, 2), big.NewRat(3, 1)})
	if got.Cmp(big.NewRat(9, 2)) != 0 {
		t.Fatalf("got %s", got)
	}
}
func TestBestBaseScoreEmpty(t *testing.T) {
	if BestBaseScore(nil) != nil {
		t.Fatal("empty score set should remain undefined")
	}
}
