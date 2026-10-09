// Package billingapp is an ordinary application's billing workflow written once
// against *openrails.Client. Embedded, standalone and multi-merchant hosts run
// this code unchanged; only client construction differs.
package billingapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
)

// Inputs are facts established by merchant setup or earlier customer activity:
// an armed checkout price, an existing subscriber and a closed invoice.
type Inputs struct {
	Currency           string
	Run                string
	CheckoutProductKey string
	CheckoutPriceKey   string
	CheckoutCustomerID billing.CustomerID
	SubscriberID       billing.CustomerID
	SubscriptionID     billing.SubscriptionID
	InvoiceID          uuid.UUID
}

// Report is what the application observed. Identifiers created per run are
// omitted so reports from different deployments compare equal.
type Report struct {
	PolicyWindows       int
	CreditLimit         int64
	Deposited           int64
	Admitted            bool
	Captured            int64
	DeniedBy            string
	UnknownRelease      bool
	Balance             int64
	UsageEvents         int64
	CheckoutRails       int
	CheckoutSession     bool
	SubscriptionStatus  string
	CancelScheduled     bool
	Resumed             bool
	InvoiceProfileSet   bool
	InvoiceDueAfterPaid int64
	InvoiceStatus       string
	InvoicePaidTwice    bool
}

