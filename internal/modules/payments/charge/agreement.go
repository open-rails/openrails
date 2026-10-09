package charge

import "errors"

// ErrAgreementRequired refuses a merchant-initiated charge on a card that
// carries no customer agreement for it: never anchored, or voided when the
// card was reissued under another brand (#1166). A customer-initiated charge
// anchors one; until then the customer is asked to act.
var ErrAgreementRequired = errors.New("the card carries no customer agreement for a merchant-initiated charge")

// AgreementRequiredCode is the failure code of an ErrAgreementRequired refusal.
const AgreementRequiredCode = "stored_credential_required"
