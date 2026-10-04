package openrails

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
)

// PaymentProviderClient shares the same authorized implementation over local and
// remote transport. External HTTP exposure is a server construction decision.
type PaymentProviderClient struct{ client *Client }

// List returns the merchant's PSPs, optionally only those on one rail
// (params.Provider), in one environment or with one status, with the rails'
// definitions. Credential values are never returned.
func (p *PaymentProviderClient) List(ctx context.Context, params *billing.PaymentProviderListParams, requestOptions ...RequestOption) (*billing.PaymentProviderList, error) {
	query := url.Values{}
	if params != nil {
		for key, value := range map[string]string{"provider": params.Provider, "environment": params.Environment, "status": params.Status} {
			if value != "" {
				query.Set(key, value)
			}
		}
	}
	var out billing.PaymentProviderList
	if err := p.client.do(ctx, http.MethodGet, "/v1/merchant/payment-providers?"+query.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// Retrieve reads the merchant's active PSP on a rail; provider is the rail
// (nmi, ccbill, stripe, solana). Several active accounts on the rail are a
// conflict.
func (p *PaymentProviderClient) Retrieve(ctx context.Context, provider string, requestOptions ...RequestOption) (*billing.PaymentProviderConfig, error) {
	return p.request(ctx, http.MethodGet, provider, "", nil, requestOptions...)
}

// Upsert creates or changes the merchant's PSP on a rail; provider is the
// rail. Credentials are write-only. Retry with the same OperationID and
// payload.
func (p *PaymentProviderClient) Upsert(ctx context.Context, provider string, params *billing.UpsertPaymentProviderParams, requestOptions ...RequestOption) (*billing.PaymentProviderConfig, error) {
	if params == nil {
		return nil, fmt.Errorf("payment provider configuration is required")
	}
	return p.request(ctx, http.MethodPut, provider, "", params, requestOptions...)
}

// Archive archives one account on a rail: it takes no new work and keeps its
// existing obligations.
func (p *PaymentProviderClient) Archive(ctx context.Context, provider string, accountID uuid.UUID, params *billing.ArchivePaymentProviderAccountParams, requestOptions ...RequestOption) (*billing.PaymentProviderConfig, error) {
	if accountID == uuid.Nil {
		return nil, fmt.Errorf("payment provider account ID is required")
	}
	return p.request(ctx, http.MethodPost, provider, "/accounts/"+accountID.String()+"/archive", params, requestOptions...)
}

func (p *PaymentProviderClient) request(ctx context.Context, method, provider, suffix string, params any, requestOptions ...RequestOption) (*billing.PaymentProviderConfig, error) {
	provider, err := pathID("provider", provider)
	if err != nil {
		return nil, err
	}
	var out struct {
		Provider billing.PaymentProviderConfig `json:"payment_provider"`
	}
	if err := p.client.do(ctx, method, "/v1/merchant/payment-providers/"+provider+suffix, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out.Provider, nil
}
