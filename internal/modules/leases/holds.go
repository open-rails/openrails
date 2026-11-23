package leases

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/shared/apperr"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
)

// A lease's hold is sized from its traffic, per limit:
//
//   - it covers the customer's demand until the next sync, and at least one
//     request's worth while the customer is far from the limit;
//   - it doubles after running dry (a want for it), and otherwise shrinks
//     toward what the last interval used; the sync that reports usage tops it
//     back up;
//   - one hold takes at most 1/HeadroomShare of what is free of the limit;
//     near the limit a wanted hold holds exactly one request and an idle one
//     is taken back;
//   - a hold another lease was refused capacity_held for shrinks to what it
//     used.
//
// A hold grows at once. A smaller size is answered with a new version and
// set aside until the gate reports that version applied, and never below
// what its open tickets took: capacity a ticket took is never given to
// another gate, even after a lost reply. Whatever the size, no hold takes
// settled + held past capacity plus the overdraft (the credit line, for
// balance).

// slot is one limit of a hold's limit set: its claim in its window (none for
// balance), one request's cost there, and its overrun.
type slot struct {
	key     string
	unit    string
	balance bool
	claim   limits.Claim
	request int64
	overrun catalog.Overrun
}

// moneyRoom is what the customer may set aside of its balance beyond every
// live hold, and the code a want that does not fit it is refused with.
type moneyRoom struct {
	room int64
	code billing.DenyCode
}

func (r *syncRun) want(i int) error {
	w := r.p.Wants[i]
	res := &r.result.Wants[i]
	fail := func(err error) {
		status, d := Details(prefixParam(err, fmt.Sprintf("wants[%d]", i)))
		*res = billing.LeaseWantResult{Status: status, Error: &d}
	}
	currency := money.NormalizeCurrency(string(w.Currency))
	switch {
	case w.CustomerID.IsZero():
		fail(invalid("customer_id", "customer_id required"))
		return nil
	case r.Allows != nil && !r.Allows(w.CustomerID):
		fail(apperr.New(http.StatusForbidden, billing.CodeServiceCredentialCustomerScopeDenied, "the credential's customer scope does not include the customer").WithParam("customer_id"))
		return nil
	case w.InvokerType != "" && w.InvokerType != billing.InvokerTypeCustomer && w.InvokerType != billing.InvokerTypeDelegated:
		fail(invalid("invoker_type", `invoker_type must be "customer" or "delegated"`))
		return nil
	case moneyutil.ValidateCurrency(currency) != nil:
		fail(apperr.New(http.StatusBadRequest, billing.CodeCurrencyUnsupported, "currency is not supported").WithParam("currency"))
		return nil
	case w.Usage != nil && w.EstimatedAmount != nil:
		fail(invalid("usage", "usage and estimated_amount are exclusive: the rate cards price usage"))
		return nil
	case w.EstimatedAmount != nil && *w.EstimatedAmount < 0:
		fail(invalid("estimated_amount", "estimated_amount must be nonnegative"))
		return nil
	case !r.live || r.p.Close:
		fail(apperr.New(http.StatusConflict, billing.CodeLeaseNotHeld, "the lease is closed or stopped syncing; start a new lease"))
		return nil
	}
	customer := w.CustomerID.UUID()
	r.touched[customer] = true
	terms := holdTerms{Invoker: strings.TrimSpace(w.Invoker), InvokerType: string(cmp.Or(w.InvokerType, billing.InvokerTypeCustomer)),
		TrustLevel: strings.TrimSpace(deref(w.TrustLevel)), Roles: w.Roles, Resource: strings.TrimSpace(deref(w.Resource))}
	if w.Usage != nil {
		terms.EventType = strings.TrimSpace(w.Usage.EventType)
	}
	req := terms.request(identity.CustomerID(w.CustomerID), currency)
	req.Usage = w.Usage
	if w.EstimatedAmount != nil {
		req.EstimatedAmount = *w.EstimatedAmount
	}
	resolved, room, err := r.resolve(req, func(capacity money.AdmissionCapacity) (admission.Want, error) {
		return r.admitter.ResolveWant(r.ctx, r.q, req, capacity, r.now)
	})
	if err != nil {
		if _, coded := asCoded(err); coded {
			fail(err)
			return nil
		}
		return err
	}
	if resolved.Refusal != nil {
		refusal := &billing.Refusal{DenyCode: resolved.Refusal.DenyCode}
		if resolved.Refusal.RetryAfterSeconds > 0 {
			refusal.RetryAfterSeconds = &resolved.Refusal.RetryAfterSeconds
		}
		*res = billing.LeaseWantResult{Status: http.StatusOK, Refusal: refusal}
		return nil
	}
	slots := []slot{{key: catalog.BalanceLimit, unit: currency, balance: true, request: resolved.Cost, overrun: catalog.OverrunRequireEstimate}}
	for _, c := range resolved.Spend {
		slots = append(slots, slot{key: c.Key, unit: c.Unit, claim: c, request: c.Cost, overrun: catalog.OverrunRequireEstimate})
	}
	for _, c := range resolved.Limits {
		slots = append(slots, slot{key: c.Key, unit: c.Unit, claim: c, request: c.Cost, overrun: c.Overrun})
	}
	set := LimitSet(currency, terms.EventType, resolved.MoneyTerms(), slotKeys(slots))
	k := holdKey{customer, set}
	if j, ok := r.answered[k]; ok {
		// A second want for the hold in one sync is the first's answer.
		*res = r.result.Wants[j]
		return nil
	}
	res.Status, res.LimitSet = http.StatusOK, &set
	refusal, err := r.size(r.current(k, r.now), k, currency, slots, true, room, resolved.Cards, terms)
	if err != nil {
		return err
	}
	res.Refusal = refusal
	r.answered[k] = i
	return nil
}

