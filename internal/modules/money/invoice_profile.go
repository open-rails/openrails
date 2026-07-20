package money

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	safecast "github.com/ccoveille/go-safecast/v2"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/pkg/identity"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Invoice collection methods (#798). charge_automatically charges the saved
// payment method via ChargeOutstanding; send_invoice is a manual-remittance
// terms receivable the collection path never touches — payment arrives via
// RecordOutOfBandInvoicePayment.
const (
	CollectionChargeAutomatically = "charge_automatically"
	CollectionSendInvoice         = "send_invoice"
)

// CustomerInvoiceProfile is a payer's enterprise invoicing profile (#798):
// net-N credit terms, collection method and the document fields snapshotted
// onto every invoice at finalize. Absent profile = zero value (due
// immediately, charge_automatically, no document fields).
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

// SetCustomerInvoiceProfile upserts a payer's invoice profile. Operator
// surface — a payer must not grant itself credit terms.
func (s *MoneyService) SetCustomerInvoiceProfile(ctx context.Context, payer identity.CustomerID, p CustomerInvoiceProfile) error {
	_, err := s.writeCustomerInvoiceProfile(ctx, payer, p, false)
	return err
}

// EnsureCustomerInvoiceProfile inserts a payer's invoice profile when none is
// stored. It never overwrites an operator-configured profile.
func (s *MoneyService) EnsureCustomerInvoiceProfile(ctx context.Context, payer identity.CustomerID, p CustomerInvoiceProfile) (bool, error) {
	return s.writeCustomerInvoiceProfile(ctx, payer, p, true)
}

func (s *MoneyService) writeCustomerInvoiceProfile(ctx context.Context, payer identity.CustomerID, p CustomerInvoiceProfile, insertOnly bool) (bool, error) {
	if s == nil || s.db == nil {
		return false, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return false, fmt.Errorf("payer required")
	}
	netTerms, castErr := safecast.Convert[int32](p.NetTermsDays)
	if castErr != nil || netTerms < 0 {
		return false, fmt.Errorf("net_terms_days must be a non-negative int32")
	}
	method, err := normalizeCollectionMethod(p.CollectionMethod)
	if err != nil {
		return false, err
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return false, err
	}
	taxJSON, err := toJSONBC(p.Tax)
	if err != nil {
		return false, fmt.Errorf("encode invoice profile tax: %w", err)
	}
	contactsJSON, err := json.Marshal(p.BillingContacts)
	if err != nil {
		return false, fmt.Errorf("encode invoice profile billing_contacts: %w", err)
	}
	if p.BillingContacts == nil {
		contactsJSON = []byte("[]")
	}
	created := false
	err = s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := ensureCustomer(ctx, q, tid.UUID(), payer.UUID()); err != nil {
			return err
		}
		_, err := q.LockCustomerForMerchant(ctx, gen.LockCustomerForMerchantParams{
			ID: payer.UUID(), MerchantID: tid.UUID(),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("invoice profile payer belongs to another merchant")
			}
			return fmt.Errorf("lock invoice profile payer: %w", err)
		}
		if insertOnly {
			rows, err := q.InsertCustomerInvoiceProfileIfAbsent(ctx, gen.InsertCustomerInvoiceProfileIfAbsentParams{
				MerchantID:       tid.UUID(),
				CustomerID:       payer.UUID(),
				NetTermsDays:     netTerms,
				CollectionMethod: method,
				PoNumber:         nilIfEmpty(p.PONumber),
				Tax:              taxJSON,
				BillingContacts:  contactsJSON,
				Memo:             nilIfEmpty(p.Memo),
				Now:              s.now(),
			})
			if err != nil {
				return err
			}
			created = rows == 1
			return nil
		}
		return q.UpsertCustomerInvoiceProfile(ctx, gen.UpsertCustomerInvoiceProfileParams{
			MerchantID:       tid.UUID(),
			CustomerID:       payer.UUID(),
			NetTermsDays:     netTerms,
			CollectionMethod: method,
			PoNumber:         nilIfEmpty(p.PONumber),
			Tax:              taxJSON,
			BillingContacts:  contactsJSON,
			Memo:             nilIfEmpty(p.Memo),
			Now:              s.now(),
		})
	})
	if err != nil {
		return false, err
	}
	return created, nil
}

// GetCustomerInvoiceProfile returns the payer's invoice profile, or nil when
// none is stored.
func (s *MoneyService) GetCustomerInvoiceProfile(ctx context.Context, payer identity.CustomerID) (*CustomerInvoiceProfile, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("money service not initialized")
	}
	if payer.IsZero() {
		return nil, fmt.Errorf("payer required")
	}
	tid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	var out *CustomerInvoiceProfile
	err = s.db.RunInMerchantConn(ctx, func(ctx context.Context) error {
		row, err := s.db.Gen(ctx).GetCustomerInvoiceProfile(ctx, gen.GetCustomerInvoiceProfileParams{
			MerchantID: tid.UUID(), CustomerID: payer.UUID(),
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return err
		}
		p, err := invoiceProfileFromGen(row)
		if err != nil {
			return err
		}
		out = p
		return nil
	})
	return out, err
}

func invoiceProfileFromGen(row gen.OpenrailsCustomerInvoiceProfile) (*CustomerInvoiceProfile, error) {
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
