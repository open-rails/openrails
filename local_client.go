package openrails

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/engine"
)

// LocalClient holds a merchant's operations that need the engine in this
// process: the ones inside a host transaction and the notification
// adoption pair. Client is the merchant's handle for everything else.
// Engine.LocalMerchant makes one; a zero LocalClient refuses every call.
type LocalClient struct{ client *Client }

var errNoLocalClient = errors.New("openrails: a LocalClient comes from Engine.LocalMerchant")

// LocalMerchant is merchant id's in-process extension. Like Merchant, it
// does no I/O; each operation keeps its merchant and lifecycle checks.
func (e *Engine) LocalMerchant(id billing.MerchantID) (*LocalClient, error) {
	if e == nil || e.engine == nil {
		return nil, fmt.Errorf("openrails: an initialized Engine is required")
	}
	client, err := e.Merchant(id)
	if err != nil {
		return nil, err
	}
	return &LocalClient{client: client}, nil
}

// Client is the merchant's handle, the same one Engine.Merchant returns.
func (c *LocalClient) Client() *Client {
	if c == nil {
		return nil
	}
	return c.client
}

// local is the handle's engine and merchant, refusing a zero LocalClient.
func (c *LocalClient) local() (*engine.Engine, billing.MerchantID, error) {
	if c == nil || c.client == nil || c.client.engine == nil {
		return nil, billing.MerchantID{}, errNoLocalClient
	}
	return c.client.engine, c.client.merchantID, nil
}

// StartRunningChargeTx is StartRunningCharge inside tx, a transaction from the
// host's pool on the engine's database, so a charge starts in the commit that
// records the work as ready. OpenRails binds the handle's merchant and never
// commits or rolls tx back; after an error the host rolls back.
func (c *LocalClient) StartRunningChargeTx(ctx context.Context, tx pgx.Tx, params billing.StartRunningChargeParams) (*billing.RunningCharge, error) {
	e, merchant, err := c.local()
	if err != nil {
		return nil, err
	}
	return e.StartRunningChargeTx(ctx, merchant, tx, params)
}

// StopRunningChargeTx is StopRunningCharge inside tx (see
// StartRunningChargeTx), so a charge stops in the commit that records the work
// as released.
func (c *LocalClient) StopRunningChargeTx(ctx context.Context, tx pgx.Tx, reference string, params billing.StopRunningChargeParams) (*billing.RunningCharge, error) {
	e, merchant, err := c.local()
	if err != nil {
		return nil, err
	}
	return e.StopRunningChargeTx(ctx, merchant, tx, reference, params)
}

// AdmitTx is Admit inside tx, a transaction from the host's pool on the
// engine's database, so an admission and its hold commit with the host's
// request row, or not at all: no outbox and no orphan hold. OpenRails binds
// the handle's merchant and never commits or rolls tx back; after an error
// the host rolls back.
func (c *LocalClient) AdmitTx(ctx context.Context, tx pgx.Tx, requests []billing.AdmitParams) ([]billing.AdmissionVerdict, error) {
	e, merchant, err := c.local()
	if err != nil {
		return nil, err
	}
	return e.AdmitTx(ctx, merchant, tx, requests)
}

// CaptureAdmissionsTx is CaptureAdmissions inside tx (see AdmitTx), so a
// capture commits with the host's record of the finished work.
func (c *LocalClient) CaptureAdmissionsTx(ctx context.Context, tx pgx.Tx, items []billing.CaptureAdmissionParams) ([]billing.CaptureResult, error) {
	e, merchant, err := c.local()
	if err != nil {
		return nil, err
	}
	return e.CaptureAdmissionsTx(ctx, merchant, tx, items)
}

// ReleaseAdmissionsTx is ReleaseAdmissions inside tx (see AdmitTx), so a
// release commits with the host's record of the failed work.
func (c *LocalClient) ReleaseAdmissionsTx(ctx context.Context, tx pgx.Tx, requestIDs []string) ([]billing.AdmissionResult, error) {
	e, merchant, err := c.local()
	if err != nil {
		return nil, err
	}
	return e.ReleaseAdmissionsTx(ctx, merchant, tx, requestIDs)
}
