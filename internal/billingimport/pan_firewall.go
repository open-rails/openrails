package billingimport

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/internal/cardguard"
)

// ErrInvalidDeclaredInput identifies a refused host-authored field before any
// database work. HTTP callers receive the same invalid_param contract.
var ErrInvalidDeclaredInput = errors.New("import billing: invalid declared input")

// A declared book is host-authored free text that lands in the same columns
// checkout writes and the merchant archive later exports: rail_method_ref,
// initial_transaction_id, last_four, card_type, expiry_date, source_id,
// user_email and the verbatim legacy `evidence` blob. Checkout scans those
// names and the archive scans those columns, so this door — the only other way
// into them — scans them too. A card number in a declared book would be stored
// under SAQ A exactly as one pasted into checkout would.
//
// Fail closed on the whole book: a declared import is one transaction, and a
// PAN in any row means the host's export is leaking cards, not that one row is
// bad.
func rejectDeclaredPANs(book DeclaredBilling) error {
	refuse := func(where, value string) error {
		if !cardguard.ContainsPAN(value) {
			return nil
		}
		return fmt.Errorf("%w: %s contains a card-number-shaped value: raw PANs must never reach OpenRails (SAQ A) — declare the provider's vault handle, never the card", ErrInvalidDeclaredInput, where)
	}
	scan := func(where string, values ...string) error {
		for _, value := range values {
			if err := refuse(where, value); err != nil {
				return err
			}
		}
		return nil
	}

	if err := scan("default_psp.key", book.DefaultPSP.Key); err != nil {
		return err
	}
	for i, customer := range book.Customers {
		if err := scan(fmt.Sprintf("customers[%d]", i), customer.Email); err != nil {
			return err
		}
	}
	for i, method := range book.PaymentMethods {
		if err := scan(fmt.Sprintf("payment_methods[%d]", i),
			method.Rail, method.PSP.Key, method.RailCustomerRef, method.RailMethodRef,
			method.InitialTransactionID, method.LastFour, method.CardType, method.ExpiryDate,
		); err != nil {
			return err
		}
	}
	for i, sub := range book.Subscriptions {
		where := fmt.Sprintf("subscriptions[%d]", i)
		if err := scan(where,
			sub.SourceID, sub.Rail, sub.RailSubscriptionID, sub.PSP.Key, sub.UserEmail,
			sub.Cancel.Kind,
		); err != nil {
			return err
		}
		if err := scanEvidencePANs(where+".evidence", sub.Evidence, refuse); err != nil {
			return err
		}
		if sub.PaymentMethod != nil {
			if err := scan(where+".payment_method",
				sub.PaymentMethod.Rail, sub.PaymentMethod.RailCustomerRef, sub.PaymentMethod.RailMethodRef,
			); err != nil {
				return err
			}
		}
	}
	for i, txn := range book.Transactions {
		if err := scan(fmt.Sprintf("transactions[%d]", i),
			txn.RailSubscriptionID, txn.TransactionID, txn.Type, txn.Currency,
		); err != nil {
			return err
		}
	}
	for i, grant := range book.AdminGrants {
		if err := scan(fmt.Sprintf("admin_grants[%d]", i), grant.SourceID); err != nil {
			return err
		}
	}
	return nil
}

// RawMessage preserves JSON escapes. Scan decoded tokens, including object keys
// and every duplicate-key occurrence, before PostgreSQL normalizes the JSONB.
func scanEvidencePANs(where string, raw json.RawMessage, refuse func(string, string) error) error {
	if len(raw) == 0 {
		return nil
	}
	if !json.Valid(raw) {
		return fmt.Errorf("%w: %s must be valid JSON", ErrInvalidDeclaredInput, where)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("%w: %s must be valid JSON", ErrInvalidDeclaredInput, where)
		}
		var value string
		switch token := token.(type) {
		case string:
			value = token
		case json.Number:
			value = token.String()
			if err := refuse(where, wholeJSONNumber(value)); err != nil {
				return err
			}
		default:
			continue
		}
		if err := refuse(where, value); err != nil {
			return err
		}
	}
}

// PostgreSQL expands JSON exponents: 4.111111111111111e15 becomes a bare PAN.
// Expand only whole numbers within PAN length, without floats or large powers.
// The input is already a valid JSON number from Decoder.Token.
func wholeJSONNumber(number string) string {
	mantissa := strings.TrimPrefix(number, "-")
	exponent := int64(0)
	if i := strings.IndexAny(mantissa, "eE"); i >= 0 {
		var err error
		exponent, err = strconv.ParseInt(mantissa[i+1:], 10, 32)
		if err != nil {
			return ""
		}
		mantissa = mantissa[:i]
	}
	if i := strings.IndexByte(mantissa, '.'); i >= 0 {
		exponent -= int64(len(mantissa) - i - 1)
		mantissa = mantissa[:i] + mantissa[i+1:]
	}
	digits := strings.TrimLeft(mantissa, "0")
	trimmed := strings.TrimRight(digits, "0")
	exponent += int64(len(digits) - len(trimmed))
	size := int64(len(trimmed)) + exponent
	if len(trimmed) == 0 || exponent < 0 || size < 13 || size > 19 {
		return ""
	}
	return trimmed + strings.Repeat("0", int(exponent))
}
