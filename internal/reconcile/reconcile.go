// Package reconcile compares local OpenRails billing state against the payment
// rails as the source of truth (#107). It is the sibling of internal/audit:
// audit checks internal DB consistency, reconcile checks local-vs-rail
// drift.
//
// This file defines phase 1 of #107: the per-provider RailFetcher
// interface and the normalized RemoteSnapshot model every fetcher produces.
// The diff engine, findings persistence, enforce appliers, and admin surfaces
// are phase 2 and deliberately absent.
//
// HARD CONSTRAINT: fetchers are read-only by construction. They are built
// exclusively on query/report/list APIs (NMI query.php, CCBill DataLink
// exports, Stripe GETs through the stripeapi read-only choke point, Solana RPC
// account reads) and never perform a rail mutation.
package reconcile

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Provider identifies the payment rail a snapshot came from. Values match
// the rail names used elsewhere in OpenRails (models.Rail*).
type Provider string

const (
	ProviderNMI    Provider = "nmi"
	ProviderCCBill Provider = "ccbill"
	ProviderStripe Provider = "stripe"
	ProviderSolana Provider = "solana"
)

// SubscriptionStatus is the normalized cross-provider subscription state. The
// provider's literal status string is always preserved alongside it in
// RemoteSubscription.RawStatus for forensics; the normalized value is what the
// phase-2 diff engine compares against local state.
type SubscriptionStatus string

const (
	SubscriptionStatusActive    SubscriptionStatus = "active"
	SubscriptionStatusCancelled SubscriptionStatus = "cancelled"
	SubscriptionStatusExpired   SubscriptionStatus = "expired"
	SubscriptionStatusPastDue   SubscriptionStatus = "past_due"
	SubscriptionStatusUnknown   SubscriptionStatus = "unknown"
)

// TransactionType is the normalized cross-provider transaction kind.
type TransactionType string

const (
	TransactionTypeSale       TransactionType = "sale"
	TransactionTypeAuth       TransactionType = "auth"
	TransactionTypeRefund     TransactionType = "refund"
	TransactionTypeChargeback TransactionType = "chargeback"
	TransactionTypeDecline    TransactionType = "decline"
)

// Capabilities declares which data classes a provider's fetcher can supply.
// The phase-2 diff engine only runs the checks the provider can answer (e.g.
// no PS-6 chargeback checks against a provider with Chargebacks=false).
type Capabilities struct {
	Subscriptions bool `json:"subscriptions"`
	Transactions  bool `json:"transactions"`
	Refunds       bool `json:"refunds"`
	Chargebacks   bool `json:"chargebacks"`
	Vault         bool `json:"vault"`
}

// RemoteSubscription is one subscription as the rail declares it.
type RemoteSubscription struct {
	// RailSubscriptionID is the rail-side subscription identifier
	// (NMI subscription_id, CCBill subscription id, Stripe sub_..., Solana
	// subscription PDA). Matches local subscriptions.rail_subscription_id.
	RailSubscriptionID string `json:"rail_subscription_id"`
	// Status is the normalized state; RawStatus preserves the provider's
	// literal status string (empty when the provider has no status field and
	// the status was inferred — see each fetcher's notes).
	Status    SubscriptionStatus `json:"status"`
	RawStatus string             `json:"raw_status,omitempty"`
	// CustomerID is the rail's customer/vault identifier when the
	// provider has one (NMI customer_vault_id, Stripe cus_..., Solana
	// subscriber wallet). Empty for CCBill (no vault concept exposed).
	CustomerID string `json:"customer_id,omitempty"`
	// Email / Username are identity fallbacks for PS-1/bootstrap matching,
	// populated when the provider exposes them.
	Email    string `json:"email,omitempty"`
	Username string `json:"username,omitempty"`
	// PlanID is the rail-side plan/price identifier (NMI plan_id, Stripe
	// price id, Solana plan PDA). Maps to local prices via provider_links.
	PlanID string `json:"plan_id,omitempty"`
	// NextBillingAt / LastBilledAt are the rail's declared billing
	// timestamps, nil when unknown.
	NextBillingAt *time.Time `json:"next_billing_at,omitempty"`
	LastBilledAt  *time.Time `json:"last_billed_at,omitempty"`
	// AmountCents is the recurring charge amount in integer cents of Currency.
	// Zero when the provider does not denominate in fiat (Solana on-chain
	// amounts are mint base units and live in Raw instead).
	AmountCents int64  `json:"amount_cents"`
	Currency    string `json:"currency,omitempty"`
	// Raw preserves the provider's record for forensics (original XML element,
	// CSV row, JSON object, or decoded account state).
	Raw json.RawMessage `json:"raw,omitempty"`
}

// RemoteTransaction is one charge-level event as the rail declares it.
type RemoteTransaction struct {
	// TransactionID is the rail transaction identifier (NMI
	// transaction_id, CCBill transaction id, Stripe ch_/re_/dp_..., Solana tx
	// signature).
	TransactionID string `json:"transaction_id"`
	// SubscriptionID links to the rail subscription when the provider
	// exposes the linkage; empty otherwise (see fetcher notes).
	SubscriptionID string          `json:"subscription_id,omitempty"`
	Type           TransactionType `json:"type"`
	Success        bool            `json:"success"`
	// AmountCents is in integer cents of Currency. Zero for Solana (base
	// units, preserved in Raw).
	AmountCents int64     `json:"amount_cents"`
	Currency    string    `json:"currency,omitempty"`
	OccurredAt  time.Time `json:"occurred_at"`
	// DeclineReason carries the rail's failure/decline text for declined
	// attempts; it is the raw material for the dunning-forensics report.
	DeclineReason string          `json:"decline_reason,omitempty"`
	Raw           json.RawMessage `json:"raw,omitempty"`
}

