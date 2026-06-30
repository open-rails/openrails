//go:build integration

package merchants

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLoadNMIWebhookSigningSecretForAccount proves #641 per-account webhook secret
// routing: with two NMI accounts on one merchant, each inbound endpoint resolves
// its OWN signing secret, and an unknown account is reported not-found so the
// caller rejects the webhook (never falling back to another account's secret).
func TestLoadNMIWebhookSigningSecretForAccount(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)
	tn, err := svc.Provision(ctx, ProvisionRequest{Slug: "acme", PermissionGroupID: "group-acme"})
	require.NoError(t, err)

	// Two NMI accounts on one merchant (mobius primary, paykings secondary).
	seedProviderAccount(t, svc, tn.ID, "nmi", "live", "100001")
	seedProviderAccount(t, svc, tn.ID, "nmi", "live", "100002")

	putSecret := func(accountID, secret string) {
		name, err := ProviderAccountSecretName("nmi", "live", accountID, "webhook_signing_secret")
		require.NoError(t, err)
		_, err = svc.secrets.Put(ctx, tn.ID, name, secret)
		require.NoError(t, err)
	}
	putSecret("100001", "whsec-mobius")
	putSecret("100002", "whsec-paykings")

	got, found, err := svc.LoadNMIWebhookSigningSecretForAccount(ctx, tn.ID, "100001")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "whsec-mobius", got)

	got, found, err = svc.LoadNMIWebhookSigningSecretForAccount(ctx, tn.ID, "100002")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "whsec-paykings", got)

	// Unknown account → not found: the webhook handler rejects (no fallback).
	_, found, err = svc.LoadNMIWebhookSigningSecretForAccount(ctx, tn.ID, "999999")
	require.NoError(t, err)
	require.False(t, found)
}
