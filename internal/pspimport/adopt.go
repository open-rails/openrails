package pspimport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingimport"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/reconcile"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// ErrProductHeld is an adoption the customer's live subscription to the same
// product refused.
var ErrProductHeld = errors.New("the customer already holds this product")

// AdoptNMISubscription lands one live NMI subscription OpenRails does not
// hold, arriving by webhook, as a one-subscription book: its v5 record mapped
// as an import maps it, on price (the price converge resolved its plan to),
// billing the vault's card, with its transaction history as receipts. The
// caller proved customer the vault's one owner.
func (im *Importer) AdoptNMISubscription(ctx context.Context, pspID uuid.UUID, sub nmi.V5Subscription, customer uuid.UUID, price *models.Price) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	scope, found, err := im.Merchants.PSPScopeByID(ctx, mid, pspID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("adopt: PSP %s is not declared", pspID)
	}
	st, err := (&nmiSource{currency: "USD"}).subscription(sub, nmi.RecurringReportRow{})
	if err != nil {
		return err
	}
	data := st.data.(subscriptionData)
	if price == nil || data.Plan == "" {
		return fmt.Errorf("adopt NMI subscription %s: no price for its plan", st.ref)
	}
	terms := priceTerms{id: price.PublicID, amount: price.Amount, currency: price.Currency}
	if price.BillingIntervalHours != nil {
		terms.hours = *price.BillingIntervalHours
	}
	if price.BillingIntervalMonths != nil {
		terms.months = *price.BillingIntervalMonths
	}
	prices := map[string]priceTerms{data.Plan: terms}
	methods, err := im.DB.Gen(ctx).ListCustomerVaultMethods(ctx, gen.ListCustomerVaultMethodsParams{MerchantID: mid.UUID(), PspID: pspID, Vault: st.customerRef, CustomerID: customer})
	if err != nil {
		return err
	}
	var cards customerData
	for i, m := range methods {
		if m != nil {
			cards.Cards = append(cards.Cards, cardData{MethodRef: *m, Primary: i == 0})
		}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	record := gen.BillingPspImportRecord{Kind: string(billing.PSPImportSubscription), Ref: st.ref, CustomerRef: &st.customerRef, Data: raw, State: string(billing.PSPImportLinked)}
	l := lander{im: im, scope: scope, prices: prices, asOf: im.now()}
	declared, _, reason, err := l.subscription(billing.CustomerID(customer), record, map[string]customerData{st.customerRef: cards}, rebills{})
	if err != nil {
		return err
	}
	if reason != "" {
		return fmt.Errorf("adopt NMI subscription %s: %s", st.ref, reason)
	}
	declared.SourceID = "nmi-adoption:" + st.ref
	// Its provider history lands with it, as an import lands a pulled one.
	src, err := sources["nmi"](ctx, im, mid, scope)
	if err != nil {
		return err
	}
	since := data.StartedAt.Add(-24 * time.Hour)
	txns, err := reconcile.NMISubscriptionTransactions(ctx, src.(*nmiSource).client, st.ref, since)
	if err != nil {
		return fmt.Errorf("adopt NMI subscription %s: read its history: %w", st.ref, err)
	}
	var history []billing.DeclaredTransaction
	for _, t := range txns {
		// The report was filtered by schedule; most rows omit its id.
		if t.SubscriptionID != "" && t.SubscriptionID != st.ref {
			continue
		}
		var typ string
		switch t.Type {
		case reconcile.TransactionTypeDecline:
			typ = "decline"
		case reconcile.TransactionTypeRefund:
			typ = "refund"
		case reconcile.TransactionTypeSale:
		default:
			continue // authorizations and voids moved no money
		}
		native, err := moneyutil.RailMinorToNative(t.Currency, moneyutil.Cents(t.AmountCents))
		if err != nil {
			return fmt.Errorf("adopt NMI subscription %s: transaction %s: %w", st.ref, t.TransactionID, err)
		}
		declared := billing.DeclaredTransaction{RailSubscriptionID: st.ref, TransactionID: t.TransactionID, OriginalTransactionID: t.OriginalTransactionID,
			Type: typ, Success: t.Success, Amount: native, Currency: strings.ToUpper(t.Currency), OccurredAt: t.OccurredAt.UTC()}
		if t.Success && typ != "decline" {
			// A named receipt is a payment (or its refund) of the subscription.
			declared.SourceID = "nmi-adoption-receipt:" + t.TransactionID + ":" + typ
		}
		history = append(history, declared)
	}
	psp := billing.PSPID(pspID)
	res, err := billingimport.Import(ctx, billingimport.Options{DB: im.DB, MerchantID: mid, Clock: im.Clock, Book: billing.DeclaredBilling{
		AsOf: l.asOf, DefaultPSP: billing.PSPRef{ID: &psp}, Customers: []billing.DeclaredCustomer{{Customer: billing.CustomerID(customer)}},
		Subscriptions: []billing.DeclaredSubscription{declared}, Transactions: history,
	}})
	if err != nil {
		return err
	}
	if slices.Contains(res.Imported, declared.SourceID) || slices.Contains(res.Skipped, declared.SourceID) {
		return nil
	}
	why := res.Reasons[declared.SourceID]
	if strings.Contains(why, "subscriptions_customer_id_product_id_key") || strings.Contains(why, "subscriptions_customer_id_tier_group_key") {
		return fmt.Errorf("adopt NMI subscription %s: %w", st.ref, ErrProductHeld)
	}
	return fmt.Errorf("adopt NMI subscription %s: %s", st.ref, why)
}
