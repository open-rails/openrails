package reconcile

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/internal/booktag"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
)

// ObjectRefs are the provider references a record of the merchant could name
// an object by.
type ObjectRefs struct {
	Subscription, Customer, Plan, Transaction string
	Reference                                 *uuid.UUID
}

// ProvenanceStore classifies unknown provider objects. A Store that lacks it
// leaves every finding as raised.
type ProvenanceStore interface {
	// BookTag is this book's tag; false when it cannot be read now.
	BookTag(ctx context.Context) (booktag.Tag, bool)
	// ObjectNamed reports whether the PSP moves real money and whether a
	// record of the merchant names the object.
	ObjectNamed(ctx context.Context, pspID uuid.UUID, refs ObjectRefs) (live, named bool, err error)
}

// BookTag is this book's tag.
func (s *PGStore) BookTag(ctx context.Context) (booktag.Tag, bool) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return booktag.Tag{}, false
	}
	return booktag.Ensure(mid.UUID(), s.DB.GenDirectory()).Tag(ctx)
}

// ObjectNamed answers by key lookups: nothing sweeps the merchant's records.
func (s *PGStore) ObjectNamed(ctx context.Context, pspID uuid.UUID, refs ObjectRefs) (bool, bool, error) {
	mid, err := merchant.Require(ctx)
	if err != nil {
		return false, false, err
	}
	text := func(v string) *string {
		if v = strings.TrimSpace(v); v == "" {
			return nil
		}
		return &v
	}
	row, err := s.DB.Gen(ctx).PSPForeignObjectCheck(ctx, gen.PSPForeignObjectCheckParams{
		MerchantID: mid.UUID(), PspID: pspID, SubscriptionRef: text(refs.Subscription), CustomerRef: text(refs.Customer),
		PlanID: text(refs.Plan), TransactionID: text(refs.Transaction), ReferenceID: refs.Reference,
	})
	return row.Live, row.Named, err
}

// provenanceTypes are the findings about a provider object no local record
// names: classified by who created the object.
var provenanceTypes = map[FindingType]bool{
	FindingRemoteSubMissingLocal: true, FindingChargeMissingLocal: true,
	FindingRefundUnrecorded: true, FindingReversalUnlinked: true,
}

// classifyProvenance marks each unknown, unmatched object by whose it is. One
// this book created and lost keeps its severity, alert and hold. A foreign
// one, or a pre-tag one on a PSP that moves no money, is recorded at low
// severity and neither alerts nor holds recovery. A record that names the
// object always keeps today's handling.
func (e *Engine) classifyProvenance(ctx context.Context, pspID uuid.UUID, snap *RemoteSnapshot, findings []Finding) []Finding {
	store, ok := e.Store.(ProvenanceStore)
	if !ok {
		return findings
	}
	var tag booktag.Tag
	var haveTag bool
	subs := map[string]*RemoteSubscription{}
	for i := range snap.Subscriptions {
		subs[snap.Subscriptions[i].RailSubscriptionID] = &snap.Subscriptions[i]
	}
	txns := map[string]*RemoteTransaction{}
	for i := range snap.Transactions {
		if _, dup := txns[snap.Transactions[i].TransactionID]; !dup {
			txns[snap.Transactions[i].TransactionID] = &snap.Transactions[i]
		}
	}
	for i := range findings {
		f := &findings[i]
		if !provenanceTypes[f.Type] || f.Apply != nil || len(f.LocalEvidence) > 0 {
			continue
		}
		var refs ObjectRefs
		var carried string
		var created time.Time
		if f.Type == FindingRemoteSubMissingLocal {
			s := subs[f.SubjectKey]
			if s == nil {
				continue
			}
			refs = ObjectRefs{Subscription: s.RailSubscriptionID, Customer: s.CustomerID, Plan: s.PlanID}
			carried, created = s.BookID, s.CreatedAt
		} else {
			t := txns[f.SubjectKey]
			if t == nil {
				continue
			}
			bc := decodeBreadcrumbs(t.Raw)
			refs = ObjectRefs{Transaction: t.TransactionID, Subscription: t.SubscriptionID, Customer: firstNonEmpty(bc.CustomerVaultID, bc.Customer)}
			if id, ok := parseRebillOrderID(bc.OrderID); ok {
				refs.Reference = &id
			}
			carried, created = t.BookID, t.OccurredAt
		}
		if !haveTag {
			if tag, haveTag = store.BookTag(ctx); !haveTag {
				return findings // unreadable: never quiet on a guess
			}
		}
		live, named, err := store.ObjectNamed(ctx, pspID, refs)
		if err != nil {
			log.WithContext(ctx).WithError(err).Warn("reconcile: provenance check failed; finding kept as raised")
			continue
		}
		if named {
			continue
		}
		provenance := tag.Classify(carried, created)
		f.Provenance = string(provenance)
		if !provenance.Quiet(live) {
			continue
		}
		f.Quiet = true
		f.Severity, f.Status, f.RequiresAdmin = SeverityLow, FindingStatusRequiresReview, false
		f.Apply, f.Change = nil, nil
		f.RecommendedAction = "no record of this merchant names this provider object and this book did not create it (" + string(provenance) + "): informational, it alerts nobody and does not hold provider recovery"
	}
	return findings
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