// request is the admission request the terms resolve as.
func (t holdTerms) request(customer identity.CustomerID, currency string) admission.AdmitRequest {
	return admission.AdmitRequest{CustomerID: customer, Invoker: t.Invoker, InvokerType: cmp.Or(t.InvokerType, string(billing.InvokerTypeCustomer)),
		TrustLevel: t.TrustLevel, Resource: t.Resource, Roles: t.Roles, Currency: currency, Source: "lease"}
}

func slotKeys(slots []slot) []string {
	keys := make([]string, len(slots))
	for j, s := range slots {
		keys[j] = s.key
	}
	return keys
}

// resolve reads the customer's money capacity under its lock and makes a
// want's or a renewal's checks with it.
func (r *syncRun) resolve(req admission.AdmitRequest, check func(money.AdmissionCapacity) (admission.Want, error)) (admission.Want, moneyRoom, error) {
	capacity, err := r.money.AdmissionCapacityTx(r.ctx, r.q, req.CustomerID, req.Currency)
	if err != nil {
		return admission.Want{}, moneyRoom{}, err
	}
	w, err := check(capacity)
	if err != nil {
		return admission.Want{}, moneyRoom{}, err
	}
	return w, moneyRoom{room: satAdd(w.Available, w.CreditLine), code: w.BalanceRefusal()}, nil
}

// resolveHold makes a renewed hold's checks again, as the invoker it was
// resolved for, with the rate cards in force.
func (r *syncRun) resolveHold(k holdKey, h *hold) (admission.Want, moneyRoom, error) {
	req := h.terms.request(identity.CustomerID(k.customer), h.row.Currency)
	return r.resolve(req, func(capacity money.AdmissionCapacity) (admission.Want, error) {
		return r.admitter.ResolveHold(r.ctx, r.q, req, h.terms.EventType, capacity, r.now)
	})
}

