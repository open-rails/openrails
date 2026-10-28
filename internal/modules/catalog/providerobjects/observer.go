// Package providerobjects records what each PSP's own catalog says: Stripe
// products and prices (active or not) and NMI plans. It is the only writer of
// billing.provider_catalog_objects. Observations are provider facts, never a
// person's: pulls list the catalog, by-id reads fill what lists lack, and the
// latest observation wins. Nothing here writes to a provider.
package providerobjects

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jonboulle/clockwork"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/integrations/stripeapi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/timeutil"
)

// Kind is what a catalog object is. An NMI plan is a price.
type Kind string

const (
	Product Kind = "product"
	Price   Kind = "price"
)

// Via is how an object was observed.
type Via string

const (
	ViaPull    Via = "pull"
	ViaWebhook Via = "webhook"
	ViaRead    Via = "read"
)

// Object is one provider catalog object. A product carries no terms.
type Object struct {
	Kind Kind
	Ref  string
	// ProductRef is a Stripe price's product.
	ProductRef string
	Name       string
	Active     bool
	// DeletedAt is when a by-id read found the object gone.
	DeletedAt *time.Time
	// Amount is the decimal major-unit amount the provider states, "" when
	// the price has no fixed amount. Currency is "" where the provider
	// names none (NMI).
	Amount   string
	Currency string
	// At most one interval is set.
	IntervalHours  int
	IntervalMonths int
	// Evidence is the provider's object as read.
	Evidence    json.RawMessage
	ObservedAt  time.Time
	ObservedVia Via
	// Retired is set on stored objects: not active, deleted, or its product
	// is. The provider no longer sells it; it keeps resolving and billing.
	Retired bool
}

// ErrNoCatalog is a PSP whose rail has no catalog to read (CCBill).
var ErrNoCatalog = errors.New("the PSP's rail has no provider catalog to read")

// Reader reads one PSP's catalog with read-only clients.
type Reader interface {
	// List is the whole catalog, inactive objects included.
	List(ctx context.Context) ([]Object, error)
	// Get reads one object; found is false when the provider has none.
	Get(ctx context.Context, kind Kind, ref string) (Object, bool, error)
}

// Observer reads PSP catalogs and records what they say for the merchant in
// ctx.
type Observer struct {
	DB        *db.DB
	Merchants *merchants.Service
	// NMI builds NMI clients; Reader makes them read-only.
	NMI *railresolve.NMIFactory
	// Stripe builds write-blocked Stripe clients.
	Stripe *stripeapi.Factory
	Clock  clockwork.Clock
}

// Now is the observer's clock. A caller takes it before reading the provider
// and records what it read at that instant.
func (o *Observer) Now() time.Time { return timeutil.FirstClock(o.Clock).Now().UTC() }

// Reader builds read-only clients from the PSP's credentials.
func (o *Observer) Reader(ctx context.Context, psp merchants.PSPScope) (Reader, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if o.Merchants == nil {
		return nil, errors.New("provider catalog: merchant configuration is not wired")
	}
	switch psp.Rail {
	case "nmi":
		if o.NMI == nil {
			return nil, errors.New("provider catalog: nmi clients are not wired")
		}
		client, err := o.NMI.Client(ctx, o.Merchants.Secrets(), mid, psp)
		if err != nil {
			return nil, err
		}
		client.ReadOnly = true
		return nmiReader{client: client}, nil
	case "stripe":
		if o.Stripe == nil {
			return nil, errors.New("provider catalog: stripe clients are not wired")
		}
		ref, err := psp.SecretRef("secret_key")
		if err != nil {
			return nil, err
		}
		sec, err := merchants.ReadSecretRef(ctx, o.Merchants.Secrets(), mid, ref)
		if err != nil {
			return nil, fmt.Errorf("secret secret_key: %w", err)
		}
		key := strings.TrimSpace(sec.Value)
		if key == "" {
			return nil, errors.New("secret secret_key is empty")
		}
		return newStripeReader(key, o.Stripe.ReadOnlyClient(30*time.Second)), nil
	}
	return nil, ErrNoCatalog
}

