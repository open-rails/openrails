package openrails

import (
	"context"
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
)

// LocalClient adds operations that require the engine in this process to the
// shared merchant Client. Engine.LocalMerchant constructs it; remote clients
// have no local extension. Client is the same handle accepted by integrations
// that work with either embedded or remote OpenRails.
type LocalClient struct{ *Client }

// LocalMerchant is a merchant handle with the engine's local-only operations.
// Use Merchant when the consumer needs the common HTTP/embedded API. Like
// Merchant, it performs no I/O; each operation retains its merchant and lifecycle checks.
func (e *Engine) LocalMerchant(id billing.MerchantID) (*LocalClient, error) {
	if e == nil || e.engine == nil {
		return nil, fmt.Errorf("openrails: an initialized Engine is required")
	}
	client, err := e.Merchant(id)
	if err != nil {
		return nil, err
	}
	return &LocalClient{Client: client}, nil
}

// DeclarePSP records a PSP identity of the handle's merchant, without
// credentials, for imported billing facts attributed to it. Call during
// setup, before serving or starting workers. It never arms the PSP for
// checkout.
func (c *LocalClient) DeclarePSP(ctx context.Context, declaration billing.PSPDeclaration) (*billing.PSP, error) {
	e := c.engine
	if e.App.Runtime == nil || e.App.Runtime.Merchants == nil {
		return nil, fmt.Errorf("openrails: runtime not initialized")
	}
	if _, err := e.LiveMerchant(ctx, c.merchantID); err != nil {
		return nil, fmt.Errorf("openrails: merchant %s: %w", c.merchantID, err)
	}
	psp, err := e.App.Runtime.Merchants.DeclarePSP(ctx, c.merchantID, declaration)
	if err != nil {
		return nil, err
	}
	return &psp, nil
}

// SCIMHandler is the SCIM 2.0 service provider for the handle's merchant, for
// a directory in the same process (AuthKit's provisioning) to push its users
// to without a network hop. Its paths are relative to the SCIM root (/Users,
// /Bulk, /ServiceProviderConfig); it authenticates nothing, since holding the
// Engine is the authority. It fails on an engine that reads Deps.UserInfo.
func (c *LocalClient) SCIMHandler() (http.Handler, error) {
	e := c.engine
	return e.SCIMHandler(c.merchantID)
}

// StartRunningChargeTx is StartRunningCharge inside tx, a transaction from the
// host's pool on the engine's database, so a charge starts in the commit that
// records the work as ready. OpenRails binds the handle's merchant and never
// commits or rolls tx back; after an error the host rolls back.
func (c *LocalClient) StartRunningChargeTx(ctx context.Context, tx pgx.Tx, params billing.StartRunningChargeParams) (*billing.RunningCharge, error) {
	e := c.engine
	params.Currency = normalizeCurrency(params.Currency)
	return e.StartRunningChargeTx(ctx, c.merchantID, tx, params)
}

// StopRunningChargeTx is StopRunningCharge inside tx (see
// StartRunningChargeTx), so a charge stops in the commit that records the work
// as released.
func (c *LocalClient) StopRunningChargeTx(ctx context.Context, tx pgx.Tx, reference string, params billing.StopRunningChargeParams) (*billing.RunningCharge, error) {
	e := c.engine
	return e.StopRunningChargeTx(ctx, c.merchantID, tx, reference, params)
}
