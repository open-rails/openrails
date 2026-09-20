package nmi

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Exported classic Direct Post response surface for sibling rails (#795): the
// custodian-proxied transport receives NMI's classic urlencoded body through the BT
// proxy and must parse it with the SAME parser and decline vocabulary as the
// direct rail — one taxonomy, two transports.

// ParseSaleResponse parses a classic Direct Post sale response body. An
// approval returns a SaleResponse; a parsed non-approval returns a
// *CustomerVaultError carrying the verbatim response code + localization id
// (the decline taxonomy nmidirect classifies on). An unreadable body returns
// a TransportAmbiguousError — the mutation likely executed (#674).
func ParseSaleResponse(raw string) (*SaleResponse, error) {
	output, err := parseDirectResponse(raw)
	if err != nil {
		return nil, err
	}
	if !isDirectResponseApproved(output) {
		return nil, newSaleError(raw, output)
	}
	return &SaleResponse{
		TransactionID: output.Get("transactionid"),
		Authcode:      output.Get("authcode"),
		ResponseText:  responseText(output, raw),
	}, nil
}

// WireAmount renders rail minor units as NMI's major-unit decimal, at the
// currency's scale. NMI's x.xx format is retained for zero-decimal currencies:
// four JPY rail units become "4.00", never "0.04". No float arithmetic.
func WireAmount(amount moneyutil.Cents, currency string) (string, error) {
	units, ok := moneyutil.LookupCurrency(currency)
	if !ok {
		return "", fmt.Errorf("unknown charge currency %q", currency)
	}
	digits := strconv.FormatInt(int64(amount), 10)
	sign := ""
	if strings.HasPrefix(digits, "-") {
		sign = "-"
		digits = digits[1:]
	}
	decimals := units.MinorDecimals
	if len(digits) <= decimals {
		digits = strings.Repeat("0", decimals-len(digits)+1) + digits
	}
	whole, fraction := digits[:len(digits)-decimals], digits[len(digits)-decimals:]
	if len(fraction) < 2 {
		fraction += strings.Repeat("0", 2-len(fraction))
	}
	return sign + whole + "." + fraction, nil
}
