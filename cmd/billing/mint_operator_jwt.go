package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	authcore "github.com/open-rails/authkit/core"
	"github.com/spf13/cobra"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/pkg/embedded"
	embcp "github.com/open-rails/openrails/pkg/embedded/controlplane"
)

// mintOperatorJWT mints a real, JWKS-verifiable, tenant-scoped user access token
// (NOT an opaque service token) for the operator tenant, so that the standalone server's
// JWT verifier accepts it on the /v1/admin/* surface (OperatorAdminRequired
// checks the `org` + `org_roles` claims). It is the e2e provisioning bridge:
// the admin catalog/price/solana-plan routes are gated by a JWT claim check,
// while mint-operator-service-token only produces an opaque service token usable on /v1/service/*.
//
// It idempotently ensures the operator tenant + role exist (control-plane
// bootstrap), creates (or reuses) a test user, makes them an operator-org
// member, assigns them the operator role, then issues a tenant-scoped JWT via
// AuthKit core. The result verifies against the control plane's own JWKS when
// the control-plane issuer is in the server's auth.issuers.
func mintOperatorJWT(cmd *cobra.Command, _ []string) error {
	cfg := cmd.Context().Value(config.ConfigContextKey).(*config.Config)
	ctx := context.Background()

	embeddedApp, err := embedded.New(embedded.Options{Config: cfg})
	if err != nil {
		return fmt.Errorf("bootstrap application: %w", err)
	}
	defer func() { _ = embeddedApp.Close(ctx) }()

	if err := embcp.Attach(ctx, embeddedApp.App(), cfg, nil); err != nil {
		return fmt.Errorf("attach control plane: %w", err)
	}
	cp := embcp.Get(embeddedApp.App())
	if cp == nil || cp.Core() == nil {
		return fmt.Errorf("control plane is not enabled (set auth.control_plane.enabled + issuer)")
	}
	core := cp.Core()

	org, _ := cmd.Flags().GetString("org")
	org = strings.ToLower(strings.TrimSpace(org))
	if org == "" && cfg != nil && cfg.Auth != nil {
		org = strings.ToLower(strings.TrimSpace(cfg.Auth.OperatorTenantSlug))
	}
	if org == "" {
		org = "operator"
	}

	email, _ := cmd.Flags().GetString("email")
	email = strings.TrimSpace(email)
	if email == "" {
		email = "e2e-operator@openrails.test"
	}
	username, _ := cmd.Flags().GetString("username")
	username = strings.TrimSpace(username)
	if username == "" {
		username = "e2e-operator"
	}
	role, _ := cmd.Flags().GetString("role")
	role = strings.TrimSpace(role)
	if role == "" {
		role = controlplane.OperatorRole
	}

	// 1. Ensure operator tenant + role exist (idempotent bootstrap).
	if _, berr := embcp.RunBootstrap(ctx, embeddedApp.App(), controlplane.BootstrapOptions{
		OperatorTenantSlug:      org,
		MintInitialServiceToken: false,
	}); berr != nil {
		return fmt.Errorf("bootstrap operator tenant/role: %w", berr)
	}

	// 2. Find or create the test user.
	userID, err := ensureUser(ctx, core, email, username)
	if err != nil {
		return fmt.Errorf("ensure user: %w", err)
	}

	// 3. Make them an operator-org member + assign the operator role.
	if err := core.AddMember(ctx, org, userID); err != nil {
		return fmt.Errorf("add member: %w", err)
	}
	if err := core.AssignRole(ctx, org, userID, role); err != nil {
		return fmt.Errorf("assign role %q: %w", role, err)
	}

	// 4. Issue the tenant-scoped JWT (carries tenant + tenant_roles claims).
	token, exp, err := core.IssueServiceToken(ctx, userID, email, org, nil)
	if err != nil {
		return fmt.Errorf("issue tenant access token: %w", err)
	}

	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"org":        org,
		"user_id":    userID,
		"email":      email,
		"role":       role,
		"token":      token,
		"expires_at": exp,
	})
}

// ensureUser returns the id of an existing user with this email, creating one
// when absent. CreateUser is the AuthKit core primitive; lookups vary by
// version, so we fall back to creating and tolerate a duplicate by re-reading.
func ensureUser(ctx context.Context, core *authcore.Service, email, username string) (string, error) {
	if u, err := core.GetUserByEmail(ctx, email); err == nil && u != nil && strings.TrimSpace(u.ID) != "" {
		return u.ID, nil
	}
	u, err := core.CreateUser(ctx, email, username)
	if err == nil && u != nil {
		return u.ID, nil
	}
	// Creation may race / already exist: re-read by email.
	if u2, gerr := core.GetUserByEmail(ctx, email); gerr == nil && u2 != nil && strings.TrimSpace(u2.ID) != "" {
		return u2.ID, nil
	}
	if err != nil {
		return "", err
	}
	return "", errors.New("user created but no id returned")
}
