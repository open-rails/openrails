// Package authkit connects AuthKit group ownership to explicit OpenRails
// merchant bindings. It is opt-in; the embedded engine does not link AuthKit.
package authkit

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	native "github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchantbinding"
)

type groupReader struct{ client *native.Client }

// MerchantGroups supplies Deps.MerchantGroups from a borrowed AuthKit client.
// Every read uses the binding's explicit tenant; it never changes the client's
// default tenant or the source authentication session.
func MerchantGroups(client *native.Client) billing.MerchantGroupReader {
	return groupReader{client: client}
}

func (r groupReader) LookupMerchantGroup(ctx context.Context, binding billing.MerchantBinding) (billing.MerchantGroup, error) {
	view, err := tenantClient(ctx, r.client, binding)
	if err != nil {
		return billing.MerchantGroup{}, err
	}
	group, err := view.Group(ctx, iam.GroupByID(binding.ID.String()))
	if err != nil {
		return billing.MerchantGroup{}, err
	}
	return metadata(group), nil
}

// BindMerchant adopts an existing, live merchant into its explicit business
// tenant. Its group ID remains its billing ID. Group creation and the billing
// binding commit together on pool; client must use that same database.
// No owner, role assignment, key or provider operation is created.
func BindMerchant(ctx context.Context, pool *pgxpool.Pool, database openrails.DatabaseConfig, client *native.Client, binding billing.MerchantBinding) error {
	view, err := tenantClient(ctx, client, binding)
	if err != nil {
		return err
	}
	var persona iam.Persona
	if err := persona.UnmarshalText([]byte(billing.MerchantGroupPersona)); err != nil {
		return err
	}
	tenant, err := iam.ParseTenantID(binding.TenantID)
	if err != nil {
		return err
	}
	return merchantbinding.Bind(ctx, pool, database, binding, func(ctx context.Context, tx pgx.Tx) error {
		group, err := view.CreateGroup(ctx, iam.NewGroup{
			ID: binding.ID.String(), TenantID: tenant, Persona: persona,
		}, native.InTx(tx))
		if err != nil {
			return err
		}
		return merchantbinding.ValidateGroup(binding, metadata(group))
	})
}

func tenantClient(ctx context.Context, client *native.Client, binding billing.MerchantBinding) (*native.Client, error) {
	if err := merchantbinding.Validate(binding); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("merchant binding requires AuthKit")
	}
	tenant, err := iam.ParseTenantID(binding.TenantID)
	if err != nil {
		return nil, err
	}
	return client.ForTenant(ctx, tenant)
}

func metadata(group iam.Group) billing.MerchantGroup {
	return billing.MerchantGroup{
		ID: group.ID, TenantID: group.TenantID.String(),
		Persona: group.Persona.String(), Deleted: group.DeletedAt != nil,
	}
}