// holdSlots are a renewed hold's limits as its terms resolve now, in their
// windows at an instant: the declared limits and spend windows in force,
// each costing the request its part last recorded. ok is false when a
// limit's window would have to open, which only a want or a settlement
// does.
func (r *syncRun) holdSlots(k holdKey, h *hold, w admission.Want, at time.Time) (slots []slot, set string, ok bool, err error) {
	spend, claims := w.Spend, w.Limits
	if !at.Equal(r.now) {
		if spend, err = w.SpendAt(r.merchant, k.customer, h.row.Currency, at); err != nil {
			return nil, "", false, err
		}
		reader := limits.Customer{Q: r.q, MerchantID: r.merchant, CustomerID: k.customer, Now: at,
			Period: func(ctx context.Context, unit string, at time.Time) (time.Time, time.Time, error) {
				return r.money.PricingPeriod(ctx, r.q, r.merchant, k.customer, unit, at)
			}}
		if claims, err = reader.ClaimsFor(r.ctx, r.defs, r.meters, h.row.Currency, h.terms.EventType); err != nil {
			return nil, "", false, err
		}
	}
	request := func(key string) int64 {
		if p := h.part(key); p != nil {
			return p.request
		}
		return 0
	}
	slots = []slot{{key: catalog.BalanceLimit, unit: h.row.Currency, balance: true, request: request(catalog.BalanceLimit), overrun: catalog.OverrunRequireEstimate}}
	for _, c := range spend {
		slots = append(slots, slot{key: c.Key, unit: c.Unit, claim: c, request: request(c.Key), overrun: catalog.OverrunRequireEstimate})
	}
	for _, c := range claims {
		if c.Opens {
			return nil, "", false, nil
		}
		slots = append(slots, slot{key: c.Key, unit: c.Unit, claim: c, request: request(c.Key), overrun: c.Overrun})
	}
	return slots, LimitSet(h.row.Currency, h.terms.EventType, w.MoneyTerms(), slotKeys(slots)), true, nil
}

