package charge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"net/url"
	"strings"
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

// FreezeHyperSwitchBinding reads the same account/custodian rows under admission
// locks for invoices, recurring obligations and initial memberships.
func FreezeHyperSwitchBinding(ctx context.Context, q *gen.Queries, method gen.OpenrailsPaymentMethod, deployment string) (HyperSwitchBinding, error) {
	if deployment == "" || method.Custodian != models.CustodianHyperSwitch || method.CustodianID == nil {
		return HyperSwitchBinding{}, fmt.Errorf("%w: HyperSwitch custody is not configured", ErrInstrumentChanged)
	}
	accounts, err := q.GetCollectionCustodianAccountsForShare(ctx, gen.GetCollectionCustodianAccountsForShareParams{MerchantID: method.MerchantID, PspID: method.PspID, CustodianID: *method.CustodianID})
	if errors.Is(err, pgx.ErrNoRows) {
		return HyperSwitchBinding{}, ErrInstrumentChanged
	}
	if err != nil {
		return HyperSwitchBinding{}, err
	}
	row := accounts.OpenrailsCustodian
	if accounts.OpenrailsPsp.Rail != method.Rail || row.Kind != method.Custodian || row.Environment != accounts.OpenrailsPsp.Environment {
		return HyperSwitchBinding{}, ErrInstrumentChanged
	}
	return HyperSwitchBindingFromAccount(row, deployment)
}

func HyperSwitchBindingFromAccount(row gen.OpenrailsCustodian, deployment string) (HyperSwitchBinding, error) {
	var settings map[string]any
	if json.Unmarshal(row.Settings, &settings) != nil {
		return HyperSwitchBinding{}, ErrInstrumentChanged
	}
	parsed, err := custodians.ParseSettings(row.Kind, settings)
	if err != nil {
		return HyperSwitchBinding{}, ErrInstrumentChanged
	}
	canonical, err := CanonicalHyperSwitchDeployment(deployment)
	if err != nil {
		return HyperSwitchBinding{}, err
	}
	binding := HyperSwitchBinding{AccountID: row.AccountID, ProfileID: parsed.ProfileID, APIBaseURL: canonical}
	return binding, binding.Validate()
}
