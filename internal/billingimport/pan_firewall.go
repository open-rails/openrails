package billingimport

import (
	"fmt"

	"github.com/open-rails/openrails/internal/cardguard"
)

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
		return fmt.Errorf("import billing: %s contains a card-number-shaped value: raw PANs must never reach OpenRails (SAQ A) — declare the provider's vault handle, never the card", where)
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
			sub.Cancel.Kind, string(sub.Evidence),
		); err != nil {
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
