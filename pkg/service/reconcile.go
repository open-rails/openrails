package service

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/modules/reconcile"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// StripeReconcileOptions controls a Stripe backfill run. It mirrors the internal
// reconcile.Options but is re-declared here so embedded hosts (which cannot
// import internal/...) have a public type.
type StripeReconcileOptions struct {
	// Apply writes changes; when false (default) the run only reports.
	Apply bool
	// MaxPages bounds the pages fetched from Stripe (0 = unlimited).
	MaxPages int
	// PageSize bounds the per-request page size (Stripe caps at 100).
	PageSize int
	// SkipPayments runs the subscription reconcile only (no charges/cards pass).
	SkipPayments bool
	// UserExists reports whether a resolved user_id exists in the host's identity
	// system. When set, any remote subscription/charge resolving to an unknown
	// user is skipped (no billing rows written) and reported. OpenRails owns no
	// users table, so the host injects this; cozy-art always wires it, making the
	// skip unconditional. Nil = no identity source (legacy behavior).
	UserExists func(ctx context.Context, userID string) (bool, error)
}

// StripeReconcileReport summarizes what a Stripe backfill observed and wrote.
type StripeReconcileReport = reconcile.Report

// ReconcileStripe runs the Stripe backfill in-process against the embedded
// runtime. It pages Stripe subscriptions + charges, mirrors them into the local
// DB (memberships, payments, card snapshots, payment methods), and returns a
// report. It NEVER charges anything and is idempotent: dry-run by default, with
// every write being create-if-not-exists / upsert / link-if-changed.
//
// This is the public entry point embedded hosts (e.g. cozy-art) call:
//
//	svc, _ := billing.Service()
//	report, err := svc.ReconcileStripe(ctx, service.StripeReconcileOptions{Apply: true})
//
// The host needs nothing beyond a configured Stripe secret key (read from the
// runtime config); the Stripe listers are built internally.
func (s *Service) ReconcileStripe(ctx context.Context, opts StripeReconcileOptions) (*StripeReconcileReport, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.Config == nil {
		return nil, fmt.Errorf("billing service: config unavailable for stripe reconcile")
	}

	subLister, err := subscriptions.NewStripeSubscriptionLister(rt.Config)
	if err != nil {
		return nil, fmt.Errorf("stripe subscription lister init failed: %w", err)
	}

	var chargeLister subscriptions.StripeChargeLister
	if !opts.SkipPayments {
		cl, err := subscriptions.NewStripeChargeLister(rt.Config)
		if err != nil {
			return nil, fmt.Errorf("stripe charge lister init failed: %w", err)
		}
		chargeLister = cl
	}

	return reconcile.Run(ctx, rt, subLister, chargeLister, reconcile.Options{
		Apply:        opts.Apply,
		MaxPages:     opts.MaxPages,
		PageSize:     opts.PageSize,
		SkipPayments: opts.SkipPayments,
		UserExists:   opts.UserExists,
	})
}
