package billingauth

import (
	"errors"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// CredentialClass is provenance supplied by a trusted verifier. An automation
// credential can authorize ordinary self reads but does not itself establish
// customer interaction for customer-present payment commands.
type CredentialClass string

const (
	CredentialClassUnknown     CredentialClass = ""
	CredentialClassUserSession CredentialClass = "user_session"
	CredentialClassAutomation  CredentialClass = "automation"
)

// Payer is who accepted a payment: the customer route's verified customer,
// or the customer a merchant relays an acceptance for.
type Payer struct {
	// CredentialClass is CredentialClassUserSession only for the customer's
	// own interactive sign-in.
	CredentialClass CredentialClass
	MerchantID      billing.MerchantID
	// SubjectID is the paying customer's canonical UUID.
	SubjectID string
	// Issuer names who vouched for the acceptance, for audit.
	Issuer string
	// Invoker is set when an invoker spends SubjectID's balance without
	// being SubjectID; such a payer accepts nothing.
	Invoker       string
	Email         string
	EmailVerified bool
	Username      string
}

// ErrPayerInvalid is a payer without its merchant or customer.
var ErrPayerInvalid = errors.New("payer requires an explicit merchant and customer")

// ValidatePayer refuses a payer without a merchant or customer.
func ValidatePayer(p *Payer) error {
	if p == nil || p.MerchantID.IsZero() || strings.TrimSpace(p.SubjectID) == "" || (p.CredentialClass != CredentialClassUnknown && p.CredentialClass != CredentialClassUserSession && p.CredentialClass != CredentialClassAutomation) {
		return ErrPayerInvalid
	}
	return nil
}
