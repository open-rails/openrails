// Package nmidirect implements the rail-agnostic charge seam (#297 Phase A,
// internal/modules/payments/charge) for the direct NMI rail: it translates
// CIT/MIT context into NMI's credential-on-file wire fields and normalizes
// gateway outcomes. The custodian-proxied transport (nmiproxy) is a sibling
// implementation of the same seam.
package nmidirect

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// StoredCredentialFor maps the rail-agnostic CIT/MIT context onto NMI's
// credential-on-file wire fields (see nmi.StoredCredential for the verified
// combinations). It is shared by the seam Charger and by the NMI
// subscription-engine lanes (add_subscription enrollments, rebill_subscription
// dunning) that charge NMI schedule objects instead of (instrument, amount).
func StoredCredentialFor(c charge.Context) *nmi.StoredCredential {
	sc := &nmi.StoredCredential{
		Recurring: c.Agreement == charge.AgreementRecurring,
	}
	switch c.Initiator {
	case charge.InitiatorMerchant:
		sc.InitiatedBy = nmi.InitiatedByMerchant
	default:
		sc.InitiatedBy = nmi.InitiatedByCustomer
	}
	// The sequence's initial transaction sends indicator=stored; everything
	// after sends used. A merchant-initiated charge is never the initial
	// transaction — a reference-less MIT (legacy instrument) still sends used,
	// just without initial_transaction_id, and the success anchors the
	// sequence (Result.CapturedRef).
	if c.FirstUse && c.Initiator == charge.InitiatorCustomer {
		sc.Indicator = nmi.IndicatorStored
	} else {
		sc.Indicator = nmi.IndicatorUsed
	}
	if ref := strings.TrimSpace(c.PriorRef); ref != "" {
		sc.InitialTransactionID = ref
	}
	return sc
}

// Charger charges stored NMI instruments through the seam. Declines that the
// gateway parsed cleanly come back as Result{Declined:true}; transient
// gateway conditions (communication errors, duplicate detection) and
// transport-ambiguous failures return an error for the caller's retry/verify
// machinery (nmi.IsTransportAmbiguous distinguishes the latter).
type Charger struct {
	Client *nmi.NMIClient
}

func New(client *nmi.NMIClient) *Charger { return &Charger{Client: client} }

var _ charge.Charger = (*Charger)(nil)

func (c *Charger) Charge(ctx context.Context, req charge.Request) (charge.Result, error) {
	if c == nil || c.Client == nil {
		return charge.Result{}, errors.New("nmidirect charger not initialized")
	}
	railCustomerRef := strings.TrimSpace(req.Instrument.CustomerRef)
	if railCustomerRef == "" {
		return charge.Result{}, errors.New("nmi instrument missing customer vault id")
	}
	if req.AmountMinor <= 0 {
		return charge.Result{}, errors.New("charge amount must be positive")
	}

	sale, err := c.Client.RunSale(ctx, nmi.SaleParams{
		CustomerVaultID:  railCustomerRef,
		BillingID:        strings.TrimSpace(req.Instrument.MethodRef),
		Amount:           req.AmountMinor,
		Currency:         req.Currency,
		OrderDescription: req.Description,
		OrderID:          req.OrderRef,
		StoredCredential: StoredCredentialFor(req.Context),
	})
	if err != nil {
		var pmErr *nmi.CustomerVaultError
		if errors.As(err, &pmErr) && IsHardDecline(pmErr.ResponseCode) {
			code := FailureCode(pmErr)
			message := pmErr.Error()
			return charge.Result{
				TokenType:      charge.TokenTypePSPToken,
				Declined:       true,
				FailureCode:    &code,
				FailureMessage: &message,
			}, nil
		}
		return charge.Result{}, err
	}

	res := charge.Result{
		TransactionID: strings.TrimSpace(sale.TransactionID),
		TokenType:     charge.TokenTypePSPToken,
	}
	// A charge that ran without a replay reference anchors the credential's
	// agreement sequence: NMI's reference IS its gateway transactionid.
	if strings.TrimSpace(req.Context.PriorRef) == "" {
		res.CapturedRef = res.TransactionID
	}
	return res, nil
}

// IsHardDecline classifies parsed gateway response codes: communication
// errors (420, 421) and duplicate-transaction detection (430) are transient —
// everything else parsed as a failure is a hard decline. Shared with the
// custodian-proxied charges (#795): one decline taxonomy, two transports.
func IsHardDecline(code int) bool {
	switch code {
	case 420, 421, 430:
		return false
	default:
		return true
	}
}

// FailureCode extracts the verbatim rail failure code from a parsed NMI
// decline (#733 no-fabrication: localization id, else nmi_response_<code>).
func FailureCode(err *nmi.CustomerVaultError) string {
	if err == nil {
		return "nmi_declined"
	}
	if code := strings.TrimSpace(err.LocalizationID); code != "" {
		return code
	}
	if err.ResponseCode != 0 {
		return fmt.Sprintf("nmi_response_%d", err.ResponseCode)
	}
	return "nmi_declined"
}
