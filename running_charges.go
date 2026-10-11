package openrails

import (
	"context"
	"net/http"
	"net/url"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
)

// A running charge bills a customer at a rate per hour until it is stopped
// (see billing.StartRunningChargeParams). Start and stop commit in
// OpenRails-owned transactions; an embedding host that must commit them with
// its own rows uses StartRunningChargeTx and StopRunningChargeTx.

// runningChargePath keeps a host's reference one URL segment: PathEscape
// leaves dot segments intact, which ServeMux would clean away.
func runningChargePath(reference string) (string, error) {
	if strings.TrimSpace(reference) == "" || strings.TrimSpace(reference) != reference || reference == "." || reference == ".." {
		return "", invalidErr("reference is required in canonical form")
	}
	return "/v1/app/running-charges/" + url.PathEscape(reference), nil
}

// StartRunningCharge starts charging a customer at a rate per hour, or answers
// the charge params.Reference already names. A changed term is
// billing.ErrIdempotencyKeyReused; a MinimumAmount the customer's capacity
// does not cover is billing.ErrInsufficientCredits, with nothing written.
func (c *Client) StartRunningCharge(ctx context.Context, params billing.StartRunningChargeParams, requestOptions ...RequestOption) (*billing.RunningCharge, error) {
	params.Currency = normalizeCurrency(params.Currency)
	var out billing.RunningCharge
	if err := c.do(ctx, http.MethodPost, "/v1/app/running-charges", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// StopRunningCharge stops the charge reference names and posts the rest of it
// to the ledger: ChargedAmount is final. Stopping it again at the same instant
// answers it; at another it is billing.ErrIdempotencyKeyReused.
func (c *Client) StopRunningCharge(ctx context.Context, reference string, params billing.StopRunningChargeParams, requestOptions ...RequestOption) (*billing.RunningCharge, error) {
	path, err := runningChargePath(reference)
	if err != nil {
		return nil, err
	}
	var out billing.RunningCharge
	if err := c.do(ctx, http.MethodPost, path+"/stop", params, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// GetRunningCharge reads the charge reference names, with what it has charged
// as of now.
func (c *Client) GetRunningCharge(ctx context.Context, reference string, requestOptions ...RequestOption) (*billing.RunningCharge, error) {
	path, err := runningChargePath(reference)
	if err != nil {
		return nil, err
	}
	var out billing.RunningCharge
	if err := c.do(ctx, http.MethodGet, path, nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// ListRunningCharges is one page of the merchant's running charges, newest
// first: of params.CustomerID and in params.State when set. With
// params.Depleted it lists only the running charges of customers whose
// capacity has run out, whether or not the watcher has looked yet: confirm
// with GetBalance, then stop what they pay for.
func (c *Client) ListRunningCharges(ctx context.Context, params billing.RunningChargeListParams, requestOptions ...RequestOption) (*billing.ListPage[billing.RunningCharge], error) {
	q := pageValues(nil, params.PageRequest)
	setQuery(q, map[string]string{"customer_id": params.CustomerID.String(), "state": string(params.State)})
	if params.Depleted {
		q.Set("depleted", "true")
	}
	var out billing.ListPage[billing.RunningCharge]
	if err := c.do(ctx, http.MethodGet, withQuery("/v1/app/running-charges", q), nil, &out, requestOptions...); err != nil {
		return nil, err
	}
	return &out, nil
}

// StartRunningChargeTx is StartRunningCharge inside tx, a transaction from the
// host's pool on the engine's database, so a charge starts in the commit that
// records the work as ready. OpenRails binds the handle's merchant and never
// commits or rolls tx back; after an error the host rolls back. Embedded only:
// a remote client returns ErrRemoteClient.
func (c *Client) StartRunningChargeTx(ctx context.Context, tx pgx.Tx, params billing.StartRunningChargeParams) (*billing.RunningCharge, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	params.Currency = normalizeCurrency(params.Currency)
	return e.StartRunningChargeTx(ctx, c.merchantID, tx, params)
}

// StopRunningChargeTx is StopRunningCharge inside tx (see
// StartRunningChargeTx), so a charge stops in the commit that records the work
// as released. Embedded only.
func (c *Client) StopRunningChargeTx(ctx context.Context, tx pgx.Tx, reference string, params billing.StopRunningChargeParams) (*billing.RunningCharge, error) {
	e, err := c.embedded()
	if err != nil {
		return nil, err
	}
	return e.StopRunningChargeTx(ctx, c.merchantID, tx, reference, params)
}
