package service

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/pagination"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
)

// maxTrustLevelBytes bounds a stored trust level.
const maxTrustLevelBytes = 64

// ListCustomerSettings lists customers' settings, newest customer first. IDs
// instead reads the named customers in one page; unknown ones are absent.
func (s *Service) ListCustomerSettings(ctx context.Context, params billing.CustomerSettingsListParams) (billing.ListPage[billing.CustomerSettings], error) {
	var page billing.ListPage[billing.CustomerSettings]
	if params.IDs != nil && (len(params.IDs) == 0 || len(params.IDs) > billing.MaxBatchItems) {
		return page, apperr.Invalidf("ids must hold 1 to %d customers", billing.MaxBatchItems).WithParam("ids")
	}
	limit, err := pagination.Limit(params.PageRequest)
	if err != nil {
		return page, err
	}
	afterAt, afterID, err := pagination.After(params.Cursor)
	if err != nil {
		return page, err
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return page, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return page, err
	}
	q := s.rt.DB.Gen(ctx)
	var customers billing.ListPage[gen.BillingCustomer]
	if params.IDs != nil {
		ids := make([]uuid.UUID, len(params.IDs))
		for i, id := range params.IDs {
			ids[i] = id.UUID()
		}
		if customers.Items, err = q.GetCustomersByIDs(ctx, gen.GetCustomersByIDsParams{MerchantID: mid.UUID(), Ids: ids}); err != nil {
			return page, err
		}
		slices.SortFunc(customers.Items, func(a, b gen.BillingCustomer) int {
			if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
				return c
			}
			return strings.Compare(b.ID.String(), a.ID.String())
		})
	} else {
		rows, err := q.ListCustomers(ctx, gen.ListCustomersParams{MerchantID: mid.UUID(), AfterAt: afterAt, AfterID: afterID, RowLimit: pagination.Fetch(limit)})
		if err != nil {
			return page, err
		}
		customers = pagination.Cut(rows, limit, func(c gen.BillingCustomer) any {
			return pagination.TimeID{At: c.CreatedAt, ID: c.ID}
		})
	}
	keys := make([]uuid.UUID, len(customers.Items))
	for i, c := range customers.Items {
		keys[i] = c.ID
	}
	docs, err := customerSettings(ctx, q, mid.UUID(), keys)
	if err != nil {
		return page, err
	}
	page.Items = make([]billing.CustomerSettings, len(keys))
	for i, id := range keys {
		page.Items[i] = docs[id]
	}
	page.Next = customers.Next
	return page, nil
}

// UpdateCustomerSettings changes 1 to billing.MaxBatchItems distinct
// customers' settings, all or none, and answers their settings in request
// order. The whole batch is validated before anything is written; settings
// never create a customer.
func (s *Service) UpdateCustomerSettings(ctx context.Context, items []billing.UpdateCustomerSettingsParams) ([]billing.CustomerSettings, error) {
	changes, err := validateCustomerSettings(items)
	if err != nil {
		return nil, err
	}
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	keys := make([]uuid.UUID, len(changes))
	for i, c := range changes {
		keys[i] = c.customer
	}
	var docs map[uuid.UUID]billing.CustomerSettings
	err = s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		now := s.rt.Clock.Now().UTC()
		// Share the policy declarations' lock before reading them, then lock
		// the customers in one order so concurrent batches never deadlock.
		if _, err := q.ReadMerchantSettingsLock(ctx, mid.UUID()); err != nil {
			return err
		}
		order := slices.Clone(changes)
		slices.SortFunc(order, func(a, b customerSettingsChange) int {
			return strings.Compare(a.customer.String(), b.customer.String())
		})
		for _, c := range order {
			if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{ID: c.customer, MerchantID: mid.UUID()}); errors.Is(err, pgx.ErrNoRows) {
				return ErrCustomerNotFound.WithParam(apperr.ItemParam(c.item, "customer_id"))
			} else if err != nil {
				return err
			}
		}
		for _, c := range changes {
			if err := c.apply(ctx, q, mid.UUID(), now); err != nil {
				return err
			}
		}
		docs, err = customerSettings(ctx, q, mid.UUID(), keys)
		return err
	})
	if err != nil {
		return nil, err
	}
	out := make([]billing.CustomerSettings, len(keys))
	for i, id := range keys {
		out[i] = docs[id]
	}
	return out, nil
}

// customerSettingsChange is one validated, canonical item.
type customerSettingsChange struct {
	item           int
	customer       uuid.UUID
	creditLimits   []billing.CreditLimit
	trustLevels    []billing.TrustLevel
	setPolicy      bool
	policy         *string
	setProfile     bool
	invoiceProfile *money.CustomerInvoiceProfile
}

