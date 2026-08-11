package service

import (
	"context"
	"fmt"
	"time"

	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/pkg/identity"
)

// or#908 business-profile facade: B2B onboarding as a first-class record.
// Posture doctrine: business posture is a consequence of onboarding, never a
// settable flag — the record's presence IS the posture, and offboarding is
// refused while the payer owes. See money/business_profile.go.

// ErrBusinessOutstandingBalance is returned by OffboardBusinessCustomer while
// the payer still owes.
var ErrBusinessOutstandingBalance = money.ErrBusinessOutstandingBalance

// BusinessOnboardingDTO is the operator onboarding/update payload: the
// terms/KYC gate plus the invoice profile and credit line written through in
// the same call.
type BusinessOnboardingDTO struct {
	TermsVersion          string            `json:"terms_version"`
	TermsAcceptedAt       time.Time         `json:"terms_accepted_at"`
	TermsAcceptedBy       string            `json:"terms_accepted_by,omitempty"`
	KYCReference          string            `json:"kyc_reference,omitempty"`
	Currency              string            `json:"currency"`
	BudgetAlertThresholds []int64           `json:"budget_alert_thresholds,omitempty"`
	InvoiceProfile        InvoiceProfileDTO `json:"invoice_profile"`
	CreditLimit           int64             `json:"credit_limit"`
}

// BusinessProfileDTO is the composed business standing: the onboarding record
// plus the sibling records one read should return together (invoice profile,
// credit line, live arrears exposure).
type BusinessProfileDTO struct {
	CustomerID            string    `json:"customer_id"`
	TermsVersion          string    `json:"terms_version"`
	TermsAcceptedAt       time.Time `json:"terms_accepted_at"`
	TermsAcceptedBy       string    `json:"terms_accepted_by,omitempty"`
	KYCReference          string    `json:"kyc_reference,omitempty"`
	Currency              string    `json:"currency"`
	BudgetAlertThresholds []int64   `json:"budget_alert_thresholds"`
	// SuspensionRecommendedAt / SuspensionReason are the or#910 dunning
	// cycle's open suspension-RECOMMENDATION episode (nil = none). A signal:
	// the host enforces, OpenRails never revokes access.
	SuspensionRecommendedAt *time.Time         `json:"suspension_recommended_at,omitempty"`
	SuspensionReason        string             `json:"suspension_reason,omitempty"`
	InvoiceProfile          *InvoiceProfileDTO `json:"invoice_profile,omitempty"`
	CreditLimit             int64              `json:"credit_limit"`
	OutstandingOwed         int64              `json:"outstanding_owed"`
	CreatedAt               time.Time          `json:"created_at"`
	UpdatedAt               time.Time          `json:"updated_at"`
}

// OnboardBusinessCustomer grants (or updates) a payer's business standing.
// Idempotent. Operator surface — never self-serve.
func (s *Service) OnboardBusinessCustomer(ctx context.Context, payer identity.CustomerID, in BusinessOnboardingDTO) (*BusinessProfileDTO, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	contacts := make([]models.InvoiceContact, 0, len(in.InvoiceProfile.BillingContacts))
	for _, c := range in.InvoiceProfile.BillingContacts {
		contacts = append(contacts, models.InvoiceContact{Name: c.Name, Email: c.Email})
	}
	if _, err := s.moneyService().OnboardBusinessCustomer(ctx, payer, money.BusinessOnboarding{
		TermsVersion:          in.TermsVersion,
		TermsAcceptedAt:       in.TermsAcceptedAt,
		TermsAcceptedBy:       in.TermsAcceptedBy,
		KYCReference:          in.KYCReference,
		Currency:              in.Currency,
		BudgetAlertThresholds: in.BudgetAlertThresholds,
		InvoiceProfile: money.CustomerInvoiceProfile{
			NetTermsDays:     in.InvoiceProfile.NetTermsDays,
			CollectionMethod: in.InvoiceProfile.CollectionMethod,
			PONumber:         in.InvoiceProfile.PONumber,
			Tax:              in.InvoiceProfile.Tax,
			BillingContacts:  contacts,
			Memo:             in.InvoiceProfile.Memo,
		},
		CreditLimit: in.CreditLimit,
	}); err != nil {
		return nil, err
	}
	return s.getBusinessProfileComposed(ctx, payer)
}

