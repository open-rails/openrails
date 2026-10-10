package nmi

import (
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ParseSaleResponse parses a classic Direct Post sale body; the
// custodian-proxied transport shares it, so both use one decline taxonomy.
// A decline (response=2) is a *CustomerVaultError with the verbatim code; any
// other non-approval or unreadable body is a TransportAmbiguousError.
func ParseSaleResponse(raw string) (*SaleResponse, error) {
	output, err := parseDirectResponse(raw)
	if err != nil {
		return nil, err
	}
	if !isDirectResponseApproved(output) {
		return nil, newSaleError(raw, output)
	}
	return saleResponse(output, raw), nil
}

func saleResponse(output url.Values, raw string) *SaleResponse {
	return &SaleResponse{
		TransactionID: output.Get("transactionid"),
		Authcode:      output.Get("authcode"),
		ResponseText:  responseText(output, raw),
		AVSResponse:   strings.TrimSpace(output.Get("avsresponse")),
		CVVResponse:   strings.TrimSpace(output.Get("cvvresponse")),
	}
}

// WireAmount renders rail minor units as NMI's major-unit decimal, at the
// currency's scale. NMI's x.xx format is retained for zero-decimal currencies:
// four JPY rail units become "4.00", never "0.04". No float arithmetic.
func WireAmount(amount moneyutil.Cents, currency string) (string, error) {
	if err := moneyutil.RequireFiatCurrency(currency); err != nil {
		return "", err
	}
	units, _ := moneyutil.LookupCurrency(currency)
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