func validateCustomerSettings(items []billing.UpdateCustomerSettingsParams) ([]customerSettingsChange, error) {
	if len(items) == 0 || len(items) > billing.MaxBatchItems {
		return nil, apperr.Invalidf("items must hold 1 to %d customers", billing.MaxBatchItems).WithParam("items")
	}
	out := make([]customerSettingsChange, len(items))
	seen := make(map[billing.CustomerID]bool, len(items))
	for i, item := range items {
		param := func(field string) string { return apperr.ItemParam(i, field) }
		if item.CustomerID.IsZero() {
			return nil, apperr.Invalidf("customer_id is required").WithParam(param("customer_id"))
		}
		if seen[item.CustomerID] {
			return nil, apperr.Invalidf("customer %s is named twice", item.CustomerID).WithParam(param("customer_id"))
		}
		seen[item.CustomerID] = true
		c := customerSettingsChange{item: i, customer: item.CustomerID.UUID()}

		currencies := map[string]bool{}
		for j, limit := range item.CreditLimits {
			field := fmt.Sprintf("credit_limits[%d]", j)
			currency, err := settingsCurrency(limit.Currency, param(field+".currency"), currencies)
			if err != nil {
				return nil, err
			}
			if limit.Amount < 0 {
				return nil, apperr.Invalidf("amount must be nonnegative").WithParam(param(field + ".amount"))
			}
			c.creditLimits = append(c.creditLimits, billing.CreditLimit{Currency: currency, Amount: limit.Amount})
		}
		currencies = map[string]bool{}
		for j, level := range item.TrustLevels {
			field := fmt.Sprintf("trust_levels[%d]", j)
			currency, err := settingsCurrency(level.Currency, param(field+".currency"), currencies)
			if err != nil {
				return nil, err
			}
			name := strings.TrimSpace(level.TrustLevel)
			if len(name) > maxTrustLevelBytes {
				return nil, apperr.Invalidf("trust_level exceeds %d bytes", maxTrustLevelBytes).WithParam(param(field + ".trust_level"))
			}
			c.trustLevels = append(c.trustLevels, billing.TrustLevel{Currency: currency, TrustLevel: name})
		}

		if item.BillingPolicy.Set {
			c.setPolicy = true
			if !item.BillingPolicy.Null {
				name, err := merchantconfig.NormalizeBillingPolicyName(item.BillingPolicy.Value)
				if err != nil {
					return nil, apperr.Invalidf("%s", err).WithParam(param("billing_policy"))
				}
				c.policy = &name
			}
		}
		if item.InvoiceProfile.Set {
			c.setProfile = true
			if !item.InvoiceProfile.Null {
				profile, err := validInvoiceProfile(item.InvoiceProfile.Value, param("invoice_profile"))
				if err != nil {
					return nil, err
				}
				c.invoiceProfile = profile
			}
		}
		out[i] = c
	}
	return out, nil
}

func settingsCurrency(code, param string, seen map[string]bool) (string, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return "", apperr.Invalidf("currency is required").WithParam(param)
	}
	normalized := moneyutil.NormalizeCurrency(code)
	if err := moneyutil.ValidateCurrency(normalized); err != nil {
		return "", fmt.Errorf("%w: %q", ErrCurrencyUnsupported.WithParam(param), normalized)
	}
	if seen[normalized] {
		return "", apperr.Invalidf("currency %s is named twice", normalized).WithParam(param)
	}
	seen[normalized] = true
	return normalized, nil
}

func validInvoiceProfile(p billing.InvoiceProfile, param string) (*money.CustomerInvoiceProfile, error) {
	if p.NetTermsDays < 0 || int64(p.NetTermsDays) > money.MaxInvoiceNetTermsDays {
		return nil, apperr.Invalidf("net_terms_days is out of range").WithParam(param + ".net_terms_days")
	}
	if p.CollectionMethod != billing.CollectChargeAutomatically && p.CollectionMethod != billing.CollectSendInvoice {
		return nil, apperr.Invalidf("collection_method is invalid").WithParam(param + ".collection_method")
	}
	contacts := make([]models.InvoiceContact, 0, len(p.BillingContacts))
	for _, contact := range p.BillingContacts {
		email := strings.TrimSpace(contact.Email)
		address, err := mail.ParseAddress(email)
		if err != nil || address.Address != email {
			return nil, apperr.Invalidf("billing contacts need valid email addresses").WithParam(param + ".billing_contacts")
		}
		contacts = append(contacts, models.InvoiceContact{Name: contact.Name, Email: email})
	}
	return &money.CustomerInvoiceProfile{
		NetTermsDays: p.NetTermsDays, CollectionMethod: string(p.CollectionMethod), PONumber: p.PONumber,
		Tax: p.Tax, BillingContacts: contacts, Memo: p.Memo,
	}, nil
}

