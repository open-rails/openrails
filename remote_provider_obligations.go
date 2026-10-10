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
// commit its provider operation atomically uses the embedded Client's Tx
// operations. Every command answers the operation.

// providerOperationPath refuses ids that cannot name a path segment with the
// same invalid_param refusal the server-side validation returns. Operation ids
// are exact canonical host strings: surrounding whitespace is refused, not
// trimmed, as validateOperationID does.
func providerOperationPath(operationID string) (string, error) {
	if trimmed := strings.TrimSpace(operationID); trimmed == "" || trimmed != operationID || operationID == "." || operationID == ".." {
		return "", invalidErr(fmt.Sprintf("operation_id %q is not a valid operation id", operationID))
	}
	return "/v1/admin/provider-operations/" + url.PathEscape(operationID), nil
}

func (c *Client) providerOperation(ctx context.Context, method, path string, body any, requestOptions []RequestOption) (*billing.ProviderOperation, error) {
	var out billing.ProviderOperation
	if err := c.do(ctx, method, path, body, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// OpenProviderOperation reserves capacity for one provider operation. An
// identical retry replays; any changed immutable field is refused.
func (c *Client) OpenProviderOperation(ctx context.Context, req billing.OpenProviderOperationParams, requestOptions ...RequestOption) (*billing.ProviderOperation, error) {
	return c.providerOperation(ctx, http.MethodPost, "/v1/admin/provider-operations", req, requestOptions)
}

// GetProviderOperation reads one operation by its id, with its latest
// increment, qualification, refusal and resolution.
func (c *Client) GetProviderOperation(ctx context.Context, operationID string, requestOptions ...RequestOption) (*billing.ProviderOperation, error) {
	path, err := providerOperationPath(operationID)
	if err != nil {
		return nil, err
	}
	return c.providerOperation(ctx, http.MethodGet, path, nil, requestOptions)
}

// IncrementProviderOperation grows an open hold by up to Amount and no less
// than MinimumAmount, or refuses with nothing written. Repeating an ordinal
// with the same amounts replays it.
func (c *Client) IncrementProviderOperation(ctx context.Context, req billing.IncrementProviderOperationParams, requestOptions ...RequestOption) (*billing.ProviderOperation, error) {
	path, err := providerOperationPath(req.OperationID)
	if err != nil {
		return nil, err
	}
	return c.providerOperation(ctx, http.MethodPost, path+"/increment", req, requestOptions)
}

// ReleaseProviderOperation releases an open hold after proven provider
// non-creation. It is refused once billing evidence exists.
func (c *Client) ReleaseProviderOperation(ctx context.Context, req billing.ReleaseProviderOperationParams, requestOptions ...RequestOption) (*billing.ProviderOperation, error) {
	path, err := providerOperationPath(req.OperationID)
	if err != nil {
		return nil, err
	}
	return c.providerOperation(ctx, http.MethodPost, path+"/release", req, requestOptions)
}

// RecordProviderBillingObservation appends immutable provider evidence, or the
// host's refusal to produce any (a host refusal kind, which refuses the hold
// until an operator closes it). OpenRails qualifies evidence and, once
// eligible, rates and settles in the same commit.
//
// The wire body is the canonical encoding the server measures against
// ProviderBillingObservationMaxBytes, so the cap is applied here first: an
// oversized observation gets the server's invalid_param refusal in every
// deployment instead of the transport's body-limit status.
func (c *Client) RecordProviderBillingObservation(ctx context.Context, req billing.RecordProviderBillingObservationParams, requestOptions ...RequestOption) (*billing.ProviderOperation, error) {
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
	return c.providerOperation(ctx, http.MethodPost, path+"/observations", json.RawMessage(encoded), requestOptions)
}

// CloseProviderOperation closes a refused hold on an operator's attestation:
// settled charges the attested provider cost (above the hold as owed),
// written_off releases it uncharged. Repeating the same close replays; a
// changed term is refused.
func (c *Client) CloseProviderOperation(ctx context.Context, req billing.CloseProviderOperationParams, requestOptions ...RequestOption) (*billing.ProviderOperation, error) {
	path, err := providerOperationPath(req.OperationID)
	if err != nil {
		return nil, err
	}
	return c.providerOperation(ctx, http.MethodPost, path+"/close", req, requestOptions)
}

// ListProviderOperations is one page of the merchant's operations, newest
// first. Refused true with State open lists the holds waiting for an operator.
func (c *Client) ListProviderOperations(ctx context.Context, params billing.ProviderOperationListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.ProviderOperation], error) {
	q := pageValues(nil, params.PageRequest)
	values := map[string]string{"state": commaList(params.State)}
	if params.Refused != nil {
		values["refused"] = strconv.FormatBool(*params.Refused)
	}
	setQuery(q, values)
	var out billing.ListPage[billing.ProviderOperation]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/admin/provider-operations", q), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}
