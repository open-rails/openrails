// Package history carries audit provenance; it never grants authority or decides billing work.
package history

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/internal/billingauth"
)

// Attribution preserves the initiator separately from the code executing a change.
type Attribution struct {
	Origin      string     `json:"origin,omitempty"`
	Actor       string     `json:"actor,omitempty"`
	Executor    string     `json:"executor,omitempty"`
	OperationID *uuid.UUID `json:"operation_id,omitempty"`
}
type contextKey struct{}

func WithAttribution(ctx context.Context, a Attribution) context.Context {
	return context.WithValue(ctx, contextKey{}, a)
}

func FromContext(ctx context.Context) Attribution {
	if a, ok := ctx.Value(contextKey{}).(Attribution); ok {
		return a
	}
	if staff, ok := billingauth.StaffFromContext(ctx); ok {
		return Attribution{Origin: "admin", Actor: billingauth.InvokerKey(staff.Identity), Executor: "request"}
	}
	if who, ok := billingauth.IdentityFromContext(ctx); ok {
		return Attribution{Origin: "user", Actor: billingauth.InvokerKey(who), Executor: "request"}
	}
	return Attribution{}
}

// Bind confines transaction-local metadata to one row write, including its old version.
func Bind(ctx context.Context, merchant, entity uuid.UUID, action string, version int64, decision string) string {
	a := struct {
		Attribution
		MerchantID uuid.UUID `json:"merchant_id"`
		EntityID   uuid.UUID `json:"entity_id"`
		Action     string    `json:"action"`
		Version    int64     `json:"version"`
		Decision   string    `json:"decision,omitempty"`
	}{FromContext(ctx), merchant, entity, action, version, decision}
	raw, _ := json.Marshal(a) // Concrete strings, UUIDs and integers cannot fail to encode.
	return string(raw)
}
