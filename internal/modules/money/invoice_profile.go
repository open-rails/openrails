package money

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	safecast "github.com/ccoveille/go-safecast/v2"
	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
)

// MaxInvoiceNetTermsDays prevents overflow when terms become a due-date duration.
const MaxInvoiceNetTermsDays = int64((1<<63 - 1) / (24 * time.Hour))

// Invoice collection methods. charge_automatically charges the saved payment
// method via ChargeOutstanding; send_invoice is a manual-remittance receivable
// the collection path never touches, paid via RecordInvoiceRemittance.
const (
	CollectionChargeAutomatically = "charge_automatically"
	CollectionSendInvoice         = "send_invoice"
)

// CustomerInvoiceProfile is a payer's enterprise invoicing profile: net-N
// terms, collection method and the document fields snapshotted onto every
// invoice at finalize. Absent = zero value (due immediately,
// charge_automatically, no document fields).
type CustomerInvoiceProfile struct {
	NetTermsDays     int                     `json:"net_terms_days"`
	CollectionMethod string                  `json:"collection_method"`
	PONumber         string                  `json:"po_number,omitempty"`
	Tax              map[string]any          `json:"tax,omitempty"`
	BillingContacts  []models.InvoiceContact `json:"billing_contacts,omitempty"`
	Memo             string                  `json:"memo,omitempty"`
}

func normalizeCollectionMethod(s string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", CollectionChargeAutomatically:
		return CollectionChargeAutomatically, nil
	case CollectionSendInvoice:
		return CollectionSendInvoice, nil
	default:
		return "", fmt.Errorf("collection_method must be %s or %s, got %q",
			CollectionChargeAutomatically, CollectionSendInvoice, s)
	}
}

// PutInvoiceProfileTx replaces a customer's invoice profile, or with nil
// removes it, inside the caller's merchant transaction. The caller validated
// p and holds the customer's lock.
func PutInvoiceProfileTx(ctx context.Context, q *gen.Queries, merchantID, customerID uuid.UUID, p *CustomerInvoiceProfile, now time.Time) error {
	if p == nil {
		return q.DeleteCustomerInvoiceProfile(ctx, gen.DeleteCustomerInvoiceProfileParams{MerchantID: merchantID, CustomerID: customerID})
	}
	netTerms, err := safecast.Convert[int32](p.NetTermsDays)
	if err != nil || netTerms < 0 || int64(netTerms) > MaxInvoiceNetTermsDays {
		return fmt.Errorf("net_terms_days must be between 0 and %d", MaxInvoiceNetTermsDays)
	}
	method, err := normalizeCollectionMethod(p.CollectionMethod)
	if err != nil {
		return err
	}
	taxJSON, err := toJSONBC(p.Tax)
	if err != nil {
		return fmt.Errorf("encode invoice profile tax: %w", err)
	}
	contactsJSON := []byte("[]")
	if p.BillingContacts != nil {
		if contactsJSON, err = json.Marshal(p.BillingContacts); err != nil {
			return fmt.Errorf("encode invoice profile billing_contacts: %w", err)
		}
	}
	return q.UpsertCustomerInvoiceProfile(ctx, gen.UpsertCustomerInvoiceProfileParams{
		MerchantID: merchantID, CustomerID: customerID, NetTermsDays: netTerms, CollectionMethod: method,
		PoNumber: nilIfEmpty(p.PONumber), Tax: taxJSON, BillingContacts: contactsJSON, Memo: nilIfEmpty(p.Memo), Now: now,
	})
}

// InvoiceProfilesTx reads the customers' invoice profiles; one with none is
// absent.
func InvoiceProfilesTx(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, customers []uuid.UUID) (map[uuid.UUID]*CustomerInvoiceProfile, error) {
	rows, err := q.ListInvoiceProfiles(ctx, gen.ListInvoiceProfilesParams{MerchantID: merchantID, CustomerIds: customers})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]*CustomerInvoiceProfile, len(rows))
	for _, row := range rows {
		p, err := invoiceProfileFromGen(row)
		if err != nil {
			return nil, err
		}
		out[row.CustomerID] = p
	}
	return out, nil
}

func invoiceProfileFromGen(row gen.BillingCustomerInvoiceProfile) (*CustomerInvoiceProfile, error) {
	p := &CustomerInvoiceProfile{
		NetTermsDays:     int(row.NetTermsDays),
		CollectionMethod: row.CollectionMethod,
		PONumber:         derefStr(row.PoNumber),
		Memo:             derefStr(row.Memo),
	}
	if err := fromJSONBC(row.Tax, &p.Tax, "customer_invoice_profiles.tax"); err != nil {
		return nil, err
	}
	if len(row.BillingContacts) > 0 {
		if err := json.Unmarshal(row.BillingContacts, &p.BillingContacts); err != nil {
			return nil, fmt.Errorf("money: decode customer_invoice_profiles.billing_contacts: %w", err)
		}
	}
	return p, nil
}