// Run executes the workflow with the given client.
func Run(ctx context.Context, client *openrails.Client, in Inputs) (Report, error) {
	var r Report
	customers, err := client.EnsureCustomers(ctx, []billing.EnsureCustomerParams{{ID: billing.CustomerID(uuid.New())}})
	if err != nil {
		return r, fmt.Errorf("ensure customer: %w", err)
	}
	payer := customers[0].ID
	invoker := "app:" + in.Run

	current, err := client.GetMerchantConfiguration(ctx)
	if err != nil {
		return r, fmt.Errorf("read configuration: %w", err)
	}
	if _, err := client.ApplyMerchantConfiguration(ctx, billing.ApplyMerchantConfigurationParams{
		ApplicationID: "billingapp-" + in.Run, ExpectedRevision: &current.Revision,
		Settings: &billing.MerchantSettings{
			BillingPolicies: []billing.BillingPolicy{{
				Name: "app_window", Kind: "window_spend_cap",
				SpendWindows: []billing.BudgetWindow{{Key: "hourly", WindowSeconds: 3600, Limit: 50_000}},
			}},
			BillingPolicyBindings: []billing.BillingPolicyBinding{{PolicyName: "app_window", Tier: "app"}},
		},
	}); err != nil {
		return r, fmt.Errorf("set policy: %w", err)
	}
	applied, err := client.GetMerchantConfiguration(ctx)
	if err != nil {
		return r, fmt.Errorf("read policy: %w", err)
	}
	for _, policy := range applied.Settings.BillingPolicies {
		if policy.Name == "app_window" {
			r.PolicyWindows = len(policy.SpendWindows)
		}
	}
	if _, err := client.UpdateCustomerSettings(ctx, []billing.UpdateCustomerSettingsParams{{CustomerID: payer, CreditLimits: []billing.CreditLimit{{Currency: in.Currency}}}}); err != nil {
		return r, fmt.Errorf("set credit limit: %w", err)
	}
	settings, err := client.ListCustomerSettings(ctx, billing.CustomerSettingsListParams{IDs: []billing.CustomerID{payer}})
	if err != nil || len(settings.Items) != 1 {
		return r, fmt.Errorf("read credit limit: %w", err)
	}
	for _, limit := range settings.Items[0].CreditLimits {
		if limit.Currency == in.Currency {
			r.CreditLimit = limit.Amount
		}
	}

	description := "prepaid balance"
	grants, err := client.CreateCreditGrants(ctx, []billing.CreateCreditGrantParams{{
		CustomerID: payer, Invoker: invoker, Currency: in.Currency, Amount: 100_000,
		Source: "billingapp", SourceID: in.Run + ":deposit", Description: &description,
	}})
	if err != nil {
		return r, fmt.Errorf("credit grant: %w", err)
	}
	r.Deposited = grants[0].Amount

	expires := time.Now().Add(time.Hour)
	job := in.Run + ":job"
	admitted, err := client.Admit(ctx, []billing.AdmitParams{{
		CustomerID: payer, Invoker: invoker, InvokerType: billing.InvokerTypeCustomer, Currency: in.Currency,
		EstimatedAmount: 10_000, ExpiresAt: &expires, RequestID: job, Source: "billingapp",
	}})
	if err != nil {
		return r, fmt.Errorf("admit: %w", err)
	}
	r.Admitted = admitted[0].Allowed()
	receipt, err := client.CaptureAdmission(ctx, job, billing.CaptureAdmissionParams{Amount: 7_500, Usage: &billing.CaptureUsage{
		EventType: "generation", Resource: "image", Source: "billingapp", SourceID: job,
	}})
	if err != nil {
		return r, fmt.Errorf("capture: %w", err)
	}
	r.Captured = receipt.Amount
	denied, err := client.Admit(ctx, []billing.AdmitParams{{
		CustomerID: payer, Invoker: invoker, InvokerType: billing.InvokerTypeCustomer, Currency: in.Currency,
		EstimatedAmount: 10_000_000, ExpiresAt: &expires, RequestID: in.Run + ":too-large", Source: "billingapp",
	}})
	if err != nil {
		return r, fmt.Errorf("admit over balance: %w", err)
	}
	if blocked := denied[0].Admission.BlockedBy; blocked != nil {
		r.DeniedBy = string(*blocked)
	}
	released, err := client.ReleaseAdmissions(ctx, []string{uuid.NewString()})
	if err != nil {
		return r, fmt.Errorf("release: %w", err)
	}
	r.UnknownRelease = errors.Is(released[0].Err(), billing.ErrNotFound)
	balance, err := client.GetBalance(ctx, payer, in.Currency)
	if err != nil {
		return r, fmt.Errorf("balance: %w", err)
	}
	r.Balance = balance.BalanceAmount
	usage, err := client.GetUsage(ctx, payer, billing.GetUsageParams{Currency: in.Currency, From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour), GroupBy: billing.UsageByResource})
	if err != nil {
		return r, fmt.Errorf("usage: %w", err)
	}
	for _, row := range usage.Rows {
		r.UsageEvents += row.EventCount
	}

	options, err := client.ListCheckoutOptions(ctx, billing.CheckoutOptionListParams{ProductKey: in.CheckoutProductKey, PriceKey: in.CheckoutPriceKey})
	if err != nil {
		return r, fmt.Errorf("checkout options: %w", err)
	}
	r.CheckoutRails = len(options.Items)
	// The buyer pays the session on the payment page; a saved card needs the
	// buyer's own proof there.
	link, err := client.CreateCheckoutSession(ctx, billing.CreateCheckoutSessionParams{
		Customer:   billing.CheckoutCustomerIdentity{ID: in.CheckoutCustomerID, VerifiedEmail: "buyer@example.test"},
		ProductKey: in.CheckoutProductKey,
		PriceKey:   in.CheckoutPriceKey,
	})
	if err != nil {
		return r, fmt.Errorf("create checkout session: %w", err)
	}
	r.CheckoutSession = link.ID != ""

	if _, err := client.CancelSubscription(ctx, in.SubscriptionID, billing.CancelSubscriptionParams{Reason: "customer request"}); err != nil {
		return r, fmt.Errorf("cancel subscription: %w", err)
	}
	page, err := client.ListSubscriptions(ctx, billing.SubscriptionListParams{CustomerID: in.SubscriberID, PageRequest: billing.PageRequest{Limit: 10}})
	if err != nil {
		return r, fmt.Errorf("list subscriptions: %w", err)
	}
	for _, sub := range page.Items {
		if sub.ID == in.SubscriptionID {
			r.SubscriptionStatus, r.CancelScheduled = string(sub.Status), sub.CancelScheduled
		}
	}
	if _, err := client.ResumeSubscription(ctx, in.SubscriptionID); err != nil {
		return r, fmt.Errorf("resume subscription: %w", err)
	}
	r.Resumed = true

	invoiceID := billing.InvoiceID(in.InvoiceID)
	invoice, err := client.GetInvoice(ctx, invoiceID)
	if err != nil {
		return r, fmt.Errorf("read invoice: %w", err)
	}
	terms, err := client.UpdateCustomerSettings(ctx, []billing.UpdateCustomerSettingsParams{{
		CustomerID: invoice.CustomerID, InvoiceProfile: catalog.Value(billing.InvoiceProfile{NetTermsDays: 14, CollectionMethod: billing.CollectSendInvoice}),
	}})
	if err != nil {
		return r, fmt.Errorf("invoice profile: %w", err)
	}
	r.InvoiceProfileSet = terms[0].InvoiceProfile.NetTermsDays == 14
	payment := billing.CreateInvoicePaymentParams{Amount: invoice.AmountDue / 2, Reference: in.Run + ":wire"}
	paid, err := client.CreateInvoicePayment(ctx, invoiceID, payment)
	if err != nil {
		return r, fmt.Errorf("record invoice payment: %w", err)
	}
	r.InvoiceDueAfterPaid = paid.AmountDue
	_, err = client.CreateInvoicePayment(ctx, invoiceID, payment)
	r.InvoicePaidTwice = err == nil
	if !errors.Is(err, billing.ErrConflict) {
		return r, fmt.Errorf("replayed remittance: %w", err)
	}
	voided, err := client.VoidInvoice(ctx, invoiceID)
	if err != nil {
		return r, fmt.Errorf("void invoice: %w", err)
	}
	r.InvoiceStatus = string(voided.Status)
	return r, nil
}
