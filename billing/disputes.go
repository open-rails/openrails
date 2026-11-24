package billing

import (
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/catalog"
)

// DisputeID names one provider dispute case; on the wire "dsp_<uuid>".
type DisputeID uuid.UUID

const disputeIDPrefix = "dsp_"

func ParseDisputeID(s string) (DisputeID, error) {
	u, err := parsePrefixedID("dispute", disputeIDPrefix, s)
	return DisputeID(u), err
}

func (id DisputeID) UUID() uuid.UUID              { return uuid.UUID(id) }
func (id DisputeID) IsZero() bool                 { return uuid.UUID(id) == uuid.Nil }
func (id DisputeID) String() string               { return formatPrefixedID(disputeIDPrefix, uuid.UUID(id)) }
func (id DisputeID) MarshalText() ([]byte, error) { return []byte(id.String()), nil }
func (id *DisputeID) UnmarshalText(b []byte) error {
	v, err := ParseDisputeID(string(b))
	*id = v
	return err
}

// DisputeKind is what a provider case is. Only a chargeback takes the money
// back and counts in a card network's dispute ratio.
type DisputeKind string

const (
	// DisputeInquiry is the issuer asking before it disputes: nothing is
	// withdrawn and nothing ends.
	DisputeInquiry    DisputeKind = "inquiry"
	DisputeChargeback DisputeKind = "chargeback"
	// DisputePrevented was resolved before it was filed: an RDR refund or a
	// Compelling Evidence 3.0 block.
	DisputePrevented DisputeKind = "prevented"
)

// DisputeStatus is where a case stands.
type DisputeStatus string

const (
	DisputeOpen DisputeStatus = "open"
	DisputeWon  DisputeStatus = "won"
	DisputeLost DisputeStatus = "lost"
	// DisputeClosed is an inquiry or a prevention that never became a
	// chargeback.
	DisputeClosed DisputeStatus = "closed"
	// DisputeUnreported is a chargeback whose rail never reports the outcome
	// (NMI, CCBill).
	DisputeUnreported DisputeStatus = "unreported"
)

// DisputeReason is the family a network reason code or a provider's reason
// belongs to.
type DisputeReason string

const (
	DisputeFraud DisputeReason = "fraud"
	// DisputeUnrecognized is a cardholder who does not recognize the charge.
	DisputeUnrecognized       DisputeReason = "unrecognized"
	DisputeNotReceived        DisputeReason = "not_received"
	DisputeCanceledRecurring  DisputeReason = "canceled_recurring"
	DisputeNotAsDescribed     DisputeReason = "not_as_described"
	DisputeCreditNotProcessed DisputeReason = "credit_not_processed"
	DisputeDuplicate          DisputeReason = "duplicate"
	DisputeProcessingError    DisputeReason = "processing_error"
	DisputeAuthorization      DisputeReason = "authorization"
	DisputeOtherReason        DisputeReason = "other"
	DisputeUnknownReason      DisputeReason = "unknown"
)

// CardNetwork is the card network a charge ran on.
type CardNetwork string

const (
	NetworkVisa       CardNetwork = "visa"
	NetworkMastercard CardNetwork = "mastercard"
	NetworkAmex       CardNetwork = "amex"
	NetworkDiscover   CardNetwork = "discover"
	NetworkJCB        CardNetwork = "jcb"
	NetworkDiners     CardNetwork = "diners"
	NetworkUnionPay   CardNetwork = "unionpay"
	NetworkOther      CardNetwork = "other"
	NetworkUnknown    CardNetwork = "unknown"
)

// RefundReason is why a refund was made.
type RefundReason string

const (
	RefundRequestedByCustomer RefundReason = "requested_by_customer"
	RefundDuplicate           RefundReason = "duplicate"
	RefundFraudulent          RefundReason = "fraudulent"
	// RefundFraudWarning answered an issuer's fraud report.
	RefundFraudWarning RefundReason = "fraud_warning"
	// RefundDisputePrevention resolved a dispute before it was filed.
	RefundDisputePrevention RefundReason = "dispute_prevention"
	// RefundChargedAfterCancel returned a charge taken after cancellation.
	RefundChargedAfterCancel RefundReason = "charged_after_cancel"
	RefundOther              RefundReason = "other"
	// RefundUnknown is a provider dashboard refund that names no reason.
	RefundUnknown RefundReason = "unknown"
)

// Valid reports whether r is one of the reasons.
func (r RefundReason) Valid() bool {
	switch r {
	case RefundRequestedByCustomer, RefundDuplicate, RefundFraudulent, RefundFraudWarning, RefundDisputePrevention,
		RefundChargedAfterCancel, RefundOther, RefundUnknown:
		return true
	}
	return false
}

