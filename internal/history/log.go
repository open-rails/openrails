package history

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
)

// Log is one history table. Every log shares one envelope: when, actor,
// source, object, action, a versioned payload, reason, request and
// operation.
type Log string

// ChangeLog is configuration history: the catalog and merchant
// configuration.
const ChangeLog Log = "change_log"

// Entry is one object's change.
type Entry struct {
	ObjectType billing.LogObjectType
	ObjectID   string
	Action     billing.LogAction
	// Changes is the action's payload: Fields, Keys or KeyRename.
	Changes any
}

// Provenance is who made a set of changes, how and when.
type Provenance struct {
	At          time.Time
	Actor       billing.LogActor
	Source      billing.LogSource
	Reason      string
	RequestID   string
	OperationID *uuid.UUID
}

// Record writes entries in q's transaction: they commit and roll back with
// the change they record.
func Record(ctx context.Context, q *gen.Queries, log Log, merchantID uuid.UUID, p Provenance, entries ...Entry) error {
	return write(ctx, q, log, merchantID, p, entries)
}

// WriteAhead writes entries on pool as statements of their own, outside any
// business transaction: evidence of an operation written before it runs,
// kept whatever follows.
func WriteAhead(ctx context.Context, pool *db.Pool, log Log, merchantID uuid.UUID, p Provenance, entries ...Entry) error {
	if pool == nil {
		return fmt.Errorf("history: write-ahead needs a pool")
	}
	return write(ctx, gen.New(pool), log, merchantID, p, entries)
}

func write(ctx context.Context, q *gen.Queries, log Log, merchantID uuid.UUID, p Provenance, entries []Entry) error {
	if log != ChangeLog {
		return fmt.Errorf("history: unknown log %q", log)
	}
	if err := p.valid(); err != nil {
		return err
	}
	for _, e := range entries {
		raw, err := json.Marshal(e.Changes)
		if err != nil {
			return fmt.Errorf("history: %s %s: %w", e.ObjectType, e.ObjectID, err)
		}
		if err := q.InsertChangeLogEntry(ctx, gen.InsertChangeLogEntryParams{
			MerchantID: merchantID, OccurredAt: p.At, Actor: string(p.Actor), Source: string(p.Source),
			ObjectType: string(e.ObjectType), ObjectID: e.ObjectID, Action: string(e.Action), Changes: raw,
			Reason: optional(p.Reason), RequestID: optional(p.RequestID), OperationID: p.OperationID,
		}); err != nil {
			return fmt.Errorf("history: record %s %s: %w", e.ObjectType, e.ObjectID, err)
		}
	}
	return nil
}

func (p Provenance) valid() error {
	if p.At.IsZero() || p.Actor == "" || p.Source == "" {
		return fmt.Errorf("history: a change needs its time, actor and source")
	}
	return nil
}

func optional(s string) *string {
	if s = strings.TrimSpace(s); s == "" {
		return nil
	}
	return &s
}

// Staff is a staff member or application, by its invoker.
func Staff(invoker string) billing.LogActor { return billing.LogActor("staff:" + invoker) }

// Customer is a customer, by its invoker.
func Customer(invoker string) billing.LogActor { return billing.LogActor("customer:" + invoker) }

// Provider is a rail's provider.
func Provider(rail billing.Rail) billing.LogActor {
	return billing.LogActor("provider:" + string(rail))
}

type requestKey struct{}
type sourceKey struct{}

// WithRequestID names the request a change is made in. Only the route gate
// calls it.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestKey{}, id)
}

// WithSource names how changes made without an admitted identity arrive:
// cli for operator tooling; job is the default.
func WithSource(ctx context.Context, source billing.LogSource) context.Context {
	return context.WithValue(ctx, sourceKey{}, source)
}

// ProvenanceAt is who the route gate admitted, at at: a staff member or
// application (console when signed in, api otherwise), or a customer; with
// no admitted identity, the system, from the source the context names.
func ProvenanceAt(ctx context.Context, at time.Time) Provenance {
	p := Provenance{At: at.UTC(), Actor: billing.LogActorSystem, Source: billing.LogSourceJob, OperationID: FromContext(ctx).OperationID}
	if id, ok := ctx.Value(requestKey{}).(string); ok {
		p.RequestID = id
	}
	if staff, ok := billingauth.StaffFromContext(ctx); ok {
		p.Actor, p.Source = Staff(billingauth.InvokerKey(staff.Identity)), billing.LogSourceAPI
		if staff.Credential.Kind == billingauth.CredentialSession {
			p.Source = billing.LogSourceConsole
		}
		return p
	}
	if who, ok := billingauth.IdentityFromContext(ctx); ok {
		p.Actor, p.Source = Customer(billingauth.InvokerKey(who)), billing.LogSourceCustomer
		return p
	}
	if source, ok := ctx.Value(sourceKey{}).(billing.LogSource); ok {
		p.Source = source
	}
	return p
}
