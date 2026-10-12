package service

import (
	"context"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// maxTrustLevelBytes bounds a stored trust level.
const maxTrustLevelBytes = 64

// UpdateCustomer changes one customer's settings and answers the customer.
// The change is validated whole before anything is written; a customer
// OpenRails has not seen is created.
func (s *Service) UpdateCustomer(ctx context.Context, customer identity.CustomerID, params billing.UpdateCustomerParams) (*billing.Customer, error) {
	if customer.IsZero() {
		return nil, apperr.Invalidf("customer_id is required").WithParam("customer_id")
	}
	change, err := validateCustomerSettings(customer.UUID(), params)
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
	if change.policy != nil {
		settings, err := merchantconfig.NewStore(s.rt.DB).Settings(ctx)
		if err != nil {
			return nil, err
		}
		if _, ok := settings.Policies[*change.policy]; !ok {
			return nil, ErrBillingPolicyNotFound.WithParam("billing_policy")
		}
	}
	err = s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		// A customer is the host's subject: one OpenRails has not billed yet
		// is created, as every commerce write does.
		if err := db.EnsureCustomerRowQ(ctx, q, mid.UUID(), change.customer); err != nil {
			return err
		}
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{ID: change.customer, MerchantID: mid.UUID()}); err != nil {
			return err
		}
		return change.apply(ctx, q, mid.UUID(), s.rt.Clock.Now().UTC())
	})
	if err != nil {
		return nil, err
	}
	row, err := s.rt.DB.Gen(ctx).GetCustomer(ctx, gen.GetCustomerParams{MerchantID: mid.UUID(), ID: change.customer})
	if err != nil {
		return nil, err
	}
	out, err := s.customers(ctx, mid, []gen.BillingCustomer{row})
	if err != nil {
		return nil, err
	}
	return &out[0], nil
}

// SetLowBalanceLine sets the customer's own low-balance line in a currency
// (null parts inherit the merchant's) and answers the effective line. The low
// notice is re-armed.
func (s *Service) SetLowBalanceLine(ctx context.Context, customer identity.CustomerID, currency string, params billing.SetLowBalanceLineParams) (*billing.EffectiveLowBalanceLine, error) {
	if customer.IsZero() {
		return nil, apperr.Invalidf("customer_id is required").WithParam("customer_id")
	}
	currency, err := settingsCurrency(currency, "currency", map[string]bool{})
	if err != nil {
		return nil, err
	}
	line := billing.LowBalanceLine{Currency: currency, Amount: params.Amount, RunwayHours: params.RunwayHours}
	if err := money.ValidateLowBalanceLine(line, ""); err != nil {
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
	err = s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		if err := db.EnsureCustomerRowQ(ctx, q, mid.UUID(), customer.UUID()); err != nil {
			return err
		}
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{ID: customer.UUID(), MerchantID: mid.UUID()}); err != nil {
			return err
		}
		return money.SetLowBalanceLineTx(ctx, q, mid.UUID(), customer.UUID(), line, s.rt.Clock.Now().UTC())
	})
	if err != nil {
		return nil, err
	}
	lines, err := s.moneyService().LowBalanceLines(ctx, customer, []string{currency})
	if err != nil {
		return nil, err
	}
	return &lines[0], nil
}

// customerSettingsChange is one validated, canonical change.
type customerSettingsChange struct {
	customer        uuid.UUID
	creditLimits    []billing.CreditLimit
	trustLevels     []billing.TrustLevel
	lowBalanceLines []billing.LowBalanceLine
	setPolicy       bool
	policy          *string
	setProfile      bool
	invoiceProfile  *money.CustomerInvoiceProfile
}

