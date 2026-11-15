package nmi

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ErrPartialApproval marks a sale the issuer approved for less than its
// amount. It is also transport-ambiguous: the authorization exists, so the
// outcome is settled from NMI's record, never taken as payment or re-sent.
var ErrPartialApproval = errors.New("nmi: sale approved for less than its amount")

// SettleOnlyInFull asks NMI to settle a partially approved sale only once it
// is collected in full (partial_payments=payment_in_full). OpenRails never
// completes one, so a partial approval moves no money.
func SettleOnlyInFull(form url.Values) { form.Set("partial_payments", "payment_in_full") }

// ParseSaleResponse parses a classic Direct Post answer to a sale of amount
// (rail minor units) in currency; the custodian-proxied transport shares it,
// so both use one decline taxonomy. A decline (response=2) is a
// *CustomerVaultError with the verbatim code; a partial approval, any other
// non-approval or an unreadable body is a TransportAmbiguousError.
func ParseSaleResponse(raw string, amount moneyutil.Cents, currency string) (*SaleResponse, error) {
	output, err := parseDirectResponse(raw)
	if err != nil {
		return nil, err
	}
	if !isDirectResponseApproved(output) {
		return nil, newSaleError(raw, output)
	}
	if err := fullApproval(output, amount, currency); err != nil {
		return nil, err
	}
	return saleResponse(output, raw), nil
}

// fullApproval refuses an approval for less than amount: NMI's
// amount_authorized differs from it, or a partial_payment_balance remains.
// Without either field the exact readback decides. A verification (amount 0)
// takes nothing.
func fullApproval(output url.Values, amount moneyutil.Cents, currency string) error {
	if amount == 0 {
		return nil
	}
	authorized := strings.TrimSpace(output.Get("amount_authorized"))
	balance := strings.TrimSpace(output.Get("partial_payment_balance"))
	if authorized != "" {
		if cents, err := moneyutil.DecimalToRailMinor(currency, authorized); err != nil || cents != amount {
			return partialApproval(authorized, balance)
		}
	}
	if partialBalance(balance) {
		return partialApproval(authorized, balance)
	}
	return nil
}

func partialApproval(authorized, balance string) error {
	return errors.Join(ErrPartialApproval, ambiguous(fmt.Errorf("NMI approved %q of the sale with %q unpaid", authorized, balance)))
}

// partialBalance reports whether NMI's partial_payment_balance names an
// unpaid remainder. Absent or zero is none; anything unreadable is one.
func partialBalance(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	digits := strings.Replace(raw, ".", "", 1)
	return digits == "" || strings.Trim(digits, "0") != ""
}

func saleResponse(output url.Values, raw string) *SaleResponse {
	return &SaleResponse{
		TransactionID:   output.Get("transactionid"),
		CustomerVaultID: strings.TrimSpace(output.Get("customer_vault_id")),
		Authcode:        output.Get("authcode"),
		ResponseText:    responseText(output, raw),
		AVSResponse:     strings.TrimSpace(output.Get("avsresponse")),
		CVVResponse:     strings.TrimSpace(output.Get("cvvresponse")),
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
