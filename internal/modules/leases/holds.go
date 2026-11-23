package leases

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/catalog"
	identity "github.com/open-rails/openrails/internal/billingidentity"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/modules/admission"
	"github.com/open-rails/openrails/internal/modules/limits"
	"github.com/open-rails/openrails/internal/modules/merchantconfig"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/retention"
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
// Whatever the size, no hold takes settled + held past capacity plus the
// overdraft (the credit line, for balance).

// slot is one limit of a hold's limit set: its claim in the current window
// (none for balance), one request's cost there, and its overrun.
type slot struct {
	key     string
	unit    string
	balance bool
	claim   limits.Claim
	request int64
	overrun catalog.Overrun
}

// moneyRoom is what the customer may set aside of its balance: the room
// counts the hold's own balance part as it stood when it was read.
type moneyRoom struct {
	room    int64
	counted int64
	code    string
}

func (r *syncRun) want(i int) error {
	w := r.p.Wants[i]
	res := &r.result.Wants[i]
	fail := func(err error) {
		status, d := Details(prefixParam(err, fmt.Sprintf("wants[%d]", i)))
		*res = billing.LeaseWantResult{Status: status, Error: &d}
	}
	currency := money.NormalizeCurrency(w.Currency)
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
		fail(apperr.New(http.StatusBadRequest, "currency_unsupported", "currency is not supported").WithParam("currency"))
		return nil
	case !r.live:
		fail(apperr.New(http.StatusConflict, billing.CodeLeaseNotHeld, "the lease is closed or stopped syncing; start a new lease"))
		return nil
	}
	customer := w.CustomerID.UUID()
	r.touched[customer] = true
	resolved, room, err := r.resolve(identity.CustomerID(w.CustomerID), admission.AdmitRequest{
		CustomerID: identity.CustomerID(w.CustomerID), Invoker: strings.TrimSpace(w.Invoker), InvokerType: string(cmp.Or(w.InvokerType, billing.InvokerTypeCustomer)),
		TrustLevel: strings.TrimSpace(deref(w.TrustLevel)), Resource: strings.TrimSpace(deref(w.Resource)), Roles: w.Roles, Currency: currency, Usage: w.Usage, Source: "lease",
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
	eventType := ""
	if w.Usage != nil {
		eventType = strings.TrimSpace(w.Usage.EventType)
	}
	keys := make([]string, len(slots))
	for j, s := range slots {
		keys[j] = s.key
	}
	set := LimitSet(currency+"\x00"+eventType, keys)
	res.Status, res.LimitSet = http.StatusOK, &set
	k := holdKey{customer, set}
	h := r.current(k, r.now)
	if h != nil && h.wanted {
		// A second want for the hold in one sync is the first's answer.
		for j := 0; j < i; j++ {
			if prev := r.result.Wants[j]; prev.LimitSet != nil && *prev.LimitSet == set && prev.Error == nil {
				*res = prev
				return nil
			}
		}
	}
	refusal, err := r.size(h, k, currency, slots, true, room, resolved.Cards)
	if err != nil {
		return err
	}
	res.Refusal = refusal
	return nil
}

// resolve makes a want's checks and reads the customer's money room. A hold
// sized without a want resolves the customer's own credential.
func (r *syncRun) resolve(customer identity.CustomerID, req admission.AdmitRequest) (admission.Want, moneyRoom, error) {
	capacity, err := r.money.AdmissionCapacityTx(r.ctx, r.q, customer, req.Currency)
	if err != nil {
		return admission.Want{}, moneyRoom{}, err
	}
	w, err := r.admitter.ResolveWant(r.ctx, r.q, req, capacity, r.now)
	if err != nil {
		return admission.Want{}, moneyRoom{}, err
	}
	room := moneyRoom{room: satAdd(w.Available, w.CreditLine), code: w.BalanceRefusal()}
	return w, room, nil
}

// counted is what of the hold's balance part the money room already counts
// as held: all of it, while the hold is live.
func counted(h *hold) int64 {
	if h == nil || h.released {
		return 0
	}
	if p := h.part(catalog.BalanceLimit); p != nil {
		return p.held
	}
	return 0
}

// size sizes the hold of k for slots (a fresh hold when h is nil, released,
// too old or in windows that ended) and answers the refusal a wanted hold
// that cannot cover one request beyond its commitment gets. dry is a want:
// the hold ran dry. A hold grows at once; it shrinks in two steps: the
// answer gives its gate the smaller size, and the next sync releases what
// that sync's commitment leaves above it (release).
func (r *syncRun) size(h *hold, k holdKey, currency string, slots []slot, dry bool, room moneyRoom, cards []byte) (*billing.Refusal, error) {
	room.counted = counted(h)
	reissue := h == nil || h.released || h.row.CreatedAt.Before(r.now.Add(-reissueAge)) || len(h.parts) != len(slots)
	if !reissue {
		for _, s := range slots {
			p := h.part(s.key)
			if p == nil || (!s.balance && !p.start.Equal(s.claim.Window.StartsAt)) {
				reissue = true
				break
			}
		}
	}
	carried := map[string]*part{}
	if h != nil {
		for _, p := range h.parts {
			carried[p.key] = p
		}
		if cards == nil {
			cards = h.row.RateCards
		}
	}
	if reissue && h != nil && !h.released {
		if err := r.release(h); err != nil {
			return nil, err
		}
		h.changed = true
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
	helds, answers, mines, requests, needs, rooms := make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n), make([]int64, n)
	failing := -1
	for j, s := range slots {
		p := carried[s.key]
		mine, used, prev, committed := int64(0), int64(0), int64(0), int64(0)
		request := s.request
		if p != nil {
			used, prev, committed = p.used, satAdd(p.answered, p.used), p.committed
			if !reissue {
				mine = p.held
			}
			if !dry {
				request = p.request
			}
		}
		var room0 int64
		if s.balance {
			room0 = satAdd(room.room, room.counted)
		} else {
			st := standing[j]
			room0 = st.Capacity - st.Settled - (st.Held - mine)
		}
		// A want asks for one request beyond what open tickets took.
		need := satAdd(committed, request)
		target := need
		switch {
		case dry:
			target = max(2*prev, satAdd(committed, used), need)
		case used > 0:
			target = max((prev+used)/2, need)
		default:
			target = max(prev/2, need)
		}
		if p != nil && p.contended != nil && p.contended.After(r.prevSync.Add(-limits.SyncInterval)) {
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
		fits := held >= need
		if s.overrun == catalog.OverrunAdmitWhilePositive {
			fits = room0 > committed || request == 0
		}
		if dry && !fits && failing < 0 {
			failing = j
		}
		helds[j], answers[j], mines[j], requests[j], needs[j], rooms[j] = held, min(target, held), mine, request, need, room0
	}
	var refusal *billing.Refusal
	if failing >= 0 {
		if refusal, err = r.refusal(holder, k, currency, slots[failing], standing[failing], rooms[failing], needs[failing], room); err != nil {
			return nil, err
		}
		// A refused hold grows nothing and keeps only what tickets took.
		for j, s := range slots {
			committed := int64(0)
			if p := carried[s.key]; p != nil {
				committed = p.committed
			}
			helds[j] = mines[j]
			if reissue {
				helds[j] = min(committed, max(rooms[j], 0))
			}
			answers[j] = min(committed, helds[j])
		}
	}
	added := make([]int64, len(claims))
	for c, j := range at {
		added[c] = helds[j] - mines[j]
	}
	if err := holder.Commit(r.ctx, locked, added); err != nil {
		return nil, err
	}
	if reissue {
		total := int64(0)
		for _, held := range helds {
			total = satAdd(total, held)
		}
		if total == 0 && (refusal != nil || !dry) {
			return refusal, nil
		}
		fresh, err := r.issue(k, currency, slots, helds, answers, requests, cards)
		if err != nil {
			return nil, err
		}
		fresh.wanted, fresh.active = dry, dry || p0used(carried)
		return refusal, nil
	}
	changed := false
	for j, s := range slots {
		p := h.part(s.key)
		changed = changed || answers[j] != p.answered || helds[j] != p.held
		p.held, p.answered, p.request, p.used, p.contended = helds[j], answers[j], requests[j], 0, nil
		p.overrun = s.overrun
		if err := r.write(h, p); err != nil {
			return nil, err
		}
	}
	h.wanted = h.wanted || dry
	h.active = h.active || dry || p0used(carried)
	h.changed = h.changed || changed
	return refusal, nil
}

// write stores a part as sized; its usage since the last sync is counted.
func (r *syncRun) write(h *hold, p *part) error {
	return r.q.ResizeLeaseHoldPart(r.ctx, gen.ResizeLeaseHoldPartParams{Held: p.held, RequestUnits: p.request, UsedUnits: 0, AnsweredUnits: p.answered,
		MerchantID: r.merchant, HoldID: h.row.ID, HoldCreatedAt: h.row.CreatedAt, LimitKey: p.key})
}

// issue writes a new hold of the lease.
func (r *syncRun) issue(k holdKey, currency string, slots []slot, helds, answers, requests []int64, cards []byte) (*hold, error) {
	row, err := r.q.InsertLeaseHold(r.ctx, gen.InsertLeaseHoldParams{MerchantID: r.merchant, CreatedAt: r.now, CustomerID: k.customer, Currency: currency,
		LeaseID: r.lease.ID, LimitSet: k.limitSet, RateCards: cards})
	if err != nil {
		return nil, err
	}
	h := &hold{row: row, changed: true, fresh: true}
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

// refusal is why a want does not fit slot s: capacity_held when its need
// would fit were other leases' holds, committed or not, returned (they
// shrink on their next sync), else the limit's own refusal.
func (r *syncRun) refusal(holder limits.Holder, k holdKey, currency string, s slot, st *limits.Standing, free, need int64, room moneyRoom) (*billing.Refusal, error) {
	from, to := limits.Epoch, limits.Epoch
	if !s.balance {
		from, to = s.claim.Window.StartsAt, s.claim.Window.StartsAt
		if s.claim.Reset.Kind == catalog.ResetRolling {
			from = limits.RollingFrom(s.claim.Reset.WindowSeconds, r.now)
		}
	}
	args := gen.SumOtherLeaseHeldParams{MerchantID: r.merchant, CustomerID: k.customer, LimitKey: s.key, Currency: currency, FromWindow: from, ToWindow: to,
		HeldSince: r.now.Add(-retention.AdmissionMaxHold), LeaseID: r.lease.ID, LeaseLiveSince: r.now.Add(-limits.LeaseLifetime)}
	others, err := r.q.SumOtherLeaseHeld(r.ctx, args)
	if err != nil {
		return nil, err
	}
	if others > 0 && need <= satAdd(free, others) {
		if err := r.q.MarkLeaseHoldsContended(r.ctx, gen.MarkLeaseHoldsContendedParams{Now: r.now, MerchantID: args.MerchantID, CustomerID: args.CustomerID,
			LimitKey: args.LimitKey, Currency: args.Currency, FromWindow: args.FromWindow, ToWindow: args.ToWindow, HeldSince: args.HeldSince,
			LeaseID: args.LeaseID, LeaseLiveSince: args.LeaseLiveSince}); err != nil {
			return nil, err
		}
		retry := int64(limits.SyncInterval / time.Second)
		return &billing.Refusal{DenyCode: billing.DenyCapacityHeld, RetryAfterSeconds: &retry}, nil
	}
	switch {
	case s.balance:
		return &billing.Refusal{DenyCode: room.code}, nil
	case strings.HasPrefix(s.key, limits.SpendKeyPrefix):
		out := &billing.Refusal{DenyCode: admission.DenyBudgetExceeded}
		if s.claim.Window.EndsAt != nil {
			retry := ceilSeconds(s.claim.Window.EndsAt.Sub(r.now))
			out.RetryAfterSeconds = &retry
		}
		return out, nil
	}
	key := s.key
	out := &billing.Refusal{DenyCode: billing.DenyLimitReached, Limit: &key, ResetsAt: holder.ResetsAt(r.ctx, s.claim, *st)}
	if out.ResetsAt != nil {
		retry := ceilSeconds(out.ResetsAt.Sub(r.now))
		out.RetryAfterSeconds = &retry
	}
	return out, nil
}

// renew keeps the live lease's other holds sized: one that reported usage is
// topped up, one whose window closed or that grew old is issued again, one
// whose customer's standing moved is sized again (near the limit an idle one
// is taken back), one another lease waits for shrinks, and an idle one
// shrinks toward one request. Before a fixed window closes, an active hold
// gets its next window's hold.
func (r *syncRun) renew() error {
	keys := make([]holdKey, 0, len(r.holds))
	for k := range r.holds {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b holdKey) int {
		return cmp.Or(compareUUID(a.customer, b.customer), strings.Compare(a.limitSet, b.limitSet))
	})
	for _, k := range keys {
		for _, h := range slices.Clone(r.holds[k]) {
			if h.released || h.wanted || h.fresh {
				continue
			}
			if err := r.renewHold(k, h); err != nil {
				return err
			}
		}
		if err := r.rollAhead(k); err != nil {
			return err
		}
	}
	return nil
}

func (r *syncRun) renewHold(k holdKey, h *hold) error {
	used, contended, ended := false, false, false
	for _, p := range h.parts {
		used = used || p.used > 0
		contended = contended || (p.contended != nil && p.contended.After(r.prevSync.Add(-limits.SyncInterval)))
		ended = ended || (p.end != nil && !r.now.Before(*p.end))
	}
	old := h.row.CreatedAt.Before(r.now.Add(-reissueAge))
	if ended && !used && !old && !committed(h) {
		// An idle hold whose window closed is not carried into the next.
		h.changed = true
		return r.release(h)
	}
	if used || old || ended || r.touched[k.customer] || r.moved[k.customer] {
		slots, err := r.slotsAt(k, h, r.now)
		if err != nil {
			return err
		}
		if slots == nil {
			h.changed = true
			return r.release(h)
		}
		w, room, err := r.resolve(identity.CustomerID(k.customer), admission.AdmitRequest{CustomerID: identity.CustomerID(k.customer),
			InvokerType: string(billing.InvokerTypeCustomer), Currency: h.row.Currency, Source: "lease"})
		if err != nil {
			return err
		}
		if w.Refusal != nil {
			return r.shrink(h, func(p *part) int64 { return p.committed })
		}
		_, err = r.size(h, k, h.row.Currency, slots, false, room, nil)
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
// tickets took; what it set aside is released at the next sync.
func (r *syncRun) shrink(h *hold, to func(*part) int64) error {
	for _, p := range h.parts {
		answered := min(p.answered, max(to(p), p.committed))
		if answered == p.answered && p.used == 0 && p.contended == nil {
			continue
		}
		if answered != p.answered {
			h.changed = true
		}
		p.answered, p.used, p.contended = answered, 0, nil
		if err := r.write(h, p); err != nil {
			return err
		}
	}
	return nil
}

// releaseAll is the second step of every live hold's last shrink: what it set
// aside beyond the size its gate was told, past what this sync's tickets
// took, is given back.
func (r *syncRun) releaseAll() error {
	for _, holds := range r.holds {
		for _, h := range holds {
			for _, p := range h.parts {
				if keep := max(p.answered, p.committed); p.held > keep {
					p.held = keep
					if err := r.q.ResizeLeaseHoldPart(r.ctx, gen.ResizeLeaseHoldPartParams{Held: p.held, RequestUnits: p.request, UsedUnits: p.used,
						AnsweredUnits: p.answered, ContendedAt: p.contended, MerchantID: r.merchant, HoldID: h.row.ID, HoldCreatedAt: h.row.CreatedAt, LimitKey: p.key}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// slotsAt are a hold's limits in their windows at an instant: a declared
// limit's current window, a spend window's tile; nil when a limit the hold
// names is no longer declared.
func (r *syncRun) slotsAt(k holdKey, h *hold, at time.Time) ([]slot, error) {
	reader := limits.Customer{Q: r.q, MerchantID: r.merchant, CustomerID: k.customer, Now: at,
		Period: func(ctx context.Context, unit string, at time.Time) (time.Time, time.Time, error) {
			return r.money.PricingPeriod(ctx, r.q, r.merchant, k.customer, unit, at)
		}}
	var windows map[string]limits.Claim
	if len(r.defs) > 0 {
		var err error
		if windows, err = reader.Windows(r.ctx, r.defs); err != nil {
			return nil, err
		}
	}
	out := make([]slot, 0, len(h.parts))
	for _, p := range h.parts {
		s := slot{key: p.key, unit: p.unit, request: p.request, overrun: catalog.OverrunRequireEstimate}
		switch {
		case p.key == catalog.BalanceLimit:
			s.balance = true
		case strings.HasPrefix(p.key, limits.SpendKeyPrefix):
			if p.end == nil {
				return nil, nil
			}
			width := p.end.Sub(p.start)
			start := p.start
			if !at.Before(*p.end) {
				start = p.start.Add(at.Sub(p.start) / width * width)
			}
			end := start.Add(width)
			capacity := int64(0)
			row, err := r.q.GetLimitWindowForUpdate(r.ctx, gen.GetLimitWindowForUpdateParams{MerchantID: r.merchant, CustomerID: k.customer, LimitKey: p.key, WindowStartsAt: p.start})
			switch {
			case err == nil:
				capacity = row.Capacity
			case !errors.Is(err, pgx.ErrNoRows):
				return nil, err
			}
			s.claim = limits.Claim{Key: p.key, Unit: p.unit, Reset: catalog.LimitReset{Kind: catalog.ResetFixed, WindowSeconds: int(width / time.Second)},
				Window: limits.Window{StartsAt: start, EndsAt: &end, Open: true}, Capacity: capacity, Overrun: catalog.OverrunRequireEstimate}
		default:
			c, ok := windows[p.key]
			if !ok {
				return nil, nil
			}
			s.claim, s.overrun = c, c.Overrun
		}
		out = append(out, s)
	}
	return out, nil
}

// rollAhead issues an active hold's next-window hold when one of its fixed
// windows closes before the lease syncs again, so the reset does not send
// every active customer to a want at once.
func (r *syncRun) rollAhead(k holdKey) error {
	h := r.current(k, r.now)
	if h == nil || h.released {
		return nil
	}
	var closes *time.Time
	for _, p := range h.parts {
		if p.end == nil || p.key == catalog.BalanceLimit {
			continue
		}
		if d, fixed := r.fixedKind(p.key); fixed && d && p.end.After(r.now) && !p.end.After(r.now.Add(rolloverAhead)) && (closes == nil || p.end.Before(*closes)) {
			end := *p.end
			closes = &end
		}
	}
	if !h.active || closes == nil {
		return nil
	}
	for _, other := range r.holds[k] {
		if other != h && !other.released && other.contains(*closes) {
			return nil
		}
	}
	// The limits whose windows close take their next window; the rest keep
	// the current one.
	slots, err := r.slotsAt(k, h, r.now)
	if err != nil || slots == nil {
		return err
	}
	next, err := r.slotsAt(k, h, *closes)
	if err != nil || next == nil {
		return err
	}
	for j := range slots {
		if p := h.part(slots[j].key); p != nil && p.end != nil && !p.end.After(*closes) {
			if fixed, known := r.fixedKind(p.key); fixed && known {
				slots[j] = next[j]
			}
		}
	}
	_, room, err := r.resolve(identity.CustomerID(k.customer), admission.AdmitRequest{CustomerID: identity.CustomerID(k.customer),
		InvokerType: string(billing.InvokerTypeCustomer), Currency: h.row.Currency, Source: "lease"})
	if err != nil {
		return err
	}
	// The next window's hold starts from one request's worth of each limit.
	for j := range slots {
		if p := h.part(slots[j].key); p != nil && p.request > 0 {
			slots[j].request = p.request
		}
	}
	_, err = r.size(nil, k, h.row.Currency, slots, false, room, h.row.RateCards)
	return err
}

// fixedKind reports whether a limit's windows tile the clock (a fixed or
// billing-period limit, or a spend window), so the next one is known before
// the current one closes.
func (r *syncRun) fixedKind(key string) (bool, bool) {
	if strings.HasPrefix(key, limits.SpendKeyPrefix) {
		return true, true
	}
	for _, d := range r.defs {
		if d.Key == key {
			return d.Reset.Kind == catalog.ResetFixed || d.Reset.Kind == catalog.ResetBillingPeriod, true
		}
	}
	return false, false
}

// answer lists the holds the sync changed and the customers' limits.
func (r *syncRun) answer() error {
	keys := make([]holdKey, 0, len(r.holds))
	for k := range r.holds {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b holdKey) int {
		return cmp.Or(compareUUID(a.customer, b.customer), strings.Compare(a.limitSet, b.limitSet))
	})
	for _, k := range keys {
		// A hold issued again stands for the one it replaced.
		replaced := false
		for _, h := range r.holds[k] {
			replaced = replaced || (h.changed && !h.released)
		}
		for _, h := range r.holds[k] {
			if !h.changed || (h.released && replaced) {
				continue
			}
			out, err := r.wire(k, h)
			if err != nil {
				return err
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

// wire is a hold as its gate reads it; a released hold holds nothing. With
// the merchant's usage_gate_safe_spend, its safe amount is its money size.
func (r *syncRun) wire(k holdKey, h *hold) (billing.Hold, error) {
	cards, bounds, err := money.HoldCards(h.row.RateCards)
	if err != nil {
		return billing.Hold{}, err
	}
	safe, err := r.safeSpend()
	if err != nil {
		return billing.Hold{}, err
	}
	out := billing.Hold{CustomerID: billing.CustomerID(k.customer), LimitSet: k.limitSet, Currency: h.row.Currency,
		Limits: make([]billing.HoldLimit, 0, len(h.parts)), RateCards: cards, PriceBounds: bounds}
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
	return out, nil
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

// p0used reports whether usage took anything from the hold's parts since the
// previous sync.
func p0used(parts map[string]*part) bool {
	for _, p := range parts {
		if p.used > 0 {
			return true
		}
	}
	return false
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