// GetBusinessProfile returns a payer's composed business standing; nil when
// the payer has no business profile (consumer posture).
func (s *Service) GetBusinessProfile(ctx context.Context, payer identity.CustomerID) (*BusinessProfileDTO, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	return s.getBusinessProfileComposed(ctx, payer)
}

func (s *Service) getBusinessProfileComposed(ctx context.Context, payer identity.CustomerID) (*BusinessProfileDTO, error) {
	p, err := s.moneyService().GetBusinessProfile(ctx, payer)
	if err != nil || p == nil {
		return nil, err
	}
	dto := BusinessProfileDTO{
		CustomerID:              p.CustomerID.String(),
		TermsVersion:            p.TermsVersion,
		TermsAcceptedAt:         p.TermsAcceptedAt,
		TermsAcceptedBy:         p.TermsAcceptedBy,
		KYCReference:            p.KYCReference,
		Currency:                p.Currency,
		BudgetAlertThresholds:   p.BudgetAlertThresholds,
		SuspensionRecommendedAt: p.SuspensionRecommendedAt,
		SuspensionReason:        p.SuspensionReason,
		CreatedAt:               p.CreatedAt,
		UpdatedAt:               p.UpdatedAt,
	}
	if ip, err := s.moneyService().GetCustomerInvoiceProfile(ctx, payer); err == nil && ip != nil {
		dto.InvoiceProfile = &InvoiceProfileDTO{
			NetTermsDays:     ip.NetTermsDays,
			CollectionMethod: ip.CollectionMethod,
			PONumber:         ip.PONumber,
			Tax:              ip.Tax,
			BillingContacts:  contactsToDTO(ip.BillingContacts),
			Memo:             ip.Memo,
		}
	}
	if limit, err := s.moneyService().GetCreditLimit(ctx, payer, p.Currency); err == nil {
		dto.CreditLimit = limit
	}
	if owed, err := s.moneyService().GetOutstandingOwed(ctx, payer, p.Currency); err == nil {
		dto.OutstandingOwed = owed
	}
	return &dto, nil
}

// ListBusinessCustomers returns the merchant's business roster (composed
// records not included — one row per onboarded payer).
func (s *Service) ListBusinessCustomers(ctx context.Context, limit int) ([]BusinessProfileDTO, error) {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return nil, pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return nil, fmt.Errorf("service not initialized")
	}
	rows, err := s.moneyService().ListBusinessProfiles(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]BusinessProfileDTO, 0, len(rows))
	for _, p := range rows {
		out = append(out, BusinessProfileDTO{
			CustomerID:              p.CustomerID.String(),
			TermsVersion:            p.TermsVersion,
			TermsAcceptedAt:         p.TermsAcceptedAt,
			TermsAcceptedBy:         p.TermsAcceptedBy,
			KYCReference:            p.KYCReference,
			Currency:                p.Currency,
			BudgetAlertThresholds:   p.BudgetAlertThresholds,
			SuspensionRecommendedAt: p.SuspensionRecommendedAt,
			SuspensionReason:        p.SuspensionReason,
			CreatedAt:               p.CreatedAt,
			UpdatedAt:               p.UpdatedAt,
		})
	}
	return out, nil
}

// OffboardBusinessCustomer withdraws business standing. Refused with
// ErrBusinessOutstandingBalance while the payer owes; a no-op for a payer
// that was never onboarded.
func (s *Service) OffboardBusinessCustomer(ctx context.Context, payer identity.CustomerID) error {
	ctx, release, pinErr := s.pin(ctx)
	if pinErr != nil {
		return pinErr
	}
	defer release()

	if s == nil || s.rt == nil {
		return fmt.Errorf("service not initialized")
	}
	return s.moneyService().OffboardBusinessCustomer(ctx, payer)
}
