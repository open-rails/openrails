package nmi

import (
	"strconv"
	"strings"
)

// TransactionAction is one Query API action of a transaction.
type TransactionAction struct {
	Type, Success, Amount string
}

// SaleReversed reports whether a transaction's approved sale was taken back:
// voided (condition canceled, or an approved void) or refunded in full. A
// reversed sale executed but paid nothing, so it is never payment evidence.
func SaleReversed(condition string, actions []TransactionAction) bool {
	if strings.EqualFold(strings.TrimSpace(condition), "canceled") {
		return true
	}
	var sold, refunded int64
	for _, a := range actions {
		if strings.TrimSpace(a.Success) != "1" {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(a.Type)) {
		case "void":
			return true
		case "sale":
			sold += amountCents(a.Amount)
		case "refund", "credit":
			refunded += amountCents(a.Amount)
		}
	}
	return sold > 0 && refunded >= sold
}

// amountCents parses a Query API decimal amount ("9.99"); unparseable is 0.
func amountCents(s string) int64 {
	whole, frac, _ := strings.Cut(strings.TrimSpace(s), ".")
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil || w < 0 {
		return 0
	}
	frac = (frac + "00")[:2]
	f, err := strconv.ParseInt(frac, 10, 64)
	if err != nil {
		return 0
	}
	return w*100 + f
}
