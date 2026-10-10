// Package cardguard is the one card-number detector behind OpenRails' PAN
// firewall (SAQ A): a card number in any request field is refused, never
// stored or forwarded, with no per-field exemptions. The one exception is
// Card, the typed `card` field of a PSP whose card_entry is server.
package cardguard

const (
	minPANDigits = 13
	maxPANDigits = 19
)

// ContainsPAN reports whether s contains a payment card number. A candidate is
// a run of digit groups joined only by card formatting (spaces, at most one
// dash per gap); it is a PAN when:
//
//  1. Grouping: issuers print it so (one run, 4-4-4-… with a short tail, or
//     Amex/Diners 4-6-5 and 4-6-4).
//  2. Standalone: a separated candidate touches no letter at either end, as
//     an identifier's digit groups always do.
//  3. Luhn: the check digit passes.
//  4. Range: its leading digits and length are an issued range (issuedRange).
//
// Rules 1 and 2 exclude UUIDs exactly; rule 4 excludes 13/16/19-digit epoch
// keys starting with 1. An unseparated 13-19 digit run is a candidate wherever
// it sits.
func ContainsPAN(s string) bool {
	if len(s) < minPANDigits {
		return false
	}
	groups := digitGroups(s)
	digits := make([]byte, 0, maxPANDigits)
	lens := make([]int, 0, maxPANDigits)
	for i := range groups {
		digits, lens = digits[:0], lens[:0]
		for j := i; j < len(groups); j++ {
			if j > i && !groups[j].joined {
				break
			}
			if len(digits)+len(groups[j].digits) > maxPANDigits {
				break
			}
			digits = append(digits, groups[j].digits...)
			lens = append(lens, len(groups[j].digits))
			if len(digits) < minPANDigits {
				continue
			}
			// Rule 2 applies to separated candidates only: an unseparated run
			// is one group and stays a candidate wherever it sits.
			if j > i && (groups[i].letterBefore || groups[j].letterAfter) {
				continue
			}
			if plausibleGrouping(lens) && luhnValid(digits) && issuedRange(digits) {
				return true
			}
		}
	}
	return false
}

// digitGroup is one run of digits, whether card formatting — and nothing else —
// separated it from the run before it, and whether a letter butts against it.
type digitGroup struct {
	digits       []byte
	joined       bool
	letterBefore bool
	letterAfter  bool
}

func digitGroups(s string) []digitGroup {
	var out []digitGroup
	gap := 0 // start of the non-digit gap preceding the current position
	for i := 0; i < len(s); {
		if !isDigit(s[i]) {
			i++
			continue
		}
		start := i
		for i < len(s) && isDigit(s[i]) {
			i++
		}
		group := digitGroup{digits: make([]byte, i-start)}
		for k := range group.digits {
			group.digits[k] = s[start+k] - '0'
		}
		group.joined = len(out) > 0 && isCardFormatting(s[gap:start])
		group.letterBefore = start > 0 && isLetter(s[start-1])
		group.letterAfter = i < len(s) && isLetter(s[i])
		out = append(out, group)
		gap = i
	}
	return out
}

// isCardFormatting reports whether a gap between two digit runs is the
// formatting of a single written card number: spaces, with at most one dash.
// Anything else — a letter, punctuation, a second dash — ends the candidate.
func isCardFormatting(gap string) bool {
	if gap == "" {
		return false
	}
	dashes := 0
	for i := 0; i < len(gap); i++ {
		switch gap[i] {
		case ' ':
		case '-':
			dashes++
		default:
			return false
		}
	}
	return dashes <= 1
}

// plausibleGrouping reports whether these group lengths are how a card number
// is written. Unformatted is one group; formatted is 4-digit groups with a
// tail of 1-4, except Amex 4-6-5 and Diners 4-6-4.
func plausibleGrouping(lens []int) bool {
	if len(lens) == 1 {
		return true
	}
	if len(lens) == 3 && lens[0] == 4 && lens[1] == 6 && (lens[2] == 5 || lens[2] == 4) {
		return true
	}
	last := len(lens) - 1
	for _, n := range lens[:last] {
		if n != 4 {
			return false
		}
	}
	return lens[last] >= 1 && lens[last] <= 4
}

// issuedRange reports whether digits start inside an issuer identification
// range issued at that length, by ISO/IEC 7812 major industry identifier and
// the published brand tables:
//
//	0        none (ISO/TC 68 assignments, not card accounts)
//	1        15      UATP
//	2        16, 19  Mastercard 2-series, Mir
//	3        14-19   Amex 15, Diners 14/16/19, JCB 16-19
//	4        13-19   Visa
//	5        13-19   Mastercard, Maestro, Diners US
//	6        13-19   Discover, UnionPay, Maestro, RuPay, WEX
//	7        19      Fuelman / FleetOne fleet cards
//	8        15, 19  Voyager fleet cards
//	9        none (national assignment, not issued as a network PAN)
//
// The windows are a deliberate superset of every brand table, so no card any
// OpenRails rail can present is excluded by them.
func issuedRange(digits []byte) bool {
	n := len(digits)
	switch digits[0] {
	case 1:
		return n == 15
	case 2:
		return n == 16 || n == 19
	case 3:
		return n >= 14
	case 4, 5, 6:
		return true
	case 7:
		return n == 19
	case 8:
		return n == 15 || n == 19
	default:
		return false
	}
}

func luhnValid(digits []byte) bool {
	sum := 0
	double := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := int(digits[i])
		if double {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

func isLetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c >= 0x80
}