// RemoteVaultEntry is one stored payment method as the rail declares it.
type RemoteVaultEntry struct {
	// CustomerVaultID is the rail vault/customer identifier.
	CustomerVaultID string `json:"customer_vault_id"`
	// CardLast4 / CardExpiry (MMYY) are populated when the provider exposes
	// masked card data.
	CardLast4  string          `json:"card_last4,omitempty"`
	CardExpiry string          `json:"card_expiry,omitempty"`
	Email      string          `json:"email,omitempty"`
	Raw        json.RawMessage `json:"raw,omitempty"`
}

// RemoteSnapshot is the normalized result of one fetch against one provider.
type RemoteSnapshot struct {
	Provider          Provider             `json:"provider"`
	ProviderAccountID string               `json:"provider_account_id,omitempty"`
	FetchedAt         time.Time            `json:"fetched_at"`
	Subscriptions     []RemoteSubscription `json:"subscriptions"`
	Transactions      []RemoteTransaction  `json:"transactions"`
	VaultEntries      []RemoteVaultEntry   `json:"vault_entries"`
	Capabilities      Capabilities         `json:"capabilities"`
	Coverage          SnapshotCoverage     `json:"coverage"`
}

// SnapshotCoverage says which provider domains were exhaustively covered by a
// fetch. Prune must use this contract rather than inferring completeness from
// non-empty arrays: transaction absence only means anything inside the exact
// covered window, and only after all provider pages have been read.
type SnapshotCoverage struct {
	SubscriptionsExhaustive       bool       `json:"subscriptions_exhaustive,omitempty"`
	TransactionsExhaustive        bool       `json:"transactions_exhaustive,omitempty"`
	TransactionsPaginatedComplete bool       `json:"transactions_paginated_complete,omitempty"`
	TransactionWindowSince        *time.Time `json:"transaction_window_since,omitempty"`
	TransactionWindowUntil        *time.Time `json:"transaction_window_until,omitempty"`
}

func (c SnapshotCoverage) CanPruneSubscriptions() bool {
	return c.SubscriptionsExhaustive
}

func (c SnapshotCoverage) CanPruneTransactions() bool {
	return c.TransactionsExhaustive &&
		c.TransactionsPaginatedComplete &&
		c.TransactionWindowSince != nil &&
		c.TransactionWindowUntil != nil
}

// FetchParams bounds a fetch. Since/Until constrain the transaction window
// (providers that cannot date-filter a data class document the behavior on
// their fetcher). SubscriptionID / CustomerID narrow the fetch to one
// subscription or customer where the provider supports server-side filtering;
// fetchers that cannot filter a class server-side return the full set and
// leave narrowing to the caller.
type FetchParams struct {
	Since          time.Time
	Until          time.Time
	SubscriptionID string
	CustomerID     string
	// ProviderAccountID is the OpenRails provider_accounts.id being pulled.
	// Fetchers do not usually need it for API calls, but it is carried for
	// evidence and for account-bound provider-pull wiring.
	ProviderAccountID string
	ProviderType      string
	AccountID         string
}

// RailFetcher pulls a provider's declared state into a RemoteSnapshot.
// Implementations MUST be read-only: query/report/list APIs only, never a
// mutation.
type RailFetcher interface {
	Name() string
	Capabilities() Capabilities
	Fetch(ctx context.Context, params FetchParams) (*RemoteSnapshot, error)
}

// ProviderKeyer is implemented by fetchers/wrappers that know which configured
// provider key backs the provider type. NMI is the important case: the local
// provider is "nmi", but the configured key may be "mobius" or another NMI MID.
type ProviderKeyer interface {
	ProviderKey() string
}

type keyedFetcher struct {
	RailFetcher
	key string
}

func (f keyedFetcher) ProviderKey() string { return f.key }

// ProviderKeyFor returns the configured provider key behind a fetcher.
func ProviderKeyFor(provider Provider, fetcher RailFetcher) string {
	if k, ok := fetcher.(ProviderKeyer); ok {
		if key := strings.ToLower(strings.TrimSpace(k.ProviderKey())); key != "" {
			return key
		}
	}
	return string(provider)
}

// rawJSON marshals v into a json.RawMessage for the Raw forensics fields,
// falling back to a JSON-encoded error note rather than failing the fetch.
func rawJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(map[string]string{"raw_encode_error": err.Error()})
	}
	return b
}

// parseAmountCents converts a decimal money string (e.g. "9.99", "23", "-5.00")
// into integer cents without floating point. Empty strings parse to 0. More
// than two fraction digits is an error (no silent truncation of money).
func parseAmountCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	neg := false
	switch s[0] {
	case '-':
		neg = true
		s = s[1:]
	case '+':
		s = s[1:]
	}
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	switch len(frac) {
	case 0:
		frac = "00"
	case 1:
		frac += "0"
	case 2:
	default:
		return 0, fmt.Errorf("amount %q has more than two fraction digits", s)
	}
	var cents int64
	for _, r := range whole + frac {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("invalid amount %q", s)
		}
		cents = cents*10 + int64(r-'0')
	}
	if neg {
		cents = -cents
	}
	return cents, nil
}
