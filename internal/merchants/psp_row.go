package merchants

import (
	"encoding/json"
	"sort"
	"time"

	"github.com/open-rails/openrails/internal/db/gen"
)

// pspCredentialState is a PSP row's credential publication state.
type pspCredentialState struct {
	Custody             string
	Refs                map[string]SecretRef
	Versions            map[string]int
	Retired             map[string]bool
	ValidatedAt         *time.Time
	WebhookEndpointID   string
	WebhookOverlapUntil time.Time
	Revision            int64
}

func credentialState(row gen.BillingPsp) pspCredentialState {
	out := pspCredentialState{
		Refs:        map[string]SecretRef{},
		Versions:    map[string]int{},
		Retired:     map[string]bool{},
		ValidatedAt: row.CredentialsValidatedAt,
		Revision:    row.Revision,
	}
	if row.CredentialCustody != nil {
		out.Custody = *row.CredentialCustody
	}
	if row.WebhookEndpointID != nil {
		out.WebhookEndpointID = *row.WebhookEndpointID
	}
	if len(row.CredentialRefs) > 0 {
		_ = json.Unmarshal(row.CredentialRefs, &out.Refs)
	}
	if len(row.CredentialVersions) > 0 {
		_ = json.Unmarshal(row.CredentialVersions, &out.Versions)
	}
	for _, key := range row.RetiredCredentials {
		out.Retired[NormalizeCredentialVersionKey(key)] = true
	}
	out.WebhookOverlapUntil = webhookOverlapExpiry(row)
	return out
}

// rowSettings decodes the PSP's declared non-secret settings.
func rowSettings(row gen.BillingPsp) map[string]any {
	var out map[string]any
	if len(row.Settings) > 0 {
		_ = json.Unmarshal(row.Settings, &out)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func retiredList(retired map[string]bool) []string {
	out := make([]string, 0, len(retired))
	for key, isRetired := range retired {
		if isRetired {
			out = append(out, key)
		}
	}
	sort.Strings(out)
	return out
}