// Dispute is one provider dispute case: an issuer's inquiry, a chargeback or
// a dispute resolved before it was filed. Its money is the payments it names.
type Dispute struct {
	ID DisputeID `json:"id"`
	// ProviderRef is the case as its rail names it: a Stripe dispute id, an
	// NMI chargeback id or a CCBill transaction id.
	ProviderRef string `json:"provider_ref"`
	Rail        Rail   `json:"rail"`
	PSPID       PSPID  `json:"psp_id"`
	// CustomerID and PaymentID are the disputed charge's; null when the rail's
	// report matched no single charge.
	CustomerID *CustomerID   `json:"customer_id"`
	PaymentID  *PaymentID    `json:"payment_id"`
	Kind       DisputeKind   `json:"kind"`
	Status     DisputeStatus `json:"status"`
	Network    CardNetwork   `json:"network"`
	Reason     DisputeReason `json:"reason"`
	// ReasonCode is the network's code (Visa 10.4, Mastercard 4853) and
	// ProviderReason the provider's reason, each verbatim.
	ReasonCode     string `json:"reason_code"`
	ProviderReason string `json:"provider_reason"`
	// Amount is null when the report named no currency it could be matched in.
	Amount   *int64            `json:"amount,string"`
	Currency *catalog.Currency `json:"currency"`
	// DisputedAt is when the case opened at the provider; ChargebackAt when it
	// became a chargeback, the date the network ratios count it at.
	DisputedAt    time.Time  `json:"disputed_at"`
	ChargebackAt  *time.Time `json:"chargeback_at"`
	EvidenceDueAt *time.Time `json:"evidence_due_at"`
	ClosedAt      *time.Time `json:"closed_at"`
	// ReversalPaymentID is the reversal that took the money back;
	// RecoveryPaymentID the payment that returned it when the dispute was won.
	ReversalPaymentID *PaymentID `json:"reversal_payment_id"`
	RecoveryPaymentID *PaymentID `json:"recovery_payment_id"`
}

// DisputeListParams selects disputes, newest first; every field is optional,
// and a list matches any of its values. Since and Until bound disputed_at to
// [Since, Until).
type DisputeListParams struct {
	PageRequest
	Kind       []DisputeKind   `form:"kind,comma"`
	Status     []DisputeStatus `form:"status,comma"`
	Reason     []DisputeReason `form:"reason,comma"`
	Network    []CardNetwork   `form:"network,comma"`
	PSPID      PSPID           `form:"psp_id"`
	CustomerID CustomerID      `form:"customer_id"`
	PaymentID  PaymentID       `form:"payment_id"`
	Since      time.Time       `form:"since"`
	Until      time.Time       `form:"until"`
}

// DisputeProgram is a dispute monitoring program OpenRails evaluates.
type DisputeProgram string

const (
	// ProgramVisaVAMP is the Visa Acquirer Monitoring Program: fraud reports
	// and disputes over settled sales, same month, by count.
	ProgramVisaVAMP DisputeProgram = "visa_vamp"
	// ProgramMastercardECP is Mastercard's Excessive Chargeback Program:
	// chargebacks over the previous month's sales, by count.
	ProgramMastercardECP DisputeProgram = "mastercard_ecp"
	// ProgramMastercardEFM is Mastercard's Excessive Fraud Merchant program:
	// fraud chargebacks over the previous month's sales, by amount.
	ProgramMastercardEFM DisputeProgram = "mastercard_efm"
	// ProgramStripe is Stripe's own dispute-rate guidance.
	ProgramStripe DisputeProgram = "stripe"
	// ProgramAcquirer is the PSP's declared dispute_ratio_limit.
	ProgramAcquirer DisputeProgram = "acquirer"
)

// DisputeLevel is how far a program's projected ratio reaches.
type DisputeLevel string

const (
	DisputeLevelNone          DisputeLevel = "none"
	DisputeLevelWarning       DisputeLevel = "warning"
	DisputeLevelExcessive     DisputeLevel = "excessive"
	DisputeLevelHighExcessive DisputeLevel = "high_excessive"
)

// DisputeThreshold is one level of a program in force in the month.
type DisputeThreshold struct {
	Level DisputeLevel `json:"level"`
	// Ratio is a fraction (0.015 is 1.5%).
	Ratio float64 `json:"ratio"`
	// MinCount is the month's least count; MinAmount the least amount in USD
	// native units (the amount basis); MinPriorSales the previous month's
	// least settled sales.
	MinCount      int64  `json:"min_count,string"`
	MinAmount     int64  `json:"min_amount,string"`
	MinPriorSales int64  `json:"min_prior_sales,string"`
	Source        string `json:"source"`
}

// DisputeStanding is one PSP's standing in one program for a month.
type DisputeStanding struct {
	PSPID   PSPID          `json:"psp_id"`
	Rail    Rail           `json:"rail"`
	Program DisputeProgram `json:"program"`
	// Month is the first instant of the UTC month.
	Month time.Time `json:"month"`
	// Count is what the program counts so far: chargebacks, plus fraud
	// reports for VAMP, or the USD amount of fraud chargebacks for EFM.
	// Denominator is the sales (or USD amount) it divides by.
	Count       int64 `json:"count,string"`
	Denominator int64 `json:"denominator,string"`
	// Ratio is the month so far; ProjectedRatio and ProjectedCount the whole
	// month, from the month so far (the last 30 days before its 7th day).
	Ratio          float64      `json:"ratio"`
	ProjectedRatio float64      `json:"projected_ratio"`
	ProjectedCount int64        `json:"projected_count,string"`
	Level          DisputeLevel `json:"level"`
	// Thresholds are the program's levels in force in the month.
	Thresholds []DisputeThreshold `json:"thresholds"`
	// FraudReportsVisible is false where the rail passes on no fraud reports
	// (NMI, CCBill): VAMP's count is then a lower bound.
	FraudReportsVisible bool `json:"fraud_reports_visible"`
	// DisputesReported is false until the PSP's rail has reported a dispute:
	// some processors report none, which reads as a clean 0%.
	DisputesReported bool `json:"disputes_reported"`
}

// DisputeProgramListParams selects the month containing At (default now) and
// optionally one PSP.
type DisputeProgramListParams struct {
	At    *time.Time `form:"at"`
	PSPID PSPID      `form:"psp_id"`
}
