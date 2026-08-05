package billingimport

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/gen"
)

// pspResolver turns a declared PSPRef into a real openrails.psps id, against the
// merchant's own catalog. or#893: an import that cannot attribute a row is
// REFUSED — writing it unattributed is what made a legacy row invisible to a
// PSP-scoped prune and collidable with a sibling account's provider ids.
type pspResolver struct {
	byID  map[uuid.UUID]gen.OpenrailsPsp
	byKey map[string]gen.OpenrailsPsp // "<rail>\x1f<key>"
	// soleByRail is the merchant's single live PSP on a rail, when there is
	// exactly one. It is NOT a fallback for a missing ref — only the resolution
	// of an ambiguity-free explicit default.
	fallback PSPRef
	known    []string
}

func newPSPResolver(ctx context.Context, q *gen.Queries, merchantID uuid.UUID, fallback PSPRef) (*pspResolver, error) {
	rows, err := q.ListPSPsForMerchant(ctx, gen.ListPSPsForMerchantParams{MerchantID: merchantID})
	if err != nil {
		return nil, fmt.Errorf("import billing: read PSP catalog: %w", err)
	}
	r := &pspResolver{
		byID:     make(map[uuid.UUID]gen.OpenrailsPsp, len(rows)),
		byKey:    make(map[string]gen.OpenrailsPsp, len(rows)),
		fallback: fallback,
	}
	for _, p := range rows {
		r.byID[p.ID] = p
		if p.Key != nil && strings.TrimSpace(*p.Key) != "" {
			r.byKey[pspKeyIndex(p.Rail, *p.Key)] = p
			r.known = append(r.known, p.Rail+"/"+strings.TrimSpace(*p.Key))
		} else {
			r.known = append(r.known, p.Rail+"/"+p.ID.String())
		}
	}
	sort.Strings(r.known)
	return r, nil
}

func pspKeyIndex(rail, key string) string {
	return strings.ToLower(strings.TrimSpace(rail)) + "\x1f" + strings.ToLower(strings.TrimSpace(key))
}

// resolve attributes one declared row. subject names the row in the refusal so
// an operator fixing a 50k-row book knows which line to fix.
func (r *pspResolver) resolve(ref PSPRef, rail, subject string) (uuid.UUID, error) {
	if ref.IsZero() {
		ref = r.fallback
	}
	if ref.IsZero() {
		return uuid.Nil, fmt.Errorf(
			"import billing: %s declares no PSP and no Options.DefaultPSP was given — a provider row must state which account it came from (known PSPs: %s)",
			subject, r.knownList())
	}
	if ref.ID != nil && *ref.ID != uuid.Nil {
		p, ok := r.byID[*ref.ID]
		if !ok {
			return uuid.Nil, fmt.Errorf("import billing: %s names PSP %s, which this merchant does not own", subject, *ref.ID)
		}
		if !railMatches(p.Rail, rail) {
			return uuid.Nil, fmt.Errorf("import billing: %s is on rail %q but PSP %s is on rail %q", subject, rail, *ref.ID, p.Rail)
		}
		return p.ID, nil
	}
	p, ok := r.byKey[pspKeyIndex(rail, ref.Key)]
	if !ok {
		return uuid.Nil, fmt.Errorf(
			"import billing: %s names PSP key %q on rail %q, which this merchant does not own (known PSPs: %s)",
			subject, ref.Key, rail, r.knownList())
	}
	return p.ID, nil
}

func railMatches(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

func (r *pspResolver) knownList() string {
	if len(r.known) == 0 {
		return "none declared"
	}
	return strings.Join(r.known, ", ")
}
