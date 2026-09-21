package nmi

import (
	"context"
	"encoding/xml"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// EnrollmentEvidence joins the exact live subscription with its recurring
// report. The report supplies the operation correlation that v5 omits. These
// are schedule facts, not proof of a charge or a stored-credential agreement;
// neither subscription API establishes the payment currency.
type EnrollmentEvidence struct {
	VaultBillingID string         `json:"vault_billing_id,omitempty"`
	Subscription   V5Subscription `json:"subscription"`
	OrderReference string         `json:"order_reference"`
	PONumber       string         `json:"po_number"`
	NextChargeDate string         `json:"next_charge_date"`
}

func (e EnrollmentEvidence) ScheduleAmountMinor(currency string) (moneyutil.Cents, error) {
	amount := strings.TrimSpace(e.Subscription.Amount)
	if amount == "" && e.Subscription.Plan != nil {
		amount = strings.TrimSpace(e.Subscription.Plan.PlanAmount)
	}
	minor, exact := exactMinorAmount(amount, currency)
	if !exact || minor < 0 {
		return 0, receiptMismatch("schedule has no exact nonnegative amount")
	}
	return moneyutil.Cents(minor), nil
}

// ReadEnrollmentEvidence reads one candidate reference on the armed account.
// It never adopts a roster match or interprets absence as nonexecution.
func (c *NMIClient) ReadEnrollmentEvidence(ctx context.Context, reference string) (EnrollmentEvidence, bool, error) {
	var facts EnrollmentEvidence
	if c.accountSecurityKey != "" {
		scoped := *c
		scoped.SecurityKey = c.accountSecurityKey
		c = &scoped
	}
	sub, found, err := c.GetSubscription(ctx, reference)
	if err != nil || !found {
		return facts, false, err
	}
	if sub.ID != reference || sub.Plan == nil || sub.Plan.ID == "" || sub.CustomerVaultID == "" {
		return facts, false, receiptMismatch("subscription does not identify its account-scoped schedule")
	}
	raw, err := c.sendQueryRequest(ctx, url.Values{
		"security_key": {c.SecurityKey}, "report_type": {"recurring"}, "subscription_id": {reference},
	})
	if err != nil {
		return facts, false, err
	}
	var report struct {
		XMLName       xml.Name `xml:"nm_response"`
		Subscriptions []struct {
			ID             string `xml:"id,attr"`
			SubscriptionID string `xml:"subscription_id"`
			OrderID        string `xml:"orderid"`
			PONumber       string `xml:"ponumber"`
			NextChargeDate string `xml:"next_charge_date"`
			PlanID         string `xml:"plan>plan_id"`
		} `xml:"subscription"`
	}
	if xml.Unmarshal([]byte(raw), &report) != nil || len(report.Subscriptions) != 1 {
		return facts, false, receiptMismatch("exact subscription report is unavailable or ambiguous")
	}
	r := report.Subscriptions[0]
	if r.SubscriptionID != reference || (r.ID != "" && r.ID != reference) || r.PlanID != sub.Plan.ID || strings.TrimSpace(r.OrderID) == "" || strings.TrimSpace(r.PONumber) == "" || strings.TrimSpace(r.NextChargeDate) == "" {
		return facts, false, receiptMismatch("subscription report lacks consistent schedule correlation")
	}
	return EnrollmentEvidence{Subscription: sub, OrderReference: r.OrderID, PONumber: r.PONumber, NextChargeDate: r.NextChargeDate}, true, nil
}
