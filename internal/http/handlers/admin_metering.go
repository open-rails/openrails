package handlers

import (
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/config"
	httprequest "github.com/open-rails/openrails/internal/http/request"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/pkg/api"
	"github.com/open-rails/openrails/pkg/pricing"
	billingservice "github.com/open-rails/openrails/pkg/service"
)

type adminUsageMeterResponse struct {
	billingservice.UsageMeterDTO
	ConfigurationSource string `json:"configuration_source"`
	WritesAllowed       bool   `json:"writes_allowed"`
}

type adminUsageMeterRequest struct {
	EventType     string            `json:"event_type"`
	ValueProperty string            `json:"value_property"`
	Aggregation   string            `json:"aggregation"`
	Unit          string            `json:"unit,omitempty"`
	GroupBy       map[string]string `json:"group_by,omitempty"`
}

type adminDefaultUsageRateCardRequest struct {
	ProductID uuid.UUID           `json:"product_id"`
	Filter    map[string][]string `json:"filter"`
	Price     pricing.RatePrice   `json:"price"`
	Allowance *pricing.Allowance  `json:"allowance,omitempty"`
}

func AdminListUsageMeters(r *httprequest.Request) {
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	page, err := svc.ListUsageMeters(r.Request.Context(), billingservice.PaginationOptions{
		Limit:  parseIntDefault(r.Query("limit"), 50),
		Offset: parseIntDefault(r.Query("offset"), 0),
	})
	if err != nil {
		writeMeteringError(r, err)
		return
	}
	items := make([]adminUsageMeterResponse, 0, len(page.Data))
	for _, meter := range page.Data {
		items = append(items, adminUsageMeterDTO(r, meter))
	}
	r.JSON(http.StatusOK, paginatedResponse[adminUsageMeterResponse]{
		Items:  items,
		Total:  page.TotalItems,
		Limit:  page.Limit,
		Offset: page.Offset,
	})
}

func AdminGetUsageMeter(r *httprequest.Request) {
	meter, ok := loadAdminUsageMeter(r, r.Param("key"))
	if !ok {
		return
	}
	r.SuccessJSON(adminUsageMeterDTO(r, *meter))
}

