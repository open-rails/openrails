package service

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/internal/identity"
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
	//
	// Takes precedence over Users below when both are set.
	UserExists func(ctx context.Context, userID string) (bool, error)

	// Users is an optional host-provided UserDirectory. When UserExists is nil
	// and Users is non-nil, Users.Exists supplies the unknown-user check. This is
	// the first-class injection point for hosts that already have a UserDirectory
	// (e.g. the profiles-backed default). When both are nil there is no identity
	// source and the reconcile treats every user as existing (legacy behavior) -
	// it never panics or hard-fails.
	Users identity.UserDirectory
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
//	svc, _ := openrails.Service()
//	report, err := svc.ReconcileStripe(ctx, service.StripeReconcileOptions{Apply: true})
//
// The host needs nothing beyond a configured Stripe secret key (read from the
// runtime rail set); the Stripe listers are built internally.
func (s *Service) ReconcileStripe(ctx context.Context, opts StripeReconcileOptions) (*StripeReconcileReport, error) {
	rt, err := s.runtime()
	if err != nil {
		return nil, err
	}
	if rt.Config == nil {
		return nil, fmt.Errorf("billing service: config unavailable for stripe reconcile")
	}

	subLister, err := subscriptions.NewStripeSubscriptionLister(rt.Rails)
	if err != nil {
		return nil, fmt.Errorf("stripe subscription lister init failed: %w", err)
	}

	var chargeLister subscriptions.StripeChargeLister
	if !opts.SkipPayments {
		cl, err := subscriptions.NewStripeChargeLister(rt.Rails)
		if err != nil {
			return nil, fmt.Errorf("stripe charge lister init failed: %w", err)
		}
		chargeLister = cl
	}

	// Resolve the unknown-user check: an explicit UserExists func wins; otherwise
	// fall back to a host-provided UserDirectory; otherwise nil (legacy: treat
	// every user as existing). Never panics on a nil directory.
	userExists := opts.UserExists
	if userExists == nil && opts.Users != nil {
		dir := opts.Users
		userExists = func(ctx context.Context, userID string) (bool, error) {
			return dir.Exists(ctx, userID)
		}
	}

	return reconcile.Run(ctx, rt, subLister, chargeLister, reconcile.Options{
		Apply:        opts.Apply,
		MaxPages:     opts.MaxPages,
		PageSize:     opts.PageSize,
		SkipPayments: opts.SkipPayments,
		UserExists:   userExists,
	})
}
