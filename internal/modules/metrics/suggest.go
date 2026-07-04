package metrics

import "strings"

// suggest returns the closest candidate by edit distance when it is close
// enough to be a plausible typo.
func suggest(input string, candidates []string) (string, bool) {
	input = strings.ToLower(input)
	best, bestDist := "", 1<<30
	for _, c := range candidates {
		d := editDistance(input, strings.ToLower(c))
		if d < bestDist {
			best, bestDist = c, d
		}
	}
	// Allow more slack for longer words; reject unrelated suggestions.
	maxDist := 1 + len(input)/4
	if maxDist > 4 {
		maxDist = 4
	}
	if best != "" && bestDist <= maxDist {
		return best, true
	}
	return "", false
}

func editDistance(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min3(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

func min3(a, b, c int) int {
	if b < a {
		a = b
	}
	if c < a {
		a = c
	}
	return a
}