// size sizes the hold of k for slots and answers the refusal a wanted hold
// that cannot cover one request beyond its commitment gets; dry is a want.
// The hold is issued again (a fresh hold when h is nil or released) when it
// grew old, its cards changed or a window moved: each part still in its
// window moves to the new hold whole, and the old hold keeps the rest until
// its gate applies the new answer.
func (r *syncRun) size(h *hold, k holdKey, currency string, slots []slot, dry bool, room moneyRoom, cards []byte, terms holdTerms) (*billing.Refusal, error) {
	live := h != nil && !h.released
	reissue := !live || h.row.CreatedAt.Before(r.now.Add(-reissueAge)) || !sameCards(h.row.RateCards, cards) || len(h.parts) != len(slots)
	same := make([]*part, len(slots))
	for j, s := range slots {
		if !live {
			continue
		}
		if p := h.part(s.key); p != nil && (s.balance || p.start.Equal(s.claim.Window.StartsAt)) {
			same[j] = p
		} else {
			reissue = true
		}
	}
	holder := limits.Holder{Q: r.q, MerchantID: r.merchant, CustomerID: k.customer, Now: r.now}
	var claims []limits.Claim
	var at []int
	for j, s := range slots {
		if !s.balance {
			claims = append(claims, s.claim)
			at = append(at, j)
		}
	}
	locked, err := holder.Lock(r.ctx, claims)
	if err != nil {
		return nil, err
	}
	standing := make([]*limits.Standing, len(slots))
	for c, j := range at {
		standing[j] = &locked.Standings[c]
	}
	n := len(slots)
	helds, answers, mines, requests, rooms, commits := make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
	var failing []int
	for j, s := range slots {
		p := same[j]
		mine, used, prev, committed, request := int64(0), int64(0), int64(0), int64(0), s.request
		if p != nil {
			mine, used, prev, committed = p.held, p.used, satAdd(p.answered, p.used), p.committed
			if !dry {
				request = p.request
			}
		}
		room0 := satAdd(room.room, mine)
		if !s.balance {
			st := standing[j]
			room0 = st.Capacity - st.Settled - (st.Held - mine)
		}
		// A want asks for one request beyond what open tickets took.
		need := satAdd(committed, request)
		var target int64
		switch {
		case dry:
			target = max(satAdd(prev, prev), satAdd(committed, used), need)
		case used > 0:
			target = max(satAdd(prev, used)/2, need)
		default:
			target = max(prev/2, need)
		}
		if p != nil && r.contended(p) {
			target = min(target, max(committed, used))
			if dry {
				target = max(target, need)
			}
		}
		if share := room0 / HeadroomShare; share < request {
			// Near the limit a wanted hold holds exactly the wanted units
			// and an idle one gives back what no ticket took.
			target = committed
			if dry {
				target = need
			}
		} else {
			target = min(target, max(share, need))
		}
		held := mine
		if want := max(target, committed); want > mine {
			held = min(want, max(room0, mine))
		}
		// require_estimate takes the whole request beyond what tickets took;
		// admit_while_positive takes any capacity free beyond them.
		fits := held >= need
		if s.overrun == catalog.OverrunAdmitWhilePositive {
			fits = room0-committed > 0 || request == 0
		}
		if dry && !fits {
			failing = append(failing, j)
		}
		helds[j], answers[j], mines[j], requests[j], rooms[j], commits[j] = held, min(target, held), mine, request, room0, committed
	}
	var refusal *billing.Refusal
	if len(failing) > 0 {
		if refusal, err = r.refusal(holder, currency, slots, standing, failing, rooms, requests, commits, room); err != nil {
			return nil, err
		}
		// A refused hold grows nothing and keeps only what tickets took.
		for j := range slots {
			helds[j] = mines[j]
			answers[j] = min(commits[j], helds[j])
		}
	}
	added := make([]int64, len(claims))
	for c, j := range at {
		added[c] = helds[j] - mines[j]
	}
	if err := holder.Commit(r.ctx, locked, added); err != nil {
		return nil, err
	}
	if !reissue {
		changed := false
		for j := range slots {
			p := same[j]
			changed = changed || answers[j] != p.answered
			p.held, p.answered, p.request, p.used = helds[j], answers[j], requests[j], 0
			if err := r.write(h, p); err != nil {
				return nil, err
			}
		}
		// A want's hold is always answered, its size changed or not.
		h.wanted = h.wanted || dry
		h.active = h.active || dry || anyUsed(h)
		h.changed = h.changed || changed || dry
		return refusal, nil
	}
	total := int64(0)
	for _, held := range helds {
		total = satAdd(total, held)
	}
	// Nothing to set aside issues no hold, but for a want that fits: its
	// gate decides its free requests against an empty hold.
	if total == 0 && (refusal != nil || !dry) {
		if live {
			return refusal, r.retire(h)
		}
		return refusal, nil
	}
	fresh, err := r.issue(k, currency, slots, helds, answers, requests, cards, terms)
	if err != nil {
		return nil, err
	}
	fresh.wanted, fresh.active = dry, dry || (live && anyUsed(h))
	for j, p := range fresh.parts {
		p.committed = commits[j]
	}
	if !live {
		return refusal, nil
	}
	// What moved to the new hold leaves the old one; the old one is
	// answered empty and keeps the rest until its gate applies the answer.
	moved := map[*part]bool{}
	for _, p := range same {
		if p != nil {
			moved[p] = true
		}
	}
	empty := true
	for _, p := range h.parts {
		if moved[p] {
			p.held = 0
		}
		p.answered, p.used = 0, 0
		if err := r.write(h, p); err != nil {
			return nil, err
		}
		empty = empty && p.held == 0
	}
	h.changed = true
	if empty {
		h.replaced = true
		return refusal, r.release(h)
	}
	return refusal, nil
}

func anyUsed(h *hold) bool {
	for _, p := range h.parts {
		if p.used > 0 {
			return true
		}
	}
	return false
}

// write stores a part as sized.
func (r *syncRun) write(h *hold, p *part) error {
	return r.q.ResizeLeaseHoldPart(r.ctx, gen.ResizeLeaseHoldPartParams{Held: p.held, RequestUnits: p.request, UsedUnits: p.used, AnsweredUnits: p.answered,
		MerchantID: r.merchant, HoldID: h.row.ID, HoldCreatedAt: h.row.CreatedAt, LimitKey: p.key})
}

