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
)

// Inputs are facts established by merchant setup or earlier customer activity:
// an armed checkout price and saved method, an existing subscriber and a closed invoice.
type Inputs struct {
	Currency                string
	Run                     string
	CheckoutProductKey      string
	CheckoutPriceKey        string
	CheckoutRail            string
	CheckoutCustomerID      billing.CustomerID
	CheckoutPaymentMethodID billing.PaymentMethodID
	SubscriberID            billing.CustomerID
	SubscriptionID          billing.SubscriptionID
	InvoiceID               uuid.UUID
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
	CheckoutReplayed    bool
	CheckoutAmount      int64
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
	customer, err := client.EnsureCustomer(ctx, billing.CustomerID(uuid.New()), billing.EnsureCustomerParams{})
	if err != nil {
		return r, fmt.Errorf("ensure customer: %w", err)
	}
	payer := customer.ID
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
	if _, err := client.SetCreditLimit(ctx, payer, billing.SetCreditLimitParams{Currency: in.Currency}); err != nil {
		return r, fmt.Errorf("set credit limit: %w", err)
	}
	limit, err := client.GetCreditLimit(ctx, payer, in.Currency)
	if err != nil {
		return r, fmt.Errorf("read credit limit: %w", err)
	}
	r.CreditLimit = limit.Amount

	description := "prepaid balance"
	grant, err := client.CreateCreditGrant(ctx, payer, billing.CreateCreditGrantParams{
		Invoker: invoker, Currency: in.Currency, Amount: 100_000,
		Source: "billingapp", SourceID: in.Run + ":deposit", Description: &description,
	})
	if err != nil {
		return r, fmt.Errorf("credit grant: %w", err)
	}
	r.Deposited = grant.Amount

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
	_, releaseErr := client.ReleaseAdmission(ctx, uuid.NewString())
	r.UnknownRelease = errors.Is(releaseErr, billing.ErrNotFound)
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

	checkoutConfig, err := client.GetCheckoutConfig(ctx, billing.GetCheckoutConfigParams{ProductKey: in.CheckoutProductKey, PriceKey: in.CheckoutPriceKey})
	if err != nil {
		return r, fmt.Errorf("checkout options: %w", err)
	}
	r.CheckoutRails = len(checkoutConfig.Options)
	buyer := in.CheckoutCustomerID
	request := billing.CreateCheckoutAttemptParams{
		Customer:       billing.CheckoutCustomerIdentity{ID: buyer, VerifiedEmail: "buyer@example.test", Username: "buyer-" + buyer.String()[:8]},
		ProductKey:     in.CheckoutProductKey,
		PriceKey:       in.CheckoutPriceKey,
		IdempotencyKey: in.Run + ":checkout",
		PaymentOptions: billing.CheckoutPaymentOptions{PSP: in.CheckoutRail, PaymentMethodID: in.CheckoutPaymentMethodID, BillingDetails: &billing.BillingDetails{Name: new("Example Buyer"), Address: &billing.BillingAddress{PostalCode: new("90210"), Country: new("US")}}},
	}
	session, err := client.CreateCheckoutAttempt(ctx, request)
	if err != nil {
		return r, fmt.Errorf("create checkout: %w", err)
	}
	replayed, err := client.CreateCheckoutAttempt(ctx, request)
	if err != nil {
		return r, fmt.Errorf("replay checkout: %w", err)
	}
	read, err := client.GetCheckoutAttempt(ctx, session.ID)
	if err != nil {
		return r, fmt.Errorf("read checkout: %w", err)
	}
	r.CheckoutReplayed = replayed.ID == session.ID && read.ID == session.ID
	if read.Amount == nil {
		return r, fmt.Errorf("priced checkout returned no amount")
	}
	r.CheckoutAmount = *read.Amount

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
	profile, err := client.SetInvoiceProfile(ctx, invoice.CustomerID, billing.SetInvoiceProfileParams{
		InvoiceProfile: billing.InvoiceProfile{NetTermsDays: 14, CollectionMethod: billing.CollectSendInvoice}, IfAbsent: true,
	})
	if err != nil {
		return r, fmt.Errorf("invoice profile: %w", err)
	}
	r.InvoiceProfileSet = profile.NetTermsDays == 14
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
