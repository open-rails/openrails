package openrails

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// These commands commit in OpenRails-owned transactions in every deployment.
// They cannot join a host database transaction; an embedding host that must
// commit its provider obligation atomically uses embed.HostTransactions.

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
func (c *Client) OpenOperationAuthorization(ctx context.Context, req OperationAuthorizationRequest) (*OperationAuthorization, error) {
	var out OperationAuthorization
	if err := c.do(ctx, http.MethodPost, "/v1/merchant/provider-operations", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetOperationAuthorization(ctx context.Context, operationID string) (*OperationAuthorization, error) {
	path, err := providerOperationPath(operationID)
	if err != nil {
		return nil, err
	}
	var out OperationAuthorization
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ReleaseOperationAuthorization releases an open reservation after proven
// provider non-creation. It is refused once billing evidence exists.
func (c *Client) ReleaseOperationAuthorization(ctx context.Context, req ReleaseOperationAuthorizationRequest) (*OperationAuthorization, error) {
	path, err := providerOperationPath(req.OperationID)
	if err != nil {
		return nil, err
	}
	var out OperationAuthorization
	if err := c.do(ctx, http.MethodPost, path+"/release", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// RecordProviderBillingObservation appends immutable provider evidence.
// OpenRails qualifies it and, once eligible, rates and settles in the same commit.
func (c *Client) RecordProviderBillingObservation(ctx context.Context, req ProviderBillingObservationRequest) (*ProviderBillingQualification, error) {
	path, err := providerOperationPath(req.OperationID)
	if err != nil {
		return nil, err
	}
	var out ProviderBillingQualification
	if err := c.do(ctx, http.MethodPost, path+"/observations", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetProviderBillingQualification(ctx context.Context, operationID string) (*ProviderBillingQualification, error) {
	path, err := providerOperationPath(operationID)
	if err != nil {
		return nil, err
	}
	var out ProviderBillingQualification
	if err := c.do(ctx, http.MethodGet, path+"/qualification", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
