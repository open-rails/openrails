package openrails

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// These commands commit in OpenRails-owned transactions in every deployment.
// They cannot join a host database transaction; an embedding host that must
// commit its provider obligation atomically uses embed.HostTransactions.

// providerOperationPath refuses ids that cannot name a path segment. The
// server-side validation refuses the same ids with the same class.
func providerOperationPath(operationID string) (string, error) {
	if operationID == "" || operationID == "." || operationID == ".." {
		return "", fmt.Errorf("%w: operation_id %q is not a valid operation id", ErrInvalid, operationID)
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
//
// The wire body is the canonical encoding the server measures against
// ProviderBillingObservationMaxBytes, so the cap is applied here first: an
// oversized observation gets the server's invalid_param refusal in every
// deployment instead of the transport's body-limit status.
func (c *Client) RecordProviderBillingObservation(ctx context.Context, req ProviderBillingObservationRequest) (*ProviderBillingQualification, error) {
	path, err := providerOperationPath(req.OperationID)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("openrails: marshal request: %w", err)
	}
	if len(encoded) > ProviderBillingObservationMaxBytes {
		return nil, invalidErr(fmt.Sprintf("%v: provider billing observation encodes to %d bytes; limit is %d", ErrInvalid, len(encoded), ProviderBillingObservationMaxBytes))
	}
	var out ProviderBillingQualification
	if err := c.do(ctx, http.MethodPost, path+"/observations", json.RawMessage(encoded), &out); err != nil {
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
