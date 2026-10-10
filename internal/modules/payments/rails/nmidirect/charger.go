// Package nmidirect implements the charge seam (internal/modules/payments/charge)
// for the direct NMI rail: it maps CIT/MIT context onto NMI's
// credential-on-file fields and normalizes gateway outcomes. nmiproxy is the
// custodian-proxied sibling.
package nmidirect

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
)

// StoredCredentialFor derives NMI's credential-on-file fields from a charge's
// flow (see nmi.StoredCredential for the verified combinations). It is the one
// NMI mapping: the direct seam, the NMI schedule lanes (add_subscription,
// rebill_subscription) and the custodian transports that post NMI's form all
// use it. A purchase that stores nothing gets no fields (nil). References
// never cross the recurring and unscheduled sequences, and a merchant-
// initiated charge needs its mandate's initial reference.
func StoredCredentialFor(c charge.Context) (*nmi.StoredCredential, error) {
	switch c.Agreement {
	case charge.AgreementNone:
		if c.Initiator != charge.InitiatorCustomer || c.Cites != nil {
			return nil, errors.New("only a customer-present purchase runs without an agreement")
		}
		return nil, nil
	case charge.AgreementRecurring, charge.AgreementUnscheduled, charge.AgreementCardOnFile:
	default:
		return nil, fmt.Errorf("unknown agreement %q", c.Agreement)
	}
	sc := &nmi.StoredCredential{Recurring: c.Agreement == charge.AgreementRecurring}
	switch c.Initiator {
	case charge.InitiatorCustomer:
		sc.InitiatedBy = nmi.InitiatedByCustomer
	case charge.InitiatorMerchant:
		if c.Agreement == charge.AgreementCardOnFile {
			return nil, errors.New("card-on-file consent never covers a merchant-initiated charge")
		}
		sc.InitiatedBy = nmi.InitiatedByMerchant
	default:
		return nil, errors.New("charge initiation is not established")
	}
	if c.Cites == nil {
		sc.Indicator = nmi.IndicatorStored
		return sc, sc.Validate()
	}
	if c.Cites.Kind.Sequence() != c.Agreement.Sequence() {
		return nil, errors.New("stored-credential references do not cross the recurring and unscheduled sequences")
	}
	sc.Indicator = nmi.IndicatorUsed
	sc.InitialTransactionID = strings.TrimSpace(c.Cites.InitialTransactionID)
	return sc, sc.Validate()
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
	storedCredential, err := StoredCredentialFor(req.Context)
	if err != nil {
		return charge.Result{}, errors.Join(charge.ErrNotDispatched, err)
	}

	sale, err := c.Client.RunSale(ctx, nmi.SaleParams{
		CustomerVaultID:  railCustomerRef,
		BillingID:        strings.TrimSpace(req.Instrument.MethodRef),
		Amount:           req.AmountMinor,
		Currency:         req.Currency,
		OrderDescription: req.Description,
		OrderID:          req.OrderRef,
		StoredCredential: storedCredential,
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
	// An approved storing transaction is its agreement's lineage: NMI's
	// reference is its gateway transactionid.
	if req.Context.Storing() {
		res.CapturedRef = res.TransactionID
	}
	return res, nil
}

// IsHardDecline classifies parsed gateway response codes: communication
// errors (420, 421) and duplicate-transaction detection (430) are transient,
// every other parsed failure is a hard decline. The custodian-proxied charges
// share it.
func IsHardDecline(code int) bool {
	return !nmi.UncertainResponseCode(code)
}

// FailureCode extracts the verbatim rail failure code from a parsed NMI
// decline: its localization id, else nmi_response_<code>.
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
