package payment

import (
	"encoding/json"

	"github.com/open-rails/openrails/internal/db/gen"
)

// authenticationRequired reports a card payment waiting on the customer's
// provider challenge (3-D Secure); the browser completes it in the page.
func authenticationRequired(operation gen.BillingProviderIntent) bool {
	var evidence struct {
		AuthenticationRequired bool `json:"authentication_required"`
	}
	return json.Unmarshal(operation.ResultEvidence, &evidence) == nil && evidence.AuthenticationRequired
}
