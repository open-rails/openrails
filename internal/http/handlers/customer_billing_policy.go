package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"

	"github.com/open-rails/openrails"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/service"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

type customerBillingPolicyWrite struct {
	PolicyName json.RawMessage `json:"policy_name"`
}

func (body *customerBillingPolicyWrite) UnmarshalJSON(raw []byte) error {
	type declared customerBillingPolicyWrite
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode((*declared)(body))
}

func ServiceGetCustomerBillingPolicy(r *httprequest.Request) {
	customer, err := openrails.ParseCustomerID(r.Param("customer_id"))
	if err != nil || customer.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	svc, err := service.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	out, err := svc.GetCustomerBillingPolicy(r.Request.Context(), customer)
	if err != nil {
		writeRefusal(r, err, "customer policy lookup failed")
		return
	}
	r.SuccessJSON(out)
}

func ServiceSetCustomerBillingPolicy(r *httprequest.Request) {
	customer, err := openrails.ParseCustomerID(r.Param("customer_id"))
	if err != nil || customer.IsZero() {
		r.ErrorJSON(http.StatusBadRequest, "invalid customer_id")
		return
	}
	var body customerBillingPolicyWrite
	if !r.BindJSON(&body) {
		return
	}
	var policyName *string
	if len(body.PolicyName) == 0 || json.Unmarshal(body.PolicyName, &policyName) != nil {
		writeRefusal(r, apperr.Invalidf("policy_name is required and must be a string or null").WithParam("policy_name"), "invalid customer policy")
		return
	}
	svc, err := service.New(r.State)
	if err != nil {
		r.InternalError("billing service unavailable", err)
		return
	}
	out, err := svc.SetCustomerBillingPolicy(r.Request.Context(), customer, policyName)
	if err != nil {
		writeRefusal(r, err, "customer policy assignment failed")
		return
	}
	r.SuccessJSON(out)
}
