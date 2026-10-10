package charge

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/merchants"
)

// HyperSwitchBinding freezes the custody policy of one accepted operation.
// Credentials may rotate; account, profile and trusted deployment may not be
// silently replaced while recovering that operation.
type HyperSwitchBinding struct {
	AccountID  string `json:"account_id"`
	ProfileID  string `json:"profile_id"`
	APIBaseURL string `json:"api_base_url"`
}

func CanonicalHyperSwitchDeployment(raw string) (string, error) {
	canonical := strings.TrimRight(raw, "/")
	u, err := url.Parse(canonical)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") || strings.TrimSpace(raw) != raw {
		return "", errors.New("HyperSwitch deployment binding is invalid")
	}
	return canonical, nil
}

func (b HyperSwitchBinding) Validate() error {
	if b.AccountID == "" || b.ProfileID == "" || len(b.AccountID) > 256 || len(b.ProfileID) > 256 || strings.TrimSpace(b.AccountID) != b.AccountID || strings.TrimSpace(b.ProfileID) != b.ProfileID {
		return errors.New("HyperSwitch account/profile binding is invalid")
	}
	canonical, err := CanonicalHyperSwitchDeployment(b.APIBaseURL)
	if err != nil || canonical != b.APIBaseURL {
		return errors.New("HyperSwitch deployment binding is not canonical")
	}
	return nil
}

// Custody is the merchant configuration a charge reads: its custodians and
// the PSPs that reach them.
type Custody interface {
	CustodianScopeByID(ctx context.Context, id billing.MerchantID, custodianID uuid.UUID) (merchants.CustodianScope, bool, error)
	CustodianRoutePSPs(ctx context.Context, id billing.MerchantID, rail string, custodianID uuid.UUID) ([]uuid.UUID, error)
}

// FreezeHyperSwitchBinding reads the PSP and custodian identities under
// admission locks for invoices, recurring obligations and initial
// memberships, and the custodian's settings from its configuration; psp is the
// PSP the charge goes through.
func FreezeHyperSwitchBinding(ctx context.Context, q *gen.Queries, custody Custody, method gen.BillingPaymentMethod, psp uuid.UUID, deployment string) (HyperSwitchBinding, error) {
	if deployment == "" || method.Custodian != models.CustodianHyperSwitch || method.CustodianID == nil || custody == nil {
		return HyperSwitchBinding{}, fmt.Errorf("%w: HyperSwitch custody is not configured", ErrInstrumentChanged)
	}
	accounts, err := q.GetCollectionCustodianAccountsForShare(ctx, gen.GetCollectionCustodianAccountsForShareParams{MerchantID: method.MerchantID, PspID: psp, CustodianID: *method.CustodianID})
	if errors.Is(err, pgx.ErrNoRows) {
		return HyperSwitchBinding{}, ErrInstrumentChanged
	}
	if err != nil {
		return HyperSwitchBinding{}, err
	}
	row := accounts.BillingCustodian
	if accounts.BillingPsp.Rail != method.Rail || row.Kind != method.Custodian || row.Environment != accounts.BillingPsp.Environment {
		return HyperSwitchBinding{}, ErrInstrumentChanged
	}
	scope, ok, err := custody.CustodianScopeByID(ctx, billing.MerchantID(method.MerchantID), row.ID)
	if err != nil {
		return HyperSwitchBinding{}, err
	}
	if !ok {
		return HyperSwitchBinding{}, ErrInstrumentChanged
	}
	return HyperSwitchBindingFromAccount(scope, deployment)
}

// HyperSwitchBindingFromAccount is the binding a HyperSwitch custodian's
// configuration names.
func HyperSwitchBindingFromAccount(custodian merchants.CustodianScope, deployment string) (HyperSwitchBinding, error) {
	parsed, err := custodians.ParseSettings(custodian.Kind, custodian.Settings)
	if err != nil {
		return HyperSwitchBinding{}, ErrInstrumentChanged
	}
	canonical, err := CanonicalHyperSwitchDeployment(deployment)
	if err != nil {
		return HyperSwitchBinding{}, err
	}
	binding := HyperSwitchBinding{AccountID: custodian.AccountID, ProfileID: parsed.ProfileID, APIBaseURL: canonical}
	return binding, binding.Validate()
}

// CustodyOf is the custody configuration bound to d (db.MerchantConfig); nil
// when none is.
func CustodyOf(d *db.DB) Custody {
	custody, _ := d.MerchantConfig().(Custody)
	return custody
}
