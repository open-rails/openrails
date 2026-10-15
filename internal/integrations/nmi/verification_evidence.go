package nmi

import (
	"context"
	"errors"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// VerificationEvidence is a completed zero-value validation. A zero response
// code means the report did not establish an outcome; it never permits a resend.
type VerificationEvidence struct {
	TransactionID string `json:"transaction_id"`
	ResponseCode  int    `json:"response_code"`
}

// ReadVerificationEvidence matches the persisted order on this exact account.
// NMI may omit the vault reference, so matching relies on the uniquely accepted
// outbound order, not masked card digits or an inferred agreement type.
func (c *NMIClient) ReadVerificationEvidence(ctx context.Context, order, vault string) (VerificationEvidence, error) {
	report, err := c.TransactionReport(ctx, QueryFilter{OrderID: order, ActionType: "validate", ResultLimit: 2})
	if err != nil {
		return VerificationEvidence{}, err
	}
	return qualifyVerification(report, order, vault)
}

func qualifyVerification(report TransactionReport, order, vault string) (VerificationEvidence, error) {
	var result VerificationEvidence
	if len(report.Transactions) == 0 {
		return result, nil
	}
	if order == "" || len(report.Transactions) != 1 {
		return result, errors.New("verification lookup is not unique")
	}
	t := report.Transactions[0]
	if t.OrderID != order || strings.TrimSpace(t.TransactionID) == "" || len(t.Actions) != 1 || (t.Vault() != "" && t.Vault() != vault) {
		return result, errors.New("verification lookup does not match accepted request")
	}
	a := t.Actions[0]
	amount, err := moneyutil.DecimalToRailMinor("USD", a.Amount)
	if err != nil || amount != 0 || !a.Is("validate") {
		return result, errors.New("verification lookup is not a zero-value validation")
	}
	if a.Succeeded() && strings.TrimSpace(a.ResponseCode) == "100" && strings.EqualFold(strings.TrimSpace(t.Condition), "complete") {
		return VerificationEvidence{t.TransactionID, 100}, nil
	}
	if code, declined := a.DefinitiveDecline(t.Condition); declined {
		return VerificationEvidence{t.TransactionID, code}, nil
	}
	return result, nil
}
