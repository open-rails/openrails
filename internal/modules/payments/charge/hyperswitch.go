package charge

import (
	"errors"
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
