package service

import (
	"context"
	"errors"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

var (
	ErrBillingPolicyCustomerNotFound = apperr.New(http.StatusNotFound, "customer_not_found", "customer not found")
	ErrBillingPolicyNotFound         = apperr.New(http.StatusNotFound, "billing_policy_not_found", "billing policy not found")
)

// GetCustomerBillingPolicy returns only the customer's explicit runtime assignment.
func (s *Service) GetCustomerBillingPolicy(ctx context.Context, customerID openrails.CustomerID) (*openrails.CustomerBillingPolicyAssignment, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if customerID.IsZero() {
		return nil, apperr.Invalidf("customer_id is required").WithParam("customer_id")
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.rt.DB.Gen(ctx).GetCustomerBillingPolicyAssignment(ctx, gen.GetCustomerBillingPolicyAssignmentParams{MerchantID: mid.UUID(), CustomerID: customerID.UUID()})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrBillingPolicyCustomerNotFound
	}
	if err != nil {
		return nil, err
	}
	return &openrails.CustomerBillingPolicyAssignment{CustomerID: openrails.CustomerID(row.CustomerID).String(), PolicyName: row.PolicyName}, nil
}

// SetCustomerBillingPolicy sets or clears one assignment, preserving every
// declaration and other customer. Unknown customers are never materialized.
func (s *Service) SetCustomerBillingPolicy(ctx context.Context, customerID openrails.CustomerID, policyName *string) (*openrails.CustomerBillingPolicyAssignment, error) {
	ctx, release, err := s.pin(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if customerID.IsZero() {
		return nil, apperr.Invalidf("customer_id is required").WithParam("customer_id")
	}
	var normalized *string
	if policyName != nil {
		name, err := merchantconfig.NormalizeBillingPolicyName(*policyName)
		if err != nil {
			return nil, apperr.Invalidf("%s", err).WithParam("policy_name")
		}
		normalized = &name
	}
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	err = s.rt.DB.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		// Share the declaration lock before touching bindings. Different
		// customers may update concurrently, while declaration replacement
		// cannot remove a policy between its validation and this assignment.
		if _, err := q.ReadMerchantSettingsLock(ctx, mid.UUID()); err != nil {
			return err
		}
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{ID: customerID.UUID(), MerchantID: mid.UUID()}); errors.Is(err, pgx.ErrNoRows) {
			return ErrBillingPolicyCustomerNotFound
		} else if err != nil {
			return err
		}
		id := customerID.UUID()
		if normalized == nil {
			return q.DeleteCustomerBillingPolicyBinding(ctx, gen.DeleteCustomerBillingPolicyBindingParams{MerchantID: mid.UUID(), CustomerID: &id})
		}
		if _, err := q.LockBillingPolicyName(ctx, gen.LockBillingPolicyNameParams{MerchantID: mid.UUID(), Name: *normalized}); errors.Is(err, pgx.ErrNoRows) {
			return ErrBillingPolicyNotFound
		} else if err != nil {
			return err
		}
		now := s.rt.Clock.Now().UTC()
		return q.UpsertBillingPolicyBindingCustomer(ctx, gen.UpsertBillingPolicyBindingCustomerParams{
			ID: uuidutil.NewV7(), MerchantID: mid.UUID(), CustomerID: &id, PolicyName: *normalized, CreatedAt: now, UpdatedAt: now,
		})
	})
	if err != nil {
		return nil, err
	}
	return &openrails.CustomerBillingPolicyAssignment{CustomerID: customerID.String(), PolicyName: normalized}, nil
}
