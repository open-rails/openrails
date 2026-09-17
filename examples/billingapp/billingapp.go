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
)

// Inputs are facts established by merchant setup or earlier customer activity:
// an armed checkout price, an existing subscriber and a closed invoice.
type Inputs struct {
	Currency         string
	Run              string
	CheckoutPriceKey string
	CheckoutRail     string
	SubscriberID     openrails.CustomerID
	SubscriptionID   openrails.SubscriptionID
	InvoiceID        uuid.UUID
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
	payer := openrails.CustomerID(uuid.New())
	invoker := "app:" + in.Run

	if err := client.SetMerchantSettings(ctx, openrails.MerchantSettings{
		BillingPolicies: []openrails.BillingPolicyInput{{
			Name: "app_window", Kind: "window_spend_cap",
			SpendWindows: []openrails.BudgetWindowInput{{Key: "hourly", WindowSeconds: 3600, Limit: 50_000}},
		}},
		BillingPolicyBindings: []openrails.BillingPolicyBindingInput{{PolicyName: "app_window", Tier: "app"}},
	}); err != nil {
		return r, fmt.Errorf("set policy: %w", err)
	}
	settings, err := client.GetMerchantSettings(ctx)
	if err != nil {
		return r, fmt.Errorf("read policy: %w", err)
	}
	for _, policy := range settings.BillingPolicies {
		if policy.Name == "app_window" {
			r.PolicyWindows = len(policy.SpendWindows)
		}
	}
	if err := client.SetCreditLimit(ctx, payer, in.Currency, 0); err != nil {
		return r, fmt.Errorf("set credit limit: %w", err)
	}
	if r.CreditLimit, err = client.GetCreditLimit(ctx, payer, in.Currency); err != nil {
		return r, fmt.Errorf("read credit limit: %w", err)
	}

	deposit, err := client.DepositCredits(ctx, openrails.DepositCreditsRequest{
		CustomerID: &payer, Invoker: invoker, Currency: in.Currency, Amount: 100_000,
		Source: "billingapp", SourceID: in.Run + ":deposit", Description: "prepaid balance",
	})
	if err != nil {
		return r, fmt.Errorf("deposit: %w", err)
	}
	r.Deposited = deposit.Amount

	expires := time.Now().Add(time.Hour)
	job := in.Run + ":job"
	admitted, err := client.Admit(ctx, openrails.AdmitRequest{
		CustomerID: payer, Invoker: invoker, InvokerType: openrails.InvokerTypePayer, Currency: in.Currency,
		EstimatedAmount: 10_000, ExpiresAt: &expires, RequestID: job, Source: "billingapp",
	})
	if err != nil {
		return r, fmt.Errorf("admit: %w", err)
	}
	r.Admitted = admitted.Allowed
	receipt, err := client.Capture(ctx, job, 7_500, &openrails.CaptureUsage{
		EventType: "generation", Resource: "image", Source: "billingapp", SourceID: job,
	})
	if err != nil {
		return r, fmt.Errorf("capture: %w", err)
	}
	r.Captured = receipt.Amount
	denied, err := client.Admit(ctx, openrails.AdmitRequest{
		CustomerID: payer, Invoker: invoker, InvokerType: openrails.InvokerTypePayer, Currency: in.Currency,
		EstimatedAmount: 10_000_000, ExpiresAt: &expires, RequestID: in.Run + ":too-large", Source: "billingapp",
	})
	if err != nil {
		return r, fmt.Errorf("admit over balance: %w", err)
	}
	r.DeniedBy = denied.BlockedBy
	r.UnknownRelease = errors.Is(client.Release(ctx, uuid.NewString()), openrails.ErrNotFound)
	balance, err := client.Balance(ctx, payer)
	if err != nil {
		return r, fmt.Errorf("balance: %w", err)
	}
	r.Balance = balance.BalanceAmount
	rows, err := client.UsageRollup(ctx, payer, in.Currency, time.Now().Add(-time.Hour), time.Now().Add(time.Hour), "resource")
	if err != nil {
		return r, fmt.Errorf("usage rollup: %w", err)
	}
	for _, row := range rows {
		r.UsageEvents += row.EventCount
	}

	price, err := client.GetPriceByKey(ctx, in.CheckoutPriceKey)
	if err != nil {
		return r, fmt.Errorf("resolve checkout price: %w", err)
	}
	options, err := client.ListCheckoutRailOptions(ctx, price.ID)
	if err != nil {
		return r, fmt.Errorf("checkout options: %w", err)
	}
	r.CheckoutRails = len(options)
	buyer := openrails.CustomerID(uuid.New())
	request := openrails.CreateCheckoutSessionRequest{
		Customer:       openrails.CheckoutCustomerIdentity{ID: buyer, VerifiedEmail: "buyer@example.test", Username: "buyer-" + buyer.String()[:8]},
		PriceID:        price.ID,
		IdempotencyKey: in.Run + ":checkout",
		Payment:        openrails.CheckoutPayment{Rail: in.CheckoutRail, NameOnCard: "Example Buyer", Zip: "90210", Country: "US"},
	}
	session, err := client.CreateCheckoutSession(ctx, request)
	if err != nil {
		return r, fmt.Errorf("create checkout: %w", err)
	}
	replayed, err := client.CreateCheckoutSession(ctx, request)
	if err != nil {
		return r, fmt.Errorf("replay checkout: %w", err)
	}
	read, err := client.GetCheckoutSession(ctx, buyer, session.ID)
	if err != nil {
		return r, fmt.Errorf("read checkout: %w", err)
	}
	r.CheckoutReplayed = replayed.ID == session.ID && read.ID == session.ID
	r.CheckoutAmount = read.Amount

	if err := client.CancelSubscription(ctx, in.SubscriptionID, openrails.CancelSubscriptionRequest{Reason: "customer request"}); err != nil {
		return r, fmt.Errorf("cancel subscription: %w", err)
	}
	page, err := client.ListSubscriptions(ctx, openrails.SubscriptionFilter{CustomerID: in.SubscriberID, PageOptions: openrails.PageOptions{Limit: 10}})
	if err != nil {
		return r, fmt.Errorf("list subscriptions: %w", err)
	}
	for _, sub := range page.Data {
		if sub.ID == in.SubscriptionID {
			r.SubscriptionStatus, r.CancelScheduled = sub.Status, sub.CancelScheduled
		}
	}
	if err := client.ResumeSubscription(ctx, in.SubscriptionID); err != nil {
		return r, fmt.Errorf("resume subscription: %w", err)
	}
	r.Resumed = true

	invoice, err := client.GetMerchantInvoice(ctx, in.InvoiceID)
	if err != nil {
		return r, fmt.Errorf("read invoice: %w", err)
	}
	if r.InvoiceProfileSet, err = client.EnsureCustomerInvoiceProfile(ctx, invoice.CustomerID, openrails.InvoiceProfileDTO{
		NetTermsDays: 14, CollectionMethod: "send_invoice",
	}); err != nil {
		return r, fmt.Errorf("invoice profile: %w", err)
	}
	payment := openrails.RecordInvoicePaymentRequest{Amount: invoice.AmountDue / 2, Reference: in.Run + ":wire"}
	paid, err := client.RecordInvoicePayment(ctx, in.InvoiceID, payment)
	if err != nil {
		return r, fmt.Errorf("record invoice payment: %w", err)
	}
	r.InvoiceDueAfterPaid = paid.AmountDue
	_, err = client.RecordInvoicePayment(ctx, in.InvoiceID, payment)
	r.InvoicePaidTwice = err == nil
	if !errors.Is(err, openrails.ErrConflict) {
		return r, fmt.Errorf("replayed remittance: %w", err)
	}
	voided, err := client.VoidInvoice(ctx, in.InvoiceID)
	if err != nil {
		return r, fmt.Errorf("void invoice: %w", err)
	}
	r.InvoiceStatus = voided.Status
	return r, nil
}