func AdminListUsageMeterOverrides(r *httprequest.Request) {
	meterKey := pricing.NormalizeKey(r.Param("key"))
	if meterKey == "" {
		r.ErrorJSON(http.StatusBadRequest, "meter key required")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	page, err := svc.ListUsageMeterOverrides(
		r.Request.Context(),
		meterKey,
		billingservice.PaginationOptions{
			Limit:  parseIntDefault(r.Query("limit"), 50),
			Offset: parseIntDefault(r.Query("offset"), 0),
		},
	)
	if err != nil {
		writeMeteringError(r, err)
		return
	}
	r.JSON(http.StatusOK, paginatedResponse[billingservice.UsageMeterOverrideDTO]{
		Items:  page.Data,
		Total:  page.TotalItems,
		Limit:  page.Limit,
		Offset: page.Offset,
	})
}

func AdminPutUsageMeter(r *httprequest.Request) {
	var req adminUsageMeterRequest
	if !r.BindJSON(&req) {
		return
	}
	spec, err := usageMeterSpec(r.Param("key"), req)
	if err != nil {
		writeMeteringValidationError(r, "usage_meter_invalid", err)
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	if err := svc.EnsureUsageMeter(r.Request.Context(), spec); err != nil {
		writeMeteringError(r, err)
		return
	}
	meter, err := svc.GetUsageMeter(r.Request.Context(), spec.Key)
	if err != nil {
		r.InternalError("usage meter stored but read-back failed", err)
		return
	}
	r.SuccessJSON(adminUsageMeterDTO(r, *meter))
}

func AdminPutDefaultUsageRateCard(r *httprequest.Request) {
	var req adminDefaultUsageRateCardRequest
	if !r.BindJSON(&req) {
		return
	}
	meter, ok := loadAdminUsageMeter(r, r.Param("key"))
	if !ok {
		return
	}
	input, err := defaultUsageRateCardInput(*meter, req)
	if err != nil {
		writeMeteringValidationError(r, "usage_rate_card_invalid", err)
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	if err := svc.SetUsageRateCard(r.Request.Context(), input); err != nil {
		writeMeteringError(r, err)
		return
	}
	stored, err := svc.GetUsageMeter(r.Request.Context(), meter.Key)
	if err != nil {
		r.InternalError("usage rate card stored but read-back failed", err)
		return
	}
	r.SuccessJSON(adminUsageMeterDTO(r, *stored))
}

func AdminDeleteDefaultUsageRateCard(r *httprequest.Request) {
	meterKey := pricing.NormalizeKey(r.Param("key"))
	if meterKey == "" {
		r.ErrorJSON(http.StatusBadRequest, "meter key required")
		return
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return
	}
	if err := svc.DeleteDefaultUsageRateCard(r.Request.Context(), meterKey); err != nil {
		writeMeteringError(r, err)
		return
	}
	r.Status(http.StatusNoContent)
}

func usageMeterSpec(pathKey string, req adminUsageMeterRequest) (billingservice.UsageMeterSpec, error) {
	meter := pricing.Meter{
		Key:           pathKey,
		EventType:     req.EventType,
		ValueProperty: req.ValueProperty,
		Aggregation:   req.Aggregation,
		Unit:          req.Unit,
		GroupBy:       req.GroupBy,
	}
	if err := pricing.ValidateMeter("usage meter", &meter); err != nil {
		return billingservice.UsageMeterSpec{}, err
	}
	if !pricing.BillingSupported(meter.Aggregation) {
		return billingservice.UsageMeterSpec{}, errors.New("usage meter aggregation must be sum or count")
	}
	return billingservice.UsageMeterSpec{
		Key:           meter.Key,
		EventType:     meter.EventType,
		ValueProperty: meter.ValueProperty,
		Aggregation:   meter.Aggregation,
		Unit:          meter.Unit,
		GroupBy:       meter.GroupBy,
	}, nil
}

func defaultUsageRateCardInput(
	meter billingservice.UsageMeterDTO,
	req adminDefaultUsageRateCardRequest,
) (billingservice.UsageRateCardInput, error) {
	if req.ProductID == uuid.Nil {
		return billingservice.UsageRateCardInput{}, errors.New("product_id required")
	}
	if err := pricing.ValidateUsagePrice("usage rate card", &req.Price); err != nil {
		return billingservice.UsageRateCardInput{}, err
	}
	if err := moneyutil.ValidateCurrency(req.Price.Currency); err != nil {
		return billingservice.UsageRateCardInput{}, err
	}
	if err := pricing.ValidateFilter("usage rate card", &req.Filter); err != nil {
		return billingservice.UsageRateCardInput{}, err
	}
	if err := pricing.ValidateAllowance("usage rate card", req.Allowance); err != nil {
		return billingservice.UsageRateCardInput{}, err
	}
	if err := pricing.ValidateDimensions("usage rate card", meter.GroupBy, req.Filter, &req.Price); err != nil {
		return billingservice.UsageRateCardInput{}, err
	}
	return billingservice.UsageRateCardInput{
		ProductID: &req.ProductID,
		MeterKey:  meter.Key,
		Filter:    req.Filter,
		Price:     req.Price,
		Allowance: req.Allowance,
	}, nil
}

func loadAdminUsageMeter(r *httprequest.Request, rawKey string) (*billingservice.UsageMeterDTO, bool) {
	meterKey := pricing.NormalizeKey(rawKey)
	if meterKey == "" {
		r.ErrorJSON(http.StatusBadRequest, "meter key required")
		return nil, false
	}
	svc, ok := newAdminBillingService(r)
	if !ok {
		return nil, false
	}
	meter, err := svc.GetUsageMeter(r.Request.Context(), meterKey)
	if err != nil {
		writeMeteringError(r, err)
		return nil, false
	}
	return meter, true
}

func adminUsageMeterDTO(r *httprequest.Request, meter billingservice.UsageMeterDTO) adminUsageMeterResponse {
	source := config.MerchantSourceManifest
	if r.State != nil && r.State.Config != nil {
		source = r.State.Config.MerchantSourceMode()
	}
	return adminUsageMeterResponse{
		UsageMeterDTO:       meter,
		ConfigurationSource: source,
		WritesAllowed:       source == config.MerchantSourceAPI,
	}
}

func writeMeteringValidationError(r *httprequest.Request, code string, err error) {
	r.APIError(api.NewAPIError(
		http.StatusBadRequest,
		api.ErrorTypeInvalidRequest,
		code,
		err.Error(),
	))
}

func writeMeteringError(r *httprequest.Request, err error) {
	var status int
	var code string
	switch {
	case errors.Is(err, billingservice.ErrUsageMeterNotFound):
		status, code = http.StatusNotFound, "usage_meter_not_found"
	case errors.Is(err, billingservice.ErrDefaultRateCardNotFound):
		status, code = http.StatusNotFound, "default_rate_card_not_found"
	case errors.Is(err, billingservice.ErrRateCardProductNotFound):
		status, code = http.StatusNotFound, "rate_card_product_not_found"
	case errors.Is(err, billingservice.ErrAllowanceMeterNotFound):
		status, code = http.StatusNotFound, "allowance_meter_not_found"
	case errors.Is(err, billingservice.ErrMeterInUse):
		status, code = http.StatusConflict, "meter_in_use"
	case errors.Is(err, billingservice.ErrDefaultRateCardRequired):
		status, code = http.StatusConflict, "default_rate_card_required"
	case errors.Is(err, billingservice.ErrRateCardHasOverrides):
		status, code = http.StatusConflict, "rate_card_has_overrides"
	case errors.Is(err, billingservice.ErrRateCardCurrencyMismatch):
		status, code = http.StatusConflict, "rate_card_currency_mismatch"
	default:
		r.InternalError("metering operation failed", err)
		return
	}
	r.APIError(api.NewAPIError(status, api.ErrorTypeInvalidRequest, code, err.Error()))
}
