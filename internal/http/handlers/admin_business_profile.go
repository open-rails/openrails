package handlers

import (
	"errors"
	"net/http"
	"time"

	httprequest "github.com/open-rails/openrails/internal/http/request"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

// or#908 merchant-admin business-profile CRUD. Posture doctrine: business
// posture is a consequence of onboarding, never a settable flag — there is no
// "set posture" route; PUT onboards (terms acceptance required) and DELETE
// offboards (refused while the payer owes).

// adminBusinessOnboardRequest is the PUT body: the terms/KYC gate plus the
// invoice profile and credit line written through in the same call.
type adminBusinessOnboardRequest struct {
	TermsVersion          string                           `json:"terms_version" binding:"required"`
	TermsAcceptedAt       time.Time                        `json:"terms_accepted_at" binding:"required"`
	TermsAcceptedBy       string                           `json:"terms_accepted_by"`
	KYCReference          string                           `json:"kyc_reference"`
	Currency              string                           `json:"currency" binding:"required"`
	BudgetAlertThresholds []int64                          `json:"budget_alert_thresholds"`
	InvoiceProfile        billingservice.InvoiceProfileDTO `json:"invoice_profile"`
	CreditLimit           int64                            `json:"credit_limit"`
}

// PutAdminBusinessProfile is PUT /v1/merchant/customers/{customer_id}/business-profile:
// onboard (or update) a payer's business standing. Idempotent.
func PutAdminBusinessProfile(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	var req adminBusinessOnboardRequest
	if !r.BindJSON(&req) {
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	profile, err := svc.OnboardBusinessCustomer(r.Request.Context(), *payer, billingservice.BusinessOnboardingDTO{
		TermsVersion:          req.TermsVersion,
		TermsAcceptedAt:       req.TermsAcceptedAt,
		TermsAcceptedBy:       req.TermsAcceptedBy,
		KYCReference:          req.KYCReference,
		Currency:              req.Currency,
		BudgetAlertThresholds: req.BudgetAlertThresholds,
		InvoiceProfile:        req.InvoiceProfile,
		CreditLimit:           req.CreditLimit,
	})
	if err != nil {
		// Validation refusals (terms gate, currency, thresholds, contacts) are
		// client errors; nothing here touches a provider.
		r.ErrorJSON(http.StatusBadRequest, err.Error())
		return
	}
	r.SuccessJSON(profile)
}

// GetAdminBusinessProfile is GET /v1/merchant/customers/{customer_id}/business-profile.
// 404 = consumer posture: no onboarding record exists.
func GetAdminBusinessProfile(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	profile, err := svc.GetBusinessProfile(r.Request.Context(), *payer)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to read business profile")
		return
	}
	if profile == nil {
		r.ErrorJSON(http.StatusNotFound, "no business profile (consumer posture)")
		return
	}
	r.SuccessJSON(profile)
}

// ListAdminBusinessProfiles is GET /v1/merchant/business-customers: the
// merchant's onboarded business roster, oldest first.
func ListAdminBusinessProfiles(r *httprequest.Request) {
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	rows, err := svc.ListBusinessCustomers(r.Request.Context(), 0)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "failed to list business customers")
		return
	}
	r.SuccessJSON(rows)
}

// DeleteAdminBusinessProfile is DELETE /v1/merchant/customers/{customer_id}/business-profile:
// offboard back to consumer posture. 409 while the payer still owes — an
// offboarded debtor would hide its receivable from the arrears cycle.
func DeleteAdminBusinessProfile(r *httprequest.Request) {
	payer, err := parseServiceCustomerID(r.Param("customer_id"))
	if err != nil || payer == nil {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	svc, err := billingservice.New(r.State)
	if err != nil {
		r.ErrorJSON(http.StatusInternalServerError, "billing service unavailable")
		return
	}
	if err := svc.OffboardBusinessCustomer(r.Request.Context(), *payer); err != nil {
		if errors.Is(err, billingservice.ErrBusinessOutstandingBalance) {
			r.ErrorJSON(http.StatusConflict, err.Error())
			return
		}
		r.ErrorJSON(http.StatusInternalServerError, "offboard failed")
		return
	}
	r.SuccessJSONMessage("business profile removed (consumer posture)")
}