// issue writes a new hold of the lease, at the sync's version.
func (r *syncRun) issue(k holdKey, currency string, slots []slot, helds, answers, requests []int64, cards []byte, terms holdTerms) (*hold, error) {
	encoded, err := json.Marshal(terms)
	if err != nil {
		return nil, err
	}
	version := r.nextVersion()
	row, err := r.q.InsertLeaseHold(r.ctx, gen.InsertLeaseHoldParams{MerchantID: r.merchant, CreatedAt: r.now, CustomerID: k.customer, Currency: currency,
		LeaseID: r.lease.ID, LimitSet: k.limitSet, Terms: encoded, RateCards: cards, Version: version, AppliedVersion: 0})
	if err != nil {
		return nil, err
	}
	h := &hold{row: row, terms: terms, version: version, changed: true, fresh: true}
	for j, s := range slots {
		p := &part{key: s.key, unit: s.unit, held: helds[j], answered: answers[j], request: requests[j], overrun: s.overrun, start: limits.Epoch}
		if !s.balance {
			p.start, p.end = s.claim.Window.StartsAt, s.claim.Window.EndsAt
		}
		if err := r.q.InsertLeaseHoldPart(r.ctx, gen.InsertLeaseHoldPartParams{MerchantID: r.merchant, HoldID: row.ID, HoldCreatedAt: row.CreatedAt,
			CustomerID: k.customer, LimitKey: p.key, Unit: p.unit, WindowStartsAt: p.start, WindowEndsAt: p.end,
			Held: p.held, RequestUnits: p.request, UsedUnits: 0, AnsweredUnits: p.answered}); err != nil {
			return nil, err
		}
		h.parts = append(h.parts, p)
	}
	r.holds[k] = append(r.holds[k], h)
	return h, nil
}

// refusal is why a want does not fit: the first failing slot that would not
// fit even were other leases' holds, committed or not, returned refuses it
// with its limit's own code; when every one would, it is capacity_held and
// those holds shrink on their next sync. A request fits by its overrun: the
// whole request beyond what tickets took (require_estimate), or any capacity
// free beyond them (admit_while_positive).
func (r *syncRun) refusal(holder limits.Holder, currency string, slots []slot, standing []*limits.Standing, failing []int, free, request, committed []int64, room moneyRoom) (*billing.Refusal, error) {
	var short []limits.Returnable
	for _, j := range failing {
		s := slots[j]
		ret := limits.BalanceReturnable(currency, r.lease.ID)
		if !s.balance {
			ret = limits.ReturnableOf(s.claim, r.now, r.lease.ID)
		}
		others, err := holder.LeaseHeld(r.ctx, ret)
		if err != nil {
			return nil, err
		}
		beyond := satAdd(satAdd(free[j], others), -committed[j])
		fits := request[j] <= beyond
		if s.overrun == catalog.OverrunAdmitWhilePositive {
			fits = beyond > 0
		}
		if others > 0 && fits {
			short = append(short, ret)
			continue
		}
		switch {
		case s.balance:
			return &billing.Refusal{DenyCode: room.code}, nil
		case strings.HasPrefix(s.key, limits.SpendKeyPrefix):
			out := &billing.Refusal{DenyCode: billing.DenyBudgetExceeded}
			if s.claim.Window.EndsAt != nil {
				retry := ceilSeconds(s.claim.Window.EndsAt.Sub(r.now))
				out.RetryAfterSeconds = &retry
			}
			return out, nil
		}
		key := s.key
		out := &billing.Refusal{DenyCode: billing.DenyLimitReached, Limit: &key, ResetsAt: holder.ResetsAt(r.ctx, s.claim, *standing[j])}
		if out.ResetsAt != nil {
			retry := ceilSeconds(out.ResetsAt.Sub(r.now))
			out.RetryAfterSeconds = &retry
		}
		return out, nil
	}
	for _, ret := range short {
		if err := holder.TakeBack(r.ctx, ret); err != nil {
			return nil, err
		}
	}
	retry := int64(limits.SyncInterval / time.Second)
	return &billing.Refusal{DenyCode: billing.DenyCapacityHeld, RetryAfterSeconds: &retry}, nil
}

