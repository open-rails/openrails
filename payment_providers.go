package openrails

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

// PaymentProviderCredentialStatus describes a credential without returning its value.
type PaymentProviderCredentialStatus struct {
	Configured      bool       `json:"configured"`
	LastValidatedAt *time.Time `json:"last_validated_at,omitempty"`
	RotationVersion int        `json:"rotation_version,omitempty"`
}

type PaymentProviderConfig struct {
	ID              uuid.UUID                                  `json:"id"`
	Rail            string                                     `json:"rail"`
	Environment     string                                     `json:"environment"`
	AccountID       string                                     `json:"account_id"`
	Archived        bool                                       `json:"archived"`
	Drained         bool                                       `json:"drained"`
	OpenObligations int64                                      `json:"open_obligations"`
	PublicConfig    map[string]string                          `json:"public_config,omitempty"`
	Credentials     map[string]PaymentProviderCredentialStatus `json:"credentials"`
	FirstSeenAt     time.Time                                  `json:"first_seen_at"`
	LastVerifiedAt  *time.Time                                 `json:"last_validated_at,omitempty"`
	ReplacedAt      *time.Time                                 `json:"replaced_at,omitempty"`
	CreatedAt       time.Time                                  `json:"created_at"`
	UpdatedAt       time.Time                                  `json:"updated_at"`
	Revision        int64                                      `json:"revision"`
}

type PaymentProviderDefinition struct {
	Rail           string   `json:"rail"`
	DisplayName    string   `json:"display_name"`
	CredentialKeys []string `json:"credential_keys"`
}

type PaymentProviderListParams struct {
	Provider    string
	Environment string
	Status      string
}

type PaymentProviderList struct {
	Data                []PaymentProviderConfig     `json:"data"`
	ProviderDefinitions []PaymentProviderDefinition `json:"provider_definitions"`
}

// UpsertPaymentProviderParams changes one selected merchant's provider account.
// Credentials are write-only. Retry with the same operation identity and payload;
// the server owns credential storage paths, environment and publication authority.
type UpsertPaymentProviderParams struct {
	OperationID       uuid.UUID         `json:"operation_id"`
	ExpectedRevision  *int64            `json:"expected_revision"`
	Enabled           *bool             `json:"enabled"`
	AccountID         string            `json:"account_id"`
	PublicConfig      map[string]string `json:"public_config"`
	Credentials       map[string]string `json:"credentials"`
	LegacyEnvironment string            `json:"environment,omitempty"`
}

type ArchivePaymentProviderAccountParams struct {
	AllowLast bool `json:"allow_last"`
}

// PaymentProviderClient shares the same authorized implementation over local and
// remote transport. External HTTP exposure is a server construction decision.
type PaymentProviderClient struct{ client *Client }

func (p *PaymentProviderClient) List(ctx context.Context, params *PaymentProviderListParams, requestOptions ...RequestOption) (*PaymentProviderList, error) {
	query := url.Values{}
	if params != nil {
		for key, value := range map[string]string{"provider": params.Provider, "environment": params.Environment, "status": params.Status} {
			if value != "" {
				query.Set(key, value)
			}
		}
	}
	var out PaymentProviderList
	if err := p.client.do(ctx, http.MethodGet, "/v1/merchant/payment-providers?"+query.Encode(), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

func (p *PaymentProviderClient) Retrieve(ctx context.Context, provider string, requestOptions ...RequestOption) (*PaymentProviderConfig, error) {
	return p.request(ctx, http.MethodGet, provider, "", nil, requestOptions...)
}

func (p *PaymentProviderClient) Upsert(ctx context.Context, provider string, params *UpsertPaymentProviderParams, requestOptions ...RequestOption) (*PaymentProviderConfig, error) {
	if params == nil {
		return nil, fmt.Errorf("payment provider configuration is required")
	}
	return p.request(ctx, http.MethodPut, provider, "", params, requestOptions...)
}

// Archive archives a particular account without deleting historical obligations.
func (p *PaymentProviderClient) Archive(ctx context.Context, provider string, accountID uuid.UUID, params *ArchivePaymentProviderAccountParams, requestOptions ...RequestOption) (*PaymentProviderConfig, error) {
	if accountID == uuid.Nil {
		return nil, fmt.Errorf("payment provider account ID is required")
	}
	return p.request(ctx, http.MethodPost, provider, "/accounts/"+accountID.String()+"/archive", params, requestOptions...)
}

func (p *PaymentProviderClient) request(ctx context.Context, method, provider, suffix string, params any, requestOptions ...RequestOption) (*PaymentProviderConfig, error) {
	provider, err := pathID("provider", provider)
	if err != nil {
		return nil, err
	}
	var out struct {
		Provider PaymentProviderConfig `json:"payment_provider"`
	}
	if err := p.client.do(ctx, method, "/v1/merchant/payment-providers/"+provider+suffix, params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out.Provider, nil
}
