package openrails

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/open-rails/openrails/billing"
)

// These commands commit in OpenRails-owned transactions in every deployment.
// They cannot join a host database transaction; an embedding host that must
// commit its provider obligation atomically uses the embedded Client's Tx
// operations.

// providerOperationPath refuses ids that cannot name a path segment with the
// same invalid_param refusal the server-side validation returns. Operation ids
// are exact canonical host strings: surrounding whitespace is refused, not
// trimmed, as validateOperationID does.
func providerOperationPath(operationID string) (string, error) {
	if trimmed := strings.TrimSpace(operationID); trimmed == "" || trimmed != operationID || operationID == "." || operationID == ".." {
		return "", invalidErr(fmt.Sprintf("operation_id %q is not a valid operation id", operationID))
	}
	return "/v1/merchant/provider-operations/" + url.PathEscape(operationID), nil
}

// OpenOperationAuthorization reserves capacity for one provider operation. An
// identical retry replays; any changed immutable field is refused.
func (c *Client) OpenOperationAuthorization(ctx context.Context, req billing.OpenOperationAuthorizationParams, requestOptions ...RequestOption) (*billing.OperationAuthorization, error) {
	var out billing.OperationAuthorization
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/provider-operations", req, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetOperationAuthorization reads one reservation by its operation ID.
func (c *Client) GetOperationAuthorization(ctx context.Context, operationID string, requestOptions ...RequestOption) (*billing.OperationAuthorization, error) {
	path, err := providerOperationPath(operationID)
	if err != nil {
		return nil, err
	}
	var out billing.OperationAuthorization
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ExtendOperationAuthorization grows an open reservation by up to Amount and
// no less than MinimumAmount, or refuses with nothing written. Repeating an
// ordinal with the same amounts replays its grant.
func (c *Client) ExtendOperationAuthorization(ctx context.Context, req billing.ExtendOperationAuthorizationParams, requestOptions ...RequestOption) (*billing.OperationAuthorizationExtension, error) {
	path, err := providerOperationPath(req.OperationID)
	if err != nil {
		return nil, err
	}
	var out billing.OperationAuthorizationExtension
	if err := c.do(ctx, http.MethodPost, path+"/extend", req, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReleaseOperationAuthorization releases an open reservation after proven
// provider non-creation. It is refused once billing evidence exists.
func (c *Client) ReleaseOperationAuthorization(ctx context.Context, req billing.ReleaseOperationAuthorizationParams, requestOptions ...RequestOption) (*billing.OperationAuthorization, error) {
	path, err := providerOperationPath(req.OperationID)
	if err != nil {
		return nil, err
	}
	var out billing.OperationAuthorization
	if err := c.do(ctx, http.MethodPost, path+"/release", req, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// RecordProviderBillingObservation appends immutable provider evidence.
// OpenRails qualifies it and, once eligible, rates and settles in the same commit.
//
// The wire body is the canonical encoding the server measures against
// ProviderBillingObservationMaxBytes, so the cap is applied here first: an
// oversized observation gets the server's invalid_param refusal in every
// deployment instead of the transport's body-limit status.
func (c *Client) RecordProviderBillingObservation(ctx context.Context, req billing.RecordProviderBillingObservationParams, requestOptions ...RequestOption) (*billing.ProviderBillingQualification, error) {
	path, err := providerOperationPath(req.OperationID)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("openrails: marshal request: %w", err)
	}
	if len(encoded) > billing.ProviderBillingObservationMaxBytes {
		return nil, invalidErr(fmt.Sprintf("%v: provider billing observation encodes to %d bytes; limit is %d", billing.ErrInvalid, len(encoded), billing.ProviderBillingObservationMaxBytes))
	}
	var out billing.ProviderBillingQualification
	if err := c.do(ctx, http.MethodPost, path+"/observations", json.RawMessage(encoded), &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetProviderBillingQualification reads how far an operation's provider
// evidence has qualified it for settlement.
func (c *Client) GetProviderBillingQualification(ctx context.Context, operationID string, requestOptions ...RequestOption) (*billing.ProviderBillingQualification, error) {
	path, err := providerOperationPath(operationID)
	if err != nil {
		return nil, err
	}
	var out billing.ProviderBillingQualification
	if err := c.do(ctx, http.MethodGet, path+"/qualification", nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ResolveProviderBillingQualification is CloseOperationAuthorization answered as
// the hold's qualification; a hold without one answers
// provider_billing_qualification_not_found.
func (c *Client) ResolveProviderBillingQualification(ctx context.Context, req billing.ResolveProviderBillingQualificationParams, requestOptions ...RequestOption) (*billing.ProviderBillingQualification, error) {
	path, err := providerOperationPath(req.OperationID)
	if err != nil {
		return nil, err
	}
	var out billing.ProviderBillingQualification
	if err := c.do(ctx, http.MethodPost, path+"/resolution", req, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListProviderBillingQualifications is one page of the merchant's provider
// billing qualifications, newest first. State refused with AuthorizationState
// open lists the holds only an operator can close.
func (c *Client) ListProviderBillingQualifications(ctx context.Context, params billing.ProviderBillingQualificationListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.ProviderBillingQualification], error) {
	q := pageValues(nil, params.PageRequest)
	setQuery(q, map[string]string{"state": commaList(params.State), "authorization_state": commaList(params.AuthorizationState)})
	var out billing.ListPage[billing.ProviderBillingQualification]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/merchant/provider-qualifications", q), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// RefuseProviderBillingQualification records that the host cannot qualify an
// open hold's provider cost (it cannot prove the lifecycle, the provider reports
// no billing, or OpenRails rejects its evidence). The hold then accepts no
// observation, extension or release and waits for CloseOperationAuthorization.
// Repeating the same refusal replays; a changed term is refused.
func (c *Client) RefuseProviderBillingQualification(ctx context.Context, req billing.RefuseProviderBillingQualificationParams, requestOptions ...RequestOption) (*billing.OperationAuthorization, error) {
	path, err := providerOperationPath(req.OperationID)
	if err != nil {
		return nil, err
	}
	var out billing.OperationAuthorization
	if err := c.do(ctx, http.MethodPost, path+"/refusal", req, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// CloseOperationAuthorization closes a refused hold on an operator's
// attestation: settled charges the attested provider cost (above the hold as
// owed), written_off releases it uncharged. Repeating the same close replays; a
// changed term is refused.
func (c *Client) CloseOperationAuthorization(ctx context.Context, req billing.CloseOperationAuthorizationParams, requestOptions ...RequestOption) (*billing.OperationAuthorization, error) {
	path, err := providerOperationPath(req.OperationID)
	if err != nil {
		return nil, err
	}
	var out billing.OperationAuthorization
	if err := c.do(ctx, http.MethodPost, path+"/close", req, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListOperationAuthorizations is one page of the merchant's holds, newest
// first, each with its refusal and resolution. Refused true with State open
// lists the holds waiting for an operator.
func (c *Client) ListOperationAuthorizations(ctx context.Context, params billing.OperationAuthorizationListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.OperationAuthorization], error) {
	q := pageValues(nil, params.PageRequest)
	values := map[string]string{"state": commaList(params.State)}
	if params.Refused != nil {
		values["refused"] = strconv.FormatBool(*params.Refused)
	}
	setQuery(q, values)
	var out billing.ListPage[billing.OperationAuthorization]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/merchant/provider-operations", q), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