// apply writes one customer's change inside the batch's transaction. A
// cleared credit limit or trust level on a currency with no settings row
// writes nothing: the row's absence already means none.
func (c customerSettingsChange) apply(ctx context.Context, q *gen.Queries, mid uuid.UUID, now time.Time) error {
	for _, limit := range c.creditLimits {
		if limit.Amount > 0 {
			if err := q.InsertMoneyAccountSettingsIfAbsent(ctx, gen.InsertMoneyAccountSettingsIfAbsentParams{
				MerchantID: mid, CustomerID: c.customer, Currency: limit.Currency, BillingMode: money.BillingModeArrears, Now: now,
			}); err != nil {
				return err
			}
		}
		if err := q.SetMoneyAccountCreditLimit(ctx, gen.SetMoneyAccountCreditLimitParams{
			MerchantID: mid, CustomerID: c.customer, Currency: limit.Currency, CreditLimit: limit.Amount, Now: now,
		}); err != nil {
			return err
		}
	}
	for _, level := range c.trustLevels {
		if level.TrustLevel != "" {
			if err := q.InsertMoneyAccountSettingsIfAbsent(ctx, gen.InsertMoneyAccountSettingsIfAbsentParams{
				MerchantID: mid, CustomerID: c.customer, Currency: level.Currency, BillingMode: money.BillingModePrepaid, Now: now,
			}); err != nil {
				return err
			}
		}
		if err := q.SetMoneyAccountTier(ctx, gen.SetMoneyAccountTierParams{
			MerchantID: mid, CustomerID: c.customer, Currency: level.Currency, Tier: level.TrustLevel, Now: now,
		}); err != nil {
			return err
		}
	}
	if c.setPolicy {
		id := c.customer
		if c.policy == nil {
			if err := q.DeleteCustomerBillingPolicyBinding(ctx, gen.DeleteCustomerBillingPolicyBindingParams{MerchantID: mid, CustomerID: &id}); err != nil {
				return err
			}
		} else {
			if _, err := q.LockBillingPolicyName(ctx, gen.LockBillingPolicyNameParams{MerchantID: mid, Name: *c.policy}); errors.Is(err, pgx.ErrNoRows) {
				return ErrBillingPolicyNotFound.WithParam(apperr.ItemParam(c.item, "billing_policy"))
			} else if err != nil {
				return err
			}
			if err := q.UpsertBillingPolicyBindingCustomer(ctx, gen.UpsertBillingPolicyBindingCustomerParams{
				ID: uuidutil.NewV7(), MerchantID: mid, CustomerID: &id, PolicyName: *c.policy, CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				return err
			}
		}
	}
	if c.setProfile {
		if err := money.PutInvoiceProfileTx(ctx, q, mid, c.customer, c.invoiceProfile, now); err != nil {
			return err
		}
	}
	return nil
}

// customerSettings reads the customers' settings documents.
func customerSettings(ctx context.Context, q *gen.Queries, mid uuid.UUID, customers []uuid.UUID) (map[uuid.UUID]billing.CustomerSettings, error) {
	out := make(map[uuid.UUID]billing.CustomerSettings, len(customers))
	for _, id := range customers {
		out[id] = billing.CustomerSettings{CustomerID: billing.CustomerID(id), CreditLimits: []billing.CreditLimit{}, TrustLevels: []billing.TrustLevel{}}
	}
	if len(customers) == 0 {
		return out, nil
	}
	accounts, err := q.ListCustomerSettingsAccounts(ctx, gen.ListCustomerSettingsAccountsParams{MerchantID: mid, CustomerIds: customers, RowLimit: int32(len(customers) * len(billing.Currencies()))})
	if err != nil {
		return nil, err
	}
	for _, a := range accounts {
		doc := out[a.CustomerID]
		if a.CreditLimitAmount != 0 {
			doc.CreditLimits = append(doc.CreditLimits, billing.CreditLimit{Currency: a.Currency, Amount: a.CreditLimitAmount})
		}
		if a.Tier != nil && *a.Tier != "" {
			doc.TrustLevels = append(doc.TrustLevels, billing.TrustLevel{Currency: a.Currency, TrustLevel: *a.Tier})
		}
		out[a.CustomerID] = doc
	}
	policies, err := q.ListCustomerBillingPolicyAssignments(ctx, gen.ListCustomerBillingPolicyAssignmentsParams{MerchantID: mid, CustomerIds: customers, RowLimit: int32(len(customers))})
	if err != nil {
		return nil, err
	}
	for _, p := range policies {
		if p.CustomerID == nil {
			continue
		}
		doc := out[*p.CustomerID]
		name := p.PolicyName
		doc.BillingPolicy = &name
		out[*p.CustomerID] = doc
	}
	profiles, err := money.InvoiceProfilesTx(ctx, q, mid, customers)
	if err != nil {
		return nil, err
	}
	for id, p := range profiles {
		doc := out[id]
		doc.InvoiceProfile = invoiceProfileView(p)
		out[id] = doc
	}
	return out, nil
}

func invoiceProfileView(p *money.CustomerInvoiceProfile) *billing.InvoiceProfile {
	contacts := make([]billing.InvoiceContact, 0, len(p.BillingContacts))
	for _, c := range p.BillingContacts {
		contacts = append(contacts, billing.InvoiceContact{Name: c.Name, Email: c.Email})
	}
	return &billing.InvoiceProfile{
		NetTermsDays: p.NetTermsDays, CollectionMethod: billing.InvoiceCollectionMethod(p.CollectionMethod), PONumber: p.PONumber,
		Tax: p.Tax, BillingContacts: contacts, Memo: p.Memo,
	}
}
