package scoring

import "math/big"

// BestBaseScore is the default non-Grader aggregation policy. Values stay decimal.
func BestBaseScore(scores []*big.Rat) *big.Rat {
	var best *big.Rat
	for _, s := range scores {
		if s == nil || s.Sign() < 0 {
			continue
		}
		if best == nil || s.Cmp(best) > 0 {
			best = new(big.Rat).Set(s)
		}
	}
	return best
}
