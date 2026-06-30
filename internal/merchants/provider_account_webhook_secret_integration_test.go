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

// TestLoadStripeCredentialsForAccount mirrors the NMI case for Stripe (#641): a
// merchant with a live + legacy Stripe account resolves each account's OWN
// webhook secret; an unknown account is not-found.
func TestLoadStripeCredentialsForAccount(t *testing.T) {
	ctx := context.Background()
	svc := newSvc(t)
	tn, err := svc.Provision(ctx, ProvisionRequest{Slug: "acme", PermissionGroupID: "group-acme"})
	require.NoError(t, err)

	seedProviderAccount(t, svc, tn.ID, "stripe", "live", "acct_new")
	seedProviderAccount(t, svc, tn.ID, "stripe", "live", "acct_old")
	put := func(accountID, val string) {
		name, err := ProviderAccountSecretName("stripe", "live", accountID, "webhook_signing_secret")
		require.NoError(t, err)
		_, err = svc.secrets.Put(ctx, tn.ID, name, val)
		require.NoError(t, err)
	}
	put("acct_new", "whsec-new")
	put("acct_old", "whsec-old")

	creds, found, err := svc.LoadStripeCredentialsForAccount(ctx, tn.ID, "acct_old")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "whsec-old", creds.WebhookSigningSecret)

	_, found, err = svc.LoadStripeCredentialsForAccount(ctx, tn.ID, "acct_unknown")
	require.NoError(t, err)
	require.False(t, found)
}
