package nmi

import (
	"fmt"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// Exported classic Direct Post response surface for sibling rails (#795): the
// vaulted_card rail receives NMI's classic urlencoded body through the BT
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

// WireAmount renders integer cents as the exact two-decimal wire string NMI
// charges in (199 -> "1.99"). Money wall: never float math (#671).
func WireAmount(cents moneyutil.Cents) string {
	neg := ""
	if cents < 0 {
		neg, cents = "-", -cents
	}
	return fmt.Sprintf("%s%d.%02d", neg, cents/100, cents%100)
}