// renew keeps the live lease's other holds sized: one that reported usage is
// topped up, one whose window closed, that grew old or whose cards changed
// is issued again, one whose customer's standing moved is sized again (near
// the limit an idle one is taken back), one another lease waits for
// shrinks, and an idle one shrinks toward one request. Before a fixed window
// closes, an active hold gets its next window's hold. Only the holds of
// customers the sync locked are touched, and at most maxRenewals of them,
// most urgent first, while the sync is within its work budget; the rest wait
// for later syncs.
func (r *syncRun) renew() error {
	tasks, err := r.rank(func(k holdKey) bool { return r.locked[k.customer] })
	if err != nil {
		return err
	}
	tasks = tasks[:min(len(tasks), maxRenewals)]
	// The chosen run in key order, each key's holds before its roll-ahead.
	slices.SortFunc(tasks, func(a, b task) int {
		return cmp.Or(compareUUID(a.key.customer, b.key.customer), strings.Compare(a.key.limitSet, b.key.limitSet), compareTasks(a, b))
	})
	for _, t := range tasks {
		if over, err := r.pastBudget(workBudget); err != nil || over {
			return err
		}
		if t.hold == nil {
			if err := r.rollAhead(t.key); err != nil {
				return err
			}
			continue
		}
		if t.hold.released || t.hold.wanted || t.hold.fresh {
			continue
		}
		if err := r.renewHold(t.key, t.hold); err != nil {
			return err
		}
	}
	return nil
}

func (r *syncRun) renewHold(k holdKey, h *hold) error {
	used, contended, ended := false, false, false
	for _, p := range h.parts {
		used = used || p.used > 0
		contended = contended || r.contended(p)
		ended = ended || (p.end != nil && !r.now.Before(*p.end))
	}
	old := h.row.CreatedAt.Before(r.now.Add(-reissueAge))
	if ended && !used && !old && !committed(h) {
		// An idle hold whose window closed is not carried into the next.
		return r.retire(h)
	}
	cardsChanged, err := r.cardsChanged(h)
	if err != nil {
		return err
	}
	if used || old || ended || cardsChanged || r.touched[k.customer] || r.moved[k.customer] {
		w, room, err := r.resolveHold(k, h)
		if err != nil {
			return err
		}
		if w.Refusal != nil {
			return r.shrink(h, func(p *part) int64 { return p.committed })
		}
		if h.terms.EventType != "" && len(w.Cards) == 0 {
			// No card prices the hold's event type any more.
			return r.retire(h)
		}
		slots, set, ok, err := r.holdSlots(k, h, w, r.now)
		if err != nil {
			return err
		}
		if !ok || set != k.limitSet {
			// Its limits changed, or a window would have to open: the next
			// want resolves the hold afresh.
			return r.retire(h)
		}
		_, err = r.size(h, k, h.row.Currency, slots, false, room, w.Cards, h.terms)
		return err
	}
	// Shrinking never needs room: a contended hold gives back what its
	// tickets did not take, an idle one halves toward one request beyond them.
	return r.shrink(h, func(p *part) int64 {
		if contended {
			return max(p.committed, p.used)
		}
		return max(p.answered/2, satAdd(p.committed, p.request))
	})
}

// committed reports whether open tickets took any of the hold.
func committed(h *hold) bool {
	for _, p := range h.parts {
		if p.committed > 0 {
			return true
		}
	}
	return false
}

