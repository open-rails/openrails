package merchantdocs

import (
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// Declared is the configuration a file declares for merchant id. Each PSP and
// custodian is in its declared environment.
// loaded is when the file was read.
func Declared(id billing.MerchantID, m config.MerchantDeclaration, loaded time.Time) Set {
	set := Set{}.Clone()
	merchant := Merchant{
		DisplayName:      strings.TrimSpace(m.DisplayName),
		MerchantSettings: m.MerchantSettings,
		Secrets:          MerchantSecrets{SCIMToken: strings.TrimSpace(m.Secrets.SCIMToken)},
	}
	for i, w := range m.AlertWebhooks {
		format := billing.AlertWebhookFormat(strings.ToLower(strings.TrimSpace(w.Format)))
		if format == "" {
			format = billing.AlertWebhookGeneric
		}
		enabled := w.Enabled == nil || *w.Enabled
		merchant.AlertWebhooks = append(merchant.AlertWebhooks, AlertWebhook{
			ID:   billing.AlertWebhookID(uuid.NewSHA1(id.UUID(), []byte("alert_webhook/"+strconv.Itoa(i)))),
			Name: strings.TrimSpace(w.Name), URL: strings.TrimSpace(w.URL), Format: format, Enabled: enabled,
			CreatedAt: loaded, UpdatedAt: loaded,
		})
	}
	set.Merchant, set.HasMerchant = Doc[Merchant]{Value: merchant, UpdatedAt: loaded}, true
	for key, p := range m.PSPs {
		key = strings.ToLower(strings.TrimSpace(key))
		account := strings.TrimSpace(p.AccountID)
		rail := strings.ToLower(strings.TrimSpace(string(p.Rail)))
		doc := PSP{
			Rail: rail, Environment: DefaultEnvironment(rail, p.Environment), AccountID: account,
			Archived: p.Archived, Custodian: strings.ToLower(strings.TrimSpace(p.Custodian)),
			Settings: maps.Clone(p.Settings), Secrets: normalizedSecrets(p.Secrets),
		}
		set.PSPs[key] = Doc[PSP]{Value: doc, UpdatedAt: loaded}
	}
	for key, c := range m.Custodians {
		key = strings.ToLower(strings.TrimSpace(key))
		set.Custodians[key] = Doc[Custodian]{Value: Custodian{
			Kind: strings.ToLower(strings.TrimSpace(c.Kind)), Environment: DeclaredCustodianEnvironment(c.Environment), AccountID: strings.TrimSpace(c.AccountID),
			Archived: c.Archived, Settings: maps.Clone(c.Settings), Secrets: normalizedSecrets(c.Secrets),
		}, UpdatedAt: loaded}
	}
	return set
}

// DeclaredCustodianEnvironment is a declared custodian's money class: live
// when omitted.
func DeclaredCustodianEnvironment(environment string) string {
	if environment = strings.ToLower(strings.TrimSpace(environment)); environment != "" {
		return environment
	}
	return config.ClassLive
}

// DefaultEnvironment is a PSP document's environment: the rail's real-money
// one when the document names none.
func DefaultEnvironment(rail, environment string) string {
	if environment = strings.TrimSpace(environment); environment != "" {
		return environment
	}
	return rails.DefaultEnvironment(models.Rail(strings.ToLower(strings.TrimSpace(rail))))
}

func normalizedSecrets(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return out
}
