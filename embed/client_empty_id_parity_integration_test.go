//go:build integration

package embed_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/integrationharness"
)

// idErrorObservation is everything a caller can branch on for an identifier
// refusal, excluding the human message and the request id.
type idErrorObservation struct {
	StatusError           bool
	Status                int
	Type, Code, Param     string
	Invalid, NotFound     bool
	Unreachable, Conflict bool
}

func observeIDError(t *testing.T, label string, err error) idErrorObservation {
	t.Helper()
	require.Error(t, err, label)
	o := idErrorObservation{
		Invalid:     errors.Is(err, openrails.ErrInvalid),
		NotFound:    errors.Is(err, openrails.ErrNotFound),
		Unreachable: errors.Is(err, openrails.ErrUnreachable),
		Conflict:    errors.Is(err, openrails.ErrConflict),
	}
	var status *openrails.StatusError
	if errors.As(err, &status) {
		o.StatusError, o.Status, o.Type, o.Code = true, status.Status, status.Type, status.Code
		if status.Param != nil {
			o.Param = *status.Param
		}
	}
	return o
}

// Blank identifiers are refused by the Client before any I/O with the same
// invalid_param refusal the server returns for a malformed identifier, so the
// in-process embedded Client, an embedded host's HTTP mount and a standalone
// server observe one error, and it is the server's own class.
func TestClientEmptyIdentifierErrorsAreIdenticalAcrossDeployments(t *testing.T) {
	ctx := context.Background()
	h := integrationharness.New(t, ctx)
	standalone := h.StartStandalone("USD")
	host := h.StartEmbeddedHost("USD")
	local, err := host.Runtime().Client()
	require.NoError(t, err)
	clients := map[string]*openrails.Client{"embedded": local, "hosted_http": host.Client(), "standalone": standalone.Client()}

	now := time.Now().UTC()
	valid := uuid.NewString()
	script := func(c *openrails.Client) map[string]idErrorObservation {
		out := map[string]idErrorObservation{}
		_, err := c.GetSubscription(ctx, "  ")
		out["subscription_blank"] = observeIDError(t, "subscription blank", err)
		out["entitlement_revoke_blank"] = observeIDError(t, "entitlement revoke blank", c.RevokeEntitlement(ctx, valid, ""))
		_, err = c.GrantEntitlement(ctx, "", openrails.GrantEntitlementRequest{Entitlement: "pro"})
		out["entitlement_grant_blank_customer"] = observeIDError(t, "entitlement grant blank customer", err)
		_, err = c.PreviewPlanMigration(ctx, openrails.PlanMigrationRequest{SourcePrice: " ", TargetPrice: valid})
		out["plan_migration_blank_source"] = observeIDError(t, "plan migration blank source", err)
		_, err = c.CancelPlanMigration(ctx, uuid.Nil)
		out["plan_migration_cancel_nil"] = observeIDError(t, "plan migration cancel nil", err)
		_, err = c.SetPriceKey(ctx, uuid.Nil, "key")
		out["price_key_nil"] = observeIDError(t, "price key nil", err)
		_, err = c.GetMerchantInvoice(ctx, uuid.Nil)
		out["invoice_nil"] = observeIDError(t, "invoice nil", err)
		_, err = c.Balance(ctx, "\t")
		out["balance_blank"] = observeIDError(t, "balance blank", err)
		_, err = c.GetOperationAuthorization(ctx, "..")
		out["operation_dot"] = observeIDError(t, "operation dot", err)
		_, err = c.ListEntitlements(ctx, "", now)
		out["entitlements_blank_subject"] = observeIDError(t, "entitlements blank subject", err)

		// Malformed identifiers reach the server; its refusal is the class
		// the Client mirrors.
		_, err = c.GetSubscription(ctx, "not-a-uuid")
		out["subscription_malformed"] = observeIDError(t, "subscription malformed", err)
		out["entitlement_revoke_malformed"] = observeIDError(t, "entitlement revoke malformed", c.RevokeEntitlement(ctx, valid, "not-a-uuid"))
		return out
	}

	observed := map[string]map[string]idErrorObservation{}
	for name, client := range clients {
		observed[name] = script(client)
	}
	want := observed["standalone"]
	invalidParam := idErrorObservation{StatusError: true, Status: 400, Type: "invalid_request_error", Code: "invalid_param", Invalid: true}
	for label, got := range want {
		require.Equal(t, invalidParam, got, "standalone %s", label)
	}
	for name, got := range observed {
		require.Equal(t, want, got, "%s identifier error contract diverged from standalone", name)
	}
}