// shrink answers each part at most what to says, never below what its
// tickets took; what it set aside beyond is released once its gate applies
// the answer.
func (r *syncRun) shrink(h *hold, to func(*part) int64) error {
	for _, p := range h.parts {
		answered := min(p.answered, max(to(p), p.committed))
		if answered == p.answered && p.used == 0 {
			continue
		}
		h.changed = h.changed || answered != p.answered
		p.answered, p.used = answered, 0
		if err := r.write(h, p); err != nil {
			return err
		}
	}
	return nil
}

// retire answers a hold empty: it is released once its gate applies the
// answer, down to what its open tickets took until they settle.
func (r *syncRun) retire(h *hold) error {
	empty := true
	for _, p := range h.parts {
		if p.answered != 0 || p.used != 0 {
			h.changed = h.changed || p.answered != 0
			p.answered, p.used = 0, 0
			if err := r.write(h, p); err != nil {
				return err
			}
		}
		empty = empty && p.held == 0
	}
	if empty {
		return r.release(h)
	}
	return nil
}

// rollAheadDue reports a key whose active hold has a fixed window closing
// before the lease syncs again that no other hold of the key covers.
func (r *syncRun) rollAheadDue(k holdKey) bool {
	_, closes := r.rollAheadOf(k)
	return closes != nil
}

func (r *syncRun) rollAheadOf(k holdKey) (*hold, *time.Time) {
	h := r.current(k, r.now)
	if h == nil || h.released || !h.active {
		return nil, nil
	}
	var closes *time.Time
	for _, p := range h.parts {
		if p.end == nil || p.key == catalog.BalanceLimit {
			continue
		}
		if r.tiles(p.key) && p.end.After(r.now) && !p.end.After(r.now.Add(rolloverAhead)) && (closes == nil || p.end.Before(*closes)) {
			end := *p.end
			closes = &end
		}
	}
	if closes == nil {
		return nil, nil
	}
	for _, other := range r.holds[k] {
		if other != h && !other.released && other.contains(*closes) {
			return nil, nil
		}
	}
	return h, closes
}

// rollAhead issues an active hold's next-window hold when one of its fixed
// windows closes before the lease syncs again, so the reset does not send
// every active customer to a want at once. It opens no window, and a hold
// whose checks now refuse it gets none.
func (r *syncRun) rollAhead(k holdKey) error {
	h, closes := r.rollAheadOf(k)
	if closes == nil {
		return nil
	}
	w, room, err := r.resolveHold(k, h)
	if err != nil || w.Refusal != nil {
		return err
	}
	if h.terms.EventType != "" && len(w.Cards) == 0 {
		return nil
	}
	slots, set, ok, err := r.holdSlots(k, h, w, *closes)
	if err != nil || !ok || set != k.limitSet {
		return err
	}
	_, err = r.size(nil, k, h.row.Currency, slots, false, room, w.Cards, h.terms)
	return err
}

// tiles reports whether a limit's windows tile the clock (a fixed or
// billing-period limit, or a spend window), so the next one is known before
// the current one closes.
func (r *syncRun) tiles(key string) bool {
	if strings.HasPrefix(key, limits.SpendKeyPrefix) {
		return true
	}
	d, ok := r.def(key)
	return ok && (d.Reset.Kind == catalog.ResetFixed || d.Reset.Kind == catalog.ResetBillingPeriod)
}

// answer lists the holds the sync changed, and while the lease lives every
// hold whose latest answer its gate has not reported applied, then the
// customers' limits. A hold whose capacity moved to a new one is answered by
// it.
func (r *syncRun) answer() error {
	keys := make([]holdKey, 0, len(r.holds))
	for k := range r.holds {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b holdKey) int {
		return cmp.Or(compareUUID(a.customer, b.customer), strings.Compare(a.limitSet, b.limitSet))
	})
	for _, k := range keys {
		for _, h := range r.holds[k] {
			listed := h.changed || (r.live && !h.released && h.version > h.applied)
			if !listed || h.replaced {
				continue
			}
			out, ok, err := r.wire(k, h)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			r.result.Holds = append(r.result.Holds, out)
			r.touched[k.customer] = true
		}
	}
	customers := make([]uuid.UUID, 0, len(r.touched))
	for c := range r.touched {
		customers = append(customers, c)
	}
	slices.SortFunc(customers, compareUUID)
	if len(r.defs) == 0 {
		return nil
	}
	for _, c := range customers {
		reader := limits.Customer{Q: r.q, MerchantID: r.merchant, CustomerID: c, Now: r.now,
			Period: func(ctx context.Context, unit string, at time.Time) (time.Time, time.Time, error) {
				return r.money.PricingPeriod(ctx, r.q, r.merchant, c, unit, at)
			}}
		lines, err := reader.List(r.ctx)
		if err != nil {
			return err
		}
		r.result.Limits = append(r.result.Limits, lines...)
	}
	return nil
}