// recordBatch bounds one upsert.
const recordBatch = 500

// Record upserts what a read that started at at observed. An object stored
// from a later observation is kept.
func (o *Observer) Record(ctx context.Context, psp merchants.PSPScope, at time.Time, objects []Object, via Via) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	objects = distinct(objects)
	for start := 0; start < len(objects); start += recordBatch {
		p := gen.UpsertProviderCatalogObjectsParams{MerchantID: mid.UUID(), PspID: psp.ID, ObservedAt: at.UTC(), ObservedVia: string(via)}
		for _, obj := range objects[start:min(start+recordBatch, len(objects))] {
			evidence := obj.Evidence
			if len(evidence) == 0 {
				evidence = json.RawMessage(`{}`)
			}
			var deleted time.Time
			if obj.DeletedAt != nil {
				deleted = obj.DeletedAt.UTC()
			}
			p.Kinds, p.Refs, p.ProductRefs, p.Names = append(p.Kinds, string(obj.Kind)), append(p.Refs, obj.Ref), append(p.ProductRefs, obj.ProductRef), append(p.Names, obj.Name)
			p.Actives, p.DeletedAts, p.Amounts, p.Currencies = append(p.Actives, obj.Active), append(p.DeletedAts, deleted), append(p.Amounts, obj.Amount), append(p.Currencies, obj.Currency)
			p.Hours, p.Months, p.Evidence = append(p.Hours, int32(obj.IntervalHours)), append(p.Months, int32(obj.IntervalMonths)), append(p.Evidence, []byte(evidence)) // #nosec G115 -- provider intervals are small
		}
		if len(p.Refs) == 0 {
			continue
		}
		if err := o.DB.Gen(ctx).UpsertProviderCatalogObjects(ctx, p); err != nil {
			return fmt.Errorf("record provider catalog: %w", err)
		}
	}
	return nil
}

// distinct keeps one object per (kind, ref), in (kind, ref) order, so
// concurrent writers lock rows alike. An object a read saw both active and
// not (Stripe moved it between the active and the inactive list) is
// recorded as the later, inactive one.
func distinct(objects []Object) []Object {
	at := map[string]int{}
	out := make([]Object, 0, len(objects))
	for _, obj := range objects {
		if obj.Ref == "" {
			continue
		}
		key := string(obj.Kind) + "\x1f" + obj.Ref
		if i, ok := at[key]; ok {
			if !obj.Active || obj.DeletedAt != nil {
				out[i] = obj
			}
			continue
		}
		at[key] = len(out)
		out = append(out, obj)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Kind != out[j].Kind {
			return out[i].Kind < out[j].Kind
		}
		return out[i].Ref < out[j].Ref
	})
	return out
}

// ObserveByID reads one object at the provider and records it as observed
// via (read, or webhook when one announced it); a Stripe price's product is
// read with it, so an archived or deleted product retires its prices. A read finding the object gone marks a stored object deleted;
// with no stored object it records stated (the terms a provider subscription
// or a provider price states for it) as deleted, else nothing. It returns
// the stored object, nil when none.
func (o *Observer) ObserveByID(ctx context.Context, psp merchants.PSPScope, kind Kind, ref string, stated *Object, via Via) (*Object, error) {
	reader, err := o.Reader(ctx, psp)
	if err != nil {
		return nil, err
	}
	return o.Observe(ctx, reader, psp, kind, ref, stated, via)
}