func validateCustomerSettings(customer uuid.UUID, params billing.UpdateCustomerParams) (customerSettingsChange, error) {
	c := customerSettingsChange{customer: customer}
	currencies := map[string]bool{}
	for j, limit := range params.CreditLimits {
		field := fmt.Sprintf("credit_limits[%d]", j)
		currency, err := settingsCurrency(limit.Currency, field+".currency", currencies)
		if err != nil {
			return c, err
		}
		if limit.Amount < 0 {
			return c, apperr.Invalidf("amount must be nonnegative").WithParam(field + ".amount")
		}
		c.creditLimits = append(c.creditLimits, billing.CreditLimit{Currency: currency, Amount: limit.Amount})
	}
	currencies = map[string]bool{}
	for j, level := range params.TrustLevels {
		field := fmt.Sprintf("trust_levels[%d]", j)
		currency, err := settingsCurrency(level.Currency, field+".currency", currencies)
		if err != nil {
			return c, err
		}
		name := strings.TrimSpace(level.TrustLevel)
		if len(name) > maxTrustLevelBytes {
			return c, apperr.Invalidf("trust_level exceeds %d bytes", maxTrustLevelBytes).WithParam(field + ".trust_level")
		}
		c.trustLevels = append(c.trustLevels, billing.TrustLevel{Currency: currency, TrustLevel: name})
	}
	currencies = map[string]bool{}
	for j, line := range params.LowBalanceLines {
		field := fmt.Sprintf("low_balance_lines[%d]", j)
		currency, err := settingsCurrency(line.Currency, field+".currency", currencies)
		if err != nil {
			return c, err
		}
		line.Currency = currency
		if err := money.ValidateLowBalanceLine(line, field+"."); err != nil {
			return c, err
		}
		c.lowBalanceLines = append(c.lowBalanceLines, line)
	}
	if params.BillingPolicy.Set {
		c.setPolicy = true
		if !params.BillingPolicy.Null {
			name, err := merchantconfig.NormalizeBillingPolicyName(params.BillingPolicy.Value)
			if err != nil {
				return c, apperr.Invalidf("%s", err).WithParam("billing_policy")
			}
			c.policy = &name
		}
	}
	if params.InvoiceProfile.Set {
		c.setProfile = true
		if !params.InvoiceProfile.Null {
			profile, err := validInvoiceProfile(params.InvoiceProfile.Value, "invoice_profile")
			if err != nil {
				return c, err
			}
			c.invoiceProfile = profile
		}
	}
	return c, nil
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
			if _, err := q.InsertMoneyAccountSettingsIfAbsent(ctx, gen.InsertMoneyAccountSettingsIfAbsentParams{
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
		// A credit line moves what an arrears customer may spend, and may
		// make it one: its watch follows, re-armed as by a funding.
		if err := q.WatchCustomerBalance(ctx, gen.WatchCustomerBalanceParams{
			MerchantID: mid, CustomerID: c.customer, Currency: limit.Currency, Funded: true, Now: now,
		}); err != nil {
			return err
		}
	}
	for _, line := range c.lowBalanceLines {
		if err := money.SetLowBalanceLineTx(ctx, q, mid, c.customer, line, now); err != nil {
			return err
		}
	}
	for _, level := range c.trustLevels {
		if level.TrustLevel != "" {
			if _, err := q.InsertMoneyAccountSettingsIfAbsent(ctx, gen.InsertMoneyAccountSettingsIfAbsentParams{
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
		if _, err := q.SetCustomerBillingPolicy(ctx, gen.SetCustomerBillingPolicyParams{MerchantID: mid, CustomerID: c.customer, Policy: c.policy}); err != nil {
			return err
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
		out[id] = billing.CustomerSettings{CreditLimits: []billing.CreditLimit{}, TrustLevels: []billing.TrustLevel{}, LowBalanceLines: []billing.LowBalanceLine{}}
	}
	if len(customers) == 0 {
		return out, nil
	}
	accounts, err := q.ListCustomerSettingsAccounts(ctx, gen.ListCustomerSettingsAccountsParams{MerchantID: mid, CustomerIds: customers, RowLimit: int32(len(customers) * len(billing.Currencies()))}) // #nosec G115 -- customers is at most 100 ids or one page (MaxPageLimit), times the currency registry
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
	lines, err := q.ListCustomerLowBalanceLines(ctx, gen.ListCustomerLowBalanceLinesParams{MerchantID: mid, CustomerIds: customers, RowLimit: int32(len(customers) * len(billing.Currencies()))}) // #nosec G115 -- customers is at most 100 ids or one page (MaxPageLimit), times the currency registry
	if err != nil {
		return nil, err
	}
	for _, l := range lines {
		doc := out[l.CustomerID]
		line := billing.LowBalanceLine{Currency: l.Currency, Amount: l.Amount}
		if l.RunwayHours != nil {
			hours := int(*l.RunwayHours)
			line.RunwayHours = &hours
		}
		doc.LowBalanceLines = append(doc.LowBalanceLines, line)
		out[l.CustomerID] = doc
	}
	policies, err := q.ListCustomerBillingPolicyAssignments(ctx, gen.ListCustomerBillingPolicyAssignmentsParams{MerchantID: mid, CustomerIds: customers, RowLimit: int32(len(customers))}) // #nosec G115 -- customers is at most 100 ids or one page (MaxPageLimit)
	if err != nil {
		return nil, err
	}
	for _, p := range policies {
		doc := out[p.CustomerID]
		name := p.PolicyName
		doc.BillingPolicy = &name
		out[p.CustomerID] = doc
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