// wire is a hold as its gate reads it, at its version; a released hold
// holds nothing. With the merchant's usage_gate_safe_spend, its safe amount
// is its money size. A hold without a balance part is never answered.
func (r *syncRun) wire(k holdKey, h *hold) (billing.Hold, bool, error) {
	if h.part(catalog.BalanceLimit) == nil {
		return billing.Hold{}, false, nil
	}
	cards, bounds, err := money.HoldCards(h.row.RateCards)
	if err != nil {
		return billing.Hold{}, false, err
	}
	safe, err := r.safeSpend()
	if err != nil {
		return billing.Hold{}, false, err
	}
	out := billing.Hold{CustomerID: billing.CustomerID(k.customer), LimitSet: k.limitSet, Currency: catalog.Currency(h.row.Currency),
		Limits: make([]billing.HoldLimit, 0, len(h.parts)), RateCards: cards, PriceBounds: bounds, Version: h.version}
	for _, p := range h.parts {
		line := billing.HoldLimit{Limit: p.key, Unit: p.unit, Held: p.answered, Overrun: cmp.Or(p.overrun, catalog.OverrunRequireEstimate)}
		if h.released {
			line.Held = 0
		}
		if safe && p.key == catalog.BalanceLimit {
			amount := line.Held
			out.Safe = &amount
		}
		if p.end != nil {
			start, end := p.start, *p.end
			line.WindowStartsAt, line.WindowEndsAt = &start, &end
		}
		if def, ok := r.def(p.key); ok {
			line.Overrun = def.Overrun
			if !catalog.LimitUnitCurrency(def.Unit) {
				for _, card := range def.RateCards {
					m := r.meters[card.Meter]
					key := def.Key
					out.RateCards = append(out.RateCards, billing.HoldRateCard{Limit: &key, Meter: card.Meter, EventType: m.EventType,
						ValueProperty: m.ValueProperty, Aggregation: m.Aggregation, GroupBy: m.GroupBy, Filter: card.Filter, Price: card.Price})
				}
			}
		}
		out.Limits = append(out.Limits, line)
	}
	return out, true, nil
}

// safeSpend reports the merchant's usage_gate_safe_spend, read once a sync.
func (r *syncRun) safeSpend() (bool, error) {
	if r.safe == nil {
		cfg, _, err := merchantconfig.NewStore(r.d).Get(r.ctx)
		if err != nil {
			return false, err
		}
		r.safe = &cfg.UsageGateSafeSpend
	}
	return *r.safe, nil
}

func (r *syncRun) def(key string) (limits.Definition, bool) {
	for _, d := range r.defs {
		if d.Key == key {
			return d, true
		}
	}
	return limits.Definition{}, false
}

func ceilSeconds(d time.Duration) int64 {
	if d <= 0 {
		return 0
	}
	s := int64(d / time.Second)
	if d%time.Second > 0 {
		s++
	}
	return s
}

func satAdd(a, b int64) int64 {
	const maxInt64 = int64(^uint64(0) >> 1)
	switch {
	case b > 0 && a > maxInt64-b:
		return maxInt64
	case b < 0 && a < -maxInt64-1-b:
		return -maxInt64 - 1
	}
	return a + b
}