// Observe is ObserveByID through a Reader the caller already holds.
func (o *Observer) Observe(ctx context.Context, reader Reader, psp merchants.PSPScope, kind Kind, ref string, stated *Object, via Via) (*Object, error) {
	at := o.Now()
	obj, found, err := reader.Get(ctx, kind, ref)
	if err != nil {
		return nil, err
	}
	if found {
		objects := []Object{obj}
		if obj.Kind == Price && obj.ProductRef != "" {
			product, ok, err := reader.Get(ctx, Product, obj.ProductRef)
			if err != nil {
				return nil, err
			}
			if ok {
				objects = append(objects, product)
			} else if err := o.markDeleted(ctx, psp, at, Product, obj.ProductRef, via); err != nil {
				return nil, err
			}
		}
		if err := o.Record(ctx, psp, at, objects, via); err != nil {
			return nil, err
		}
		return o.Get(ctx, psp, kind, ref)
	}
	stored, err := o.Get(ctx, psp, kind, ref)
	if err != nil {
		return nil, err
	}
	switch {
	case stored != nil:
		if err := o.markDeleted(ctx, psp, at, kind, ref, via); err != nil {
			return nil, err
		}
	case stated != nil:
		gone := *stated
		gone.Kind, gone.Ref, gone.DeletedAt = kind, ref, &at
		if err := o.Record(ctx, psp, at, []Object{gone}, via); err != nil {
			return nil, err
		}
	default:
		return nil, nil
	}
	return o.Get(ctx, psp, kind, ref)
}

// markDeleted records that a read at at found a stored object gone.
func (o *Observer) markDeleted(ctx context.Context, psp merchants.PSPScope, at time.Time, kind Kind, ref string, via Via) error {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return err
	}
	_, err = o.DB.Gen(ctx).MarkProviderCatalogObjectDeleted(ctx, gen.MarkProviderCatalogObjectDeletedParams{MerchantID: mid.UUID(), PspID: psp.ID,
		Kind: string(kind), Ref: ref, ObservedAt: at.UTC(), ObservedVia: string(via)})
	return err
}

// Get is the stored object, nil when none was observed.
func (o *Observer) Get(ctx context.Context, psp merchants.PSPScope, kind Kind, ref string) (*Object, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	row, err := o.DB.Gen(ctx).GetProviderCatalogObject(ctx, gen.GetProviderCatalogObjectParams{MerchantID: mid.UUID(), PspID: psp.ID, Kind: string(kind), Ref: ref})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	out := &Object{Kind: Kind(row.Kind), Ref: row.Ref, Active: row.Active, DeletedAt: row.DeletedAt, Evidence: row.Evidence,
		ObservedAt: row.ObservedAt, ObservedVia: Via(row.ObservedVia), Retired: row.Retired}
	out.ProductRef, out.Name, out.Amount, out.Currency = deref(row.ProductRef), deref(row.Name), deref(row.Amount), deref(row.Currency)
	if row.BillingIntervalHours != nil {
		out.IntervalHours = int(*row.BillingIntervalHours)
	}
	if row.BillingIntervalMonths != nil {
		out.IntervalMonths = int(*row.BillingIntervalMonths)
	}
	return out, nil
}

// Refs are every object stored for the PSP, by kind.
func (o *Observer) Refs(ctx context.Context, psp merchants.PSPScope) (map[Kind][]string, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := o.DB.Gen(ctx).ListProviderCatalogRefs(ctx, gen.ListProviderCatalogRefsParams{MerchantID: mid.UUID(), PspID: psp.ID})
	if err != nil {
		return nil, err
	}
	out := map[Kind][]string{}
	for _, r := range rows {
		out[Kind(r.Kind)] = append(out[Kind(r.Kind)], r.Ref)
	}
	return out, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// Bound are the objects live provider prices of the PSP bind (NMI plans,
// Stripe prices), each with the terms its provider price stores.
func (o *Observer) Bound(ctx context.Context, psp merchants.PSPScope) ([]*Object, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := o.DB.Gen(ctx).ListPSPBoundProviderRefs(ctx, gen.ListPSPBoundProviderRefsParams{MerchantID: mid.UUID(), PspID: psp.ID})
	if err != nil {
		return nil, err
	}
	out := make([]*Object, 0, len(rows))
	for _, r := range rows {
		out = append(out, Stated(r.Ref, r.Amount, r.Currency, psp.Rail != "nmi", intOf(r.BillingIntervalHours), intOf(r.BillingIntervalMonths)))
	}
	return out, nil
}
