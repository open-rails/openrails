package reconcile

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/db/models"
)

// localIndex precomputes the lookups the diff checks share.
type localIndex struct {
	byPSID              map[string]*LocalSubscription
	byID                map[uuid.UUID]*LocalSubscription
	byEmail             map[string][]*LocalSubscription
	bySubject           map[uuid.UUID][]*LocalSubscription
	pmByRailCustomerRef map[string]*LocalPaymentMethod
	pmByID              map[uuid.UUID]*LocalPaymentMethod
	prices              []LocalPrice
}

func buildLocalIndex(local *LocalState) *localIndex {
	idx := &localIndex{
		byPSID:              map[string]*LocalSubscription{},
		byID:                map[uuid.UUID]*LocalSubscription{},
		byEmail:             map[string][]*LocalSubscription{},
		bySubject:           map[uuid.UUID][]*LocalSubscription{},
		pmByRailCustomerRef: map[string]*LocalPaymentMethod{},
		pmByID:              map[uuid.UUID]*LocalPaymentMethod{},
	}
	for i := range local.Subscriptions {
		s := &local.Subscriptions[i]
		idx.byID[s.ID] = s
		idx.bySubject[s.CustomerID] = append(idx.bySubject[s.CustomerID], s)
		if s.RailSubscriptionID != "" {
			// Prefer the live row when several local rows share one rail
			// subscription id (supersede history).
			if prev, ok := idx.byPSID[s.RailSubscriptionID]; !ok || (!prev.IsLive() && s.IsLive()) {
				idx.byPSID[s.RailSubscriptionID] = s
			}
		}
		if email := strings.ToLower(strings.TrimSpace(s.UserEmail)); email != "" {
			idx.byEmail[email] = append(idx.byEmail[email], s)
		}
	}
	for i := range local.PaymentMethods {
		pm := &local.PaymentMethods[i]
		idx.pmByRailCustomerRef[pm.RailCustomerRef] = pm
		idx.pmByID[pm.ID] = pm
	}
	idx.prices = local.Prices
	return idx
}

// planLink is one (price, local rail name) pair whose provider link
// carries a given remote plan id.
type planLink struct {
	price    *LocalPrice
	railName string
}

// planLinkIDKeys are the provider-link config keys that hold a rail-side
// plan/price identifier (plan_pda: solana remote plan ids are the plan PDA).
var planLinkIDKeys = []string{"plan_id", "price_id", "recurring_billing_option_id", "plan_pda"}

// buildPlanIndex maps remote plan ids onto local prices via the catalog
// provider_links blobs, restricted to the provider's local rail names. Blobs
// key on the merchant's ACCOUNT key with the rail recorded in each entry —
// two accounts carrying the same plan id on one price dedup to a single link.
func buildPlanIndex(provider Provider, prices []LocalPrice) map[string][]planLink {
	idx := map[string][]planLink{}
	names := localRailNames(provider)
	for i := range prices {
		p := &prices[i]
		for _, name := range names {
			seen := map[string]struct{}{}
			for _, cfg := range models.PSPLinksOnRail(p.PSPLinks, models.Rail(name)) {
				for _, key := range planLinkIDKeys {
					id := strings.TrimSpace(cfg[key])
					if id == "" {
						continue
					}
					if _, dup := seen[id]; dup {
						continue
					}
					seen[id] = struct{}{}
					idx[id] = append(idx[id], planLink{price: p, railName: name})
				}
			}
		}
	}
	return idx
}

// remoteIndex precomputes snapshot lookups.
type remoteIndex struct {
	subByPSID               map[string]*RemoteSubscription
	subsByCust              map[string][]*RemoteSubscription
	remoteByRailCustomerRef map[string]*RemotePaymentMethod
	liveSubByPSID           map[string]bool
}

func remoteLive(status SubscriptionStatus) bool {
	switch status {
	case SubscriptionStatusActive, SubscriptionStatusPastDue:
		return true
	}
	return false
}

func buildRemoteIndex(snap *RemoteSnapshot) *remoteIndex {
	idx := &remoteIndex{
		subByPSID:               map[string]*RemoteSubscription{},
		subsByCust:              map[string][]*RemoteSubscription{},
		remoteByRailCustomerRef: map[string]*RemotePaymentMethod{},
		liveSubByPSID:           map[string]bool{},
	}
	for i := range snap.Subscriptions {
		r := &snap.Subscriptions[i]
		if r.RailSubscriptionID == "" {
			continue
		}
		// On duplicate snapshot entries for one rail subscription id
		// (CCBill: ACTIVEMEMBERS roster + a windowed termination event), the
		// point-in-time roster entry (live) wins.
		if prev, ok := idx.subByPSID[r.RailSubscriptionID]; !ok || (!remoteLive(prev.Status) && remoteLive(r.Status)) {
			idx.subByPSID[r.RailSubscriptionID] = r
		}
		if r.CustomerID != "" {
			idx.subsByCust[r.CustomerID] = append(idx.subsByCust[r.CustomerID], r)
		}
	}
	for psid, r := range idx.subByPSID {
		idx.liveSubByPSID[psid] = remoteLive(r.Status)
	}
	for i := range snap.PaymentMethods {
		v := &snap.PaymentMethods[i]
		if v.RailCustomerRef != "" {
			idx.remoteByRailCustomerRef[v.RailCustomerRef] = v
		}
	}
	return idx
}

// txnBreadcrumbs are the correlation hints fetchers preserve in
// RemoteTransaction.Raw: NMI keeps order_id / customer_vault_id / email,
// Stripe objects carry customer / invoice / charge.
type txnBreadcrumbs struct {
	OrderID         string `json:"order_id"`
	CustomerVaultID string `json:"customer_vault_id"`
	Email           string `json:"email"`
	Customer        string `json:"customer"`
	Invoice         string `json:"invoice"`
	Charge          string `json:"charge"`
}

func decodeBreadcrumbs(raw json.RawMessage) txnBreadcrumbs {
	var bc txnBreadcrumbs
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &bc)
	}
	return bc
}

// parseRebillOrderID extracts the local subscription uuid from the dunning
// worker's order_id breadcrumb `rebill-<subscriptionID>-<unixPeriodEnd>`, or
// from a bare local-subscription-uuid order id (checkout signup shape).
func parseRebillOrderID(orderID string) (uuid.UUID, bool) {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return uuid.Nil, false
	}
	if rest, ok := strings.CutPrefix(orderID, "rebill-"); ok {
		if i := strings.LastIndex(rest, "-"); i > 0 {
			if id, err := uuid.Parse(rest[:i]); err == nil {
				return id, true
			}
		}
		return uuid.Nil, false
	}
	if id, err := uuid.Parse(orderID); err == nil {
		return id, true
	}
	return uuid.Nil, false
}

// correlator resolves a remote transaction to a local subscription, never
// guessing: zero or multiple candidates surface as ambiguous.
type correlator struct {
	local  *localIndex
	remote *remoteIndex
}

// subForTxn resolves the local subscription a remote transaction belongs to.
// how documents the successful path; ambiguous=true means candidates existed
// but identity could not be pinned to exactly one subscription.
func (c *correlator) subForTxn(t *RemoteTransaction) (sub *LocalSubscription, how string, ambiguous bool) {
	// 1. Direct rail subscription id (CCBill, Solana).
	if t.SubscriptionID != "" {
		if s, ok := c.local.byPSID[t.SubscriptionID]; ok {
			return s, "rail_subscription_id", false
		}
	}

	bc := decodeBreadcrumbs(t.Raw)

	// 2. NMI order_id breadcrumb (`rebill-<subID>-<unix>` or the local
	// subscription uuid set at signup).
	if id, ok := parseRebillOrderID(bc.OrderID); ok {
		if s, ok := c.local.byID[id]; ok {
			return s, "order_id", false
		}
	}

	// 3. Vault/customer id -> local payment method -> that subject's
	// subscriptions; unique match only.
	for _, railCustomerRef := range []string{bc.CustomerVaultID, bc.Customer} {
		if railCustomerRef == "" {
			continue
		}
		if pm, ok := c.local.pmByRailCustomerRef[railCustomerRef]; ok {
			subs := c.local.bySubject[pm.CustomerID]
			if s, ok := uniqueSub(subs); ok {
				return s, "vault_id", false
			}
			if len(subs) > 1 {
				ambiguous = true
			}
		}
		// 4. Same customer id on a remote subscription whose rail
		// subscription id matches locally (Stripe charges: customer ->
		// subscription roster -> local).
		var candidates []*LocalSubscription
		for _, r := range c.remote.subsByCust[railCustomerRef] {
			if s, ok := c.local.byPSID[r.RailSubscriptionID]; ok {
				candidates = append(candidates, s)
			}
		}
		if s, ok := uniqueSub(candidates); ok {
			return s, "customer_id", false
		}
		if len(candidates) > 1 {
			ambiguous = true
		}
	}

	// 5. Email fallback; unique match only.
	if email := strings.ToLower(strings.TrimSpace(bc.Email)); email != "" {
		subs := c.local.byEmail[email]
		if s, ok := uniqueSub(subs); ok {
			return s, "email", false
		}
		if len(subs) > 1 {
			ambiguous = true
		}
	}

	return nil, "", ambiguous
}

// uniqueSub returns the single subscription of a candidate set, preferring a
// single LIVE one when the set mixes a live row with its dead predecessors.
func uniqueSub(subs []*LocalSubscription) (*LocalSubscription, bool) {
	if len(subs) == 1 {
		return subs[0], true
	}
	var live []*LocalSubscription
	for _, s := range subs {
		if s.IsLive() {
			live = append(live, s)
		}
	}
	if len(live) == 1 {
		return live[0], true
	}
	return nil, false
}

// --- evidence helpers ---

func localSubEvidence(s *LocalSubscription) map[string]any {
	ev := map[string]any{
		"subscription_id":      s.ID.String(),
		"customer_id":          s.CustomerID.String(),
		"status":               s.Status,
		"rail":                 s.Rail,
		"rail_subscription_id": s.RailSubscriptionID,
	}
	if s.UserEmail != "" {
		ev["user_email"] = s.UserEmail
	}
	if s.CurrentPeriodEndsAt != nil {
		ev["current_period_ends_at"] = s.CurrentPeriodEndsAt.Format(time.RFC3339)
	}
	if s.CancelType != "" {
		ev["cancel_type"] = s.CancelType
	}
	if s.DeletionScheduledAt != nil {
		ev["deletion_scheduled_at"] = s.DeletionScheduledAt.Format(time.RFC3339)
	}
	return ev
}

func remoteSubEvidence(r *RemoteSubscription) map[string]any {
	ev := map[string]any{
		"rail_subscription_id": r.RailSubscriptionID,
		"status":               string(r.Status),
	}
	if r.RawStatus != "" {
		ev["raw_status"] = r.RawStatus
	}
	if r.Email != "" {
		ev["email"] = r.Email
	}
	if r.CustomerID != "" {
		ev["customer_id"] = r.CustomerID
	}
	if r.PlanID != "" {
		ev["plan_id"] = r.PlanID
	}
	if r.NextBillingAt != nil {
		ev["next_billing_at"] = r.NextBillingAt.Format(time.RFC3339)
	}
	if r.AmountCents != 0 {
		ev["amount_cents"] = r.AmountCents
	}
	return ev
}

func remoteTxnEvidence(t *RemoteTransaction) map[string]any {
	ev := map[string]any{
		"transaction_id": t.TransactionID,
		"type":           string(t.Type),
		"success":        t.Success,
		"amount_cents":   t.AmountCents,
		// occurred_at drives the windowed auto-resolve of PS-4/5/6.
		"occurred_at": t.OccurredAt.UTC().Format(time.RFC3339),
	}
	if t.SubscriptionID != "" {
		ev["subscription_id"] = t.SubscriptionID
	}
	if t.Currency != "" {
		ev["currency"] = t.Currency
	}
	if t.DeclineReason != "" {
		ev["decline_reason"] = t.DeclineReason
	}
	return ev
}

// deletionIntent builds the class-3 intent annotation (design decision 5)
// when a local subscription carries a recorded-but-unexecuted rail
// delete marker.
func deletionIntent(s *LocalSubscription) map[string]any {
	if s.DeletionScheduledAt == nil {
		return nil
	}
	return map[string]any{
		"deletion_scheduled_at": s.DeletionScheduledAt.Format(time.RFC3339),
		"explanation":           "local cancellation recorded a deferred rail delete that has not executed yet; the provider intent executor (#358) replays the recorded delete",
	}
}

const deletionIntentAction = "no admin action needed: the provider intent executor replays the recorded delete (deletion_scheduled_at marker present)"

// diffOptions carries per-run diff behavior switches.
type diffOptions struct {
	// Materialize (set automatically in enforce mode; advisory never writes):
	// PS-1 findings whose identity and plan both resolve unambiguously carry
	// a materialize apply action instead of going requires_review.
	Materialize bool
	// EvidenceFloor (#835) is the merchant's first-pull instant, handed to
	// every decider invocation this diff makes: a cancel resting on evidence
	// older than it is withheld and the row parks as `unknown`.
	EvidenceFloor time.Time
}

// diffProvider runs every capability-gated PULL-plane check for one provider
// snapshot vs local state and returns the findings, each carrying its enforce
// instruction where one is safe. Internal-plane checks (DERIVE/LIFE/CON) live
// in the Convergence Engine, never here (#665 single-writer rule).
func diffProvider(provider Provider, snap *RemoteSnapshot, local *LocalState, localPayments []LocalPayment, now time.Time, opts diffOptions) []Finding {
	idx := buildLocalIndex(local)
	ridx := buildRemoteIndex(snap)
	corr := &correlator{local: idx, remote: ridx}
	traits := traitsFor(provider)
	caps := snap.Capabilities

	paymentsByTxnID := map[string]*LocalPayment{}
	for i := range localPayments {
		p := &localPayments[i]
		paymentsByTxnID[p.TransactionID] = p
	}

	var findings []Finding

	if caps.Subscriptions {
		findings = append(findings, diffSubscriptions(provider, snap, idx, ridx, traits, now, opts)...)
		findings = append(findings, diffDuplicates(provider, idx, ridx)...)
	}
	if caps.Transactions {
		findings = append(findings, diffTransactions(provider, snap, idx, corr, paymentsByTxnID, now)...)
	}
	if caps.Vault {
		findings = append(findings, diffPaymentMethods(provider, local, ridx, traits)...)
	}

	// #835: every cancel the staleness floor withheld surfaces as its own
	// requires_review finding. Done HERE, over the finished finding set, so it
	// covers every decider invocation this diff makes — present and future —
	// rather than being repeated at each decideApply call site.
	findings = append(findings, evidenceFlooredFindings(provider, findings)...)

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Type != findings[j].Type {
			return findings[i].Type < findings[j].Type
		}
		return findings[i].SubjectKey < findings[j].SubjectKey
	})
	return findings
}

// evidenceFlooredFindings raises the operator record for each decision the #835
// staleness floor downgraded from a terminal cancel to a park.
func evidenceFlooredFindings(provider Provider, findings []Finding) []Finding {
	var out []Finding
	for i := range findings {
		a := findings[i].Apply
		if a == nil || a.Decide == nil || !a.Decide.Decision.EvidenceFloored {
			continue
		}
		out = append(out, Finding{
			Provider:      provider,
			Type:          FindingEvidenceStale,
			SubjectKey:    evidenceStaleSubjectKey(provider, a.Decide.SubscriptionID.String()),
			Severity:      SeverityHigh,
			Status:        FindingStatusRequiresReview,
			RequiresAdmin: true,
			LocalEvidence: map[string]any{
				"subscription_id": a.Decide.SubscriptionID.String(),
				"withheld_cause":  a.Decide.Decision.Reason,
			},
			RecommendedAction: evidenceStaleAction(a.Decide.Decision.Reason),
		})
	}
	return out
}

// diffSubscriptions covers PS-1, PS-2 and PS-3.
func diffSubscriptions(provider Provider, snap *RemoteSnapshot, idx *localIndex, ridx *remoteIndex, traits providerTraits, now time.Time, opts diffOptions) []Finding {
	var findings []Finding
	planIdx := buildPlanIndex(provider, idx.prices)

	// Remote -> local: PS-1 and presence-based PS-2/PS-3.
	for psid, r := range ridx.subByPSID {
		localSub, matched := idx.byPSID[psid]
		if !matched {
			if !remoteLive(r.Status) {
				// A terminated remote subscription we never knew is historical
				// noise, not actionable drift.
				continue
			}
			findings = append(findings, makePS1(provider, r, idx, planIdx, snap, opts))
			continue
		}

		// Matched pair: status comparison.
		findings = append(findings, compareStatuses(provider, snap, localSub, r, now, opts)...)
	}

	// Local -> remote: absence-based PS-2 (NMI: the recurring report only
	// lists live subscriptions, so a locally-live linked subscription that is
	// absent is dead at the rail). Guarded upstream by the circuit breaker and
	// the cancellation cap.
	//
	// #842: absence is only proof when the roster PROVES it covered everything.
	// This used to force SubscriptionsExhaustive=true regardless of what the
	// fetcher declared, so an empty or silently-truncated pull cancelled the
	// merchant's whole book. The snapshot's own coverage decides now.
	if traits.absenceMeansCancelled && snap.Coverage.SubscriptionsExhaustive {
		for _, s := range idx.byPSID {
			if !s.IsLive() {
				continue
			}
			if _, present := ridx.subByPSID[s.RailSubscriptionID]; present {
				continue
			}
			f := Finding{
				Provider:      provider,
				Type:          FindingLocalActiveRemoteDead,
				SubjectKey:    s.ID.String(),
				Severity:      SeverityHigh,
				Status:        FindingStatusReconcileRequired,
				LocalEvidence: localSubEvidence(s),
				RemoteEvidence: map[string]any{
					"absent_from_recurring_report": true,
					"rail_subscription_id":         s.RailSubscriptionID,
				},
				RecommendedAction: "rail no longer bills this subscription; enforce cancels it locally and revokes its subscription-sourced entitlements (decider: provider-confirmed dead)",
				// absenceMeansCancelled: the provider's live-only roster IS
				// exhaustive for this subject — coverage-absence proof (#665).
				Apply: decideApply(s, snap, now, opts),
			}
			if intent := deletionIntent(s); intent != nil {
				f.IntentEvidence = intent
			}
			findings = append(findings, f)
		}
	}

	return findings
}

func makePS1(provider Provider, r *RemoteSubscription, idx *localIndex, planIdx map[string][]planLink, snap *RemoteSnapshot, opts diffOptions) Finding {
	remoteEv := remoteSubEvidence(r)
	if len(r.Raw) > 0 {
		var raw any
		if err := json.Unmarshal(r.Raw, &raw); err == nil {
			remoteEv["raw"] = raw
		}
	}
	f := Finding{
		Provider:          provider,
		Type:              FindingRemoteSubMissingLocal,
		SubjectKey:        r.RailSubscriptionID,
		Severity:          SeverityCritical,
		Status:            FindingStatusAdminRequired,
		RequiresAdmin:     true,
		RemoteEvidence:    remoteEv,
		RecommendedAction: "the rail is billing a subscription OpenRails does not know; investigate and either import it locally or cancel it at the rail (remote action, admin-only). Advisory runs never auto-create",
	}
	// Email fallback: surface candidates so the admin sees the likely owner,
	// but never auto-link (zero or multiple matches = human decision).
	if email := strings.ToLower(strings.TrimSpace(r.Email)); email != "" {
		if candidates := idx.byEmail[email]; len(candidates) > 0 {
			matches := make([]map[string]any, 0, len(candidates))
			for _, c := range candidates {
				matches = append(matches, map[string]any{
					"subscription_id":      c.ID.String(),
					"customer_id":          c.CustomerID.String(),
					"status":               c.Status,
					"rail_subscription_id": c.RailSubscriptionID,
				})
			}
			f.LocalEvidence = map[string]any{"email_candidates": matches}
			if len(candidates) == 1 {
				f.RecommendedAction = "the rail bills a subscription unknown locally, but exactly one local subscription shares its email (see local_evidence); verify and link/import manually, or re-run fix (enforce materializes resolvable PS-1s)"
			}
		}
	}
	if !opts.Materialize {
		return f
	}

	// Materialization (bootstrap mode v1.1): auto-create ONLY when both
	// identity and plan resolve unambiguously; anything else stays
	// requires_review exactly as without the flag, with the blocker documented.
	subjectID, identityVia, identityNote := resolvePS1Identity(r, idx)
	link, planNote := resolvePS1Plan(r, planIdx)
	var blockers []string
	if identityNote != "" {
		blockers = append(blockers, identityNote)
	}
	if planNote != "" {
		blockers = append(blockers, planNote)
	}
	if r.Status == SubscriptionStatusPastDue && r.NextBillingAt == nil {
		// Local past_due requires a period end (chk_past_due_has_period_end).
		blockers = append(blockers, "remote is past_due without a next billing date; local past_due requires a period end")
	}
	if discoveredNotLocalRaw(r.Raw) {
		// #714: chain-scan discoveries (permissionless `subscribe`) never
		// auto-create local billing state — operator decision only.
		blockers = append(blockers, "subscription was discovered on-chain with no local checkout trail (#714); creating billing state from chain data alone is an operator decision")
	}
	if len(blockers) > 0 {
		remoteEv["materialize_blocked"] = strings.Join(blockers, "; ")
		return f
	}

	action := &MaterializeSubscriptionAction{
		Provider:           provider,
		Rail:               link.railName,
		RailSubscriptionID: r.RailSubscriptionID,
		CustomerID:         subjectID,
		PriceID:            link.price.ID,
		ProductID:          link.price.ProductID,
		Status:             string(r.Status),
		PeriodEndsAt:       r.NextBillingAt,
		UserEmail:          strings.TrimSpace(r.Email),
		IdentityVia:        identityVia,
	}
	if r.LastBilledAt != nil {
		action.StartedAt = r.LastBilledAt
		// chk_valid_period: only carry the period start when it precedes the end.
		if r.NextBillingAt == nil || r.LastBilledAt.Before(*r.NextBillingAt) {
			action.PeriodStartsAt = r.LastBilledAt
		}
	}
	if t := latestChargeForRemoteSub(snap, r); t != nil {
		action.Backfill = &BackfillPaymentAction{
			Rail:          link.railName,
			TransactionID: t.TransactionID,
			AmountCents:   t.AmountCents,
			Currency:      strings.ToLower(t.Currency),
			PurchasedAt:   t.OccurredAt,
			PriceID:       link.price.ID,
			CustomerID:    subjectID,
			Metadata: map[string]any{
				"reconcile_backfill":    true,
				"reconcile_materialize": true,
				"provider":              string(provider),
			},
		}
	}

	f.Status = FindingStatusReconcileRequired
	f.RequiresAdmin = false
	f.Apply = &ApplyAction{Materialize: action}
	f.RecommendedAction = fmt.Sprintf(
		"materialize: identity resolved via %s and plan %q maps to exactly one local price; enforce creates the local subscription (status %s) with the remote periods%s",
		identityVia, r.PlanID, r.Status,
		map[bool]string{true: " and backfills the snapshot's latest charge", false: ""}[action.Backfill != nil])
	if f.LocalEvidence == nil {
		f.LocalEvidence = map[string]any{}
	}
	f.LocalEvidence["materialize_identity_via"] = identityVia
	f.LocalEvidence["materialize_customer_id"] = subjectID.String()
	f.LocalEvidence["materialize_price_id"] = link.price.ID.String()
	f.LocalEvidence["materialize_rail"] = link.railName
	return f
}

// resolvePS1Identity resolves the remote subscription's owner through the
// engine's existing matcher surfaces: the vault/customer id against local
// payment methods, then the email against local subscriptions. Exactly one
// DISTINCT subject across both is a resolution; zero or multiple is not
// (never guess).
func resolvePS1Identity(r *RemoteSubscription, idx *localIndex) (uuid.UUID, string, string) {
	subjects := map[uuid.UUID]string{} // subject -> how it matched (vault wins)
	if r.CustomerID != "" {
		if pm, ok := idx.pmByRailCustomerRef[r.CustomerID]; ok {
			subjects[pm.CustomerID] = "vault_id"
		}
	}
	if email := strings.ToLower(strings.TrimSpace(r.Email)); email != "" {
		for _, s := range idx.byEmail[email] {
			if _, ok := subjects[s.CustomerID]; !ok {
				subjects[s.CustomerID] = "email"
			}
		}
	}
	switch len(subjects) {
	case 0:
		return uuid.Nil, "", "identity unresolved: no local vault/email match"
	case 1:
		for id, via := range subjects {
			return id, via, ""
		}
	}
	return uuid.Nil, "", fmt.Sprintf("identity ambiguous: %d candidate subjects", len(subjects))
}

// resolvePS1Plan maps the remote plan id onto exactly one local price via the
// catalog provider_links.
func resolvePS1Plan(r *RemoteSubscription, planIdx map[string][]planLink) (planLink, string) {
	planID := strings.TrimSpace(r.PlanID)
	if planID == "" {
		return planLink{}, "plan unresolved: remote record carries no plan id"
	}
	links := planIdx[planID]
	switch len(links) {
	case 0:
		return planLink{}, fmt.Sprintf("plan unresolved: no local price carries a provider link for plan %q", planID)
	case 1:
		return links[0], ""
	}
	return planLink{}, fmt.Sprintf("plan ambiguous: %d local prices carry provider links for plan %q", len(links), planID)
}

// latestChargeForRemoteSub finds the snapshot's most recent successful sale
// attributable to the remote subscription (direct subscription linkage, or a
// matching vault/customer breadcrumb).
func latestChargeForRemoteSub(snap *RemoteSnapshot, r *RemoteSubscription) *RemoteTransaction {
	var best *RemoteTransaction
	for i := range snap.Transactions {
		t := &snap.Transactions[i]
		if t.Type != TransactionTypeSale || !t.Success || t.TransactionID == "" || t.AmountCents <= 0 {
			continue
		}
		matched := t.SubscriptionID != "" && t.SubscriptionID == r.RailSubscriptionID
		if !matched && r.CustomerID != "" {
			bc := decodeBreadcrumbs(t.Raw)
			matched = bc.CustomerVaultID == r.CustomerID || bc.Customer == r.CustomerID
		}
		if !matched {
			continue
		}
		if best == nil || t.OccurredAt.After(best.OccurredAt) {
			best = t
		}
	}
	return best
}

// perSubscriptionSnapshot narrows a bulk snapshot to one subscription's
// evidence slice (roster entry + linked charge events + coverage), the shape
// the decider consumes. forceExhaustive stamps SubscriptionsExhaustive for
// providers whose live-only roster is exhaustive by construction
// (traits.absenceMeansCancelled) even when a fixture omitted the coverage flag.
func perSubscriptionSnapshot(snap *RemoteSnapshot, psid string) *RemoteSnapshot {
	out := &RemoteSnapshot{
		Provider:     snap.Provider,
		FetchedAt:    snap.FetchedAt,
		Capabilities: snap.Capabilities,
		Coverage:     snap.Coverage,
	}
	for i := range snap.Subscriptions {
		if snap.Subscriptions[i].RailSubscriptionID == psid {
			out.Subscriptions = append(out.Subscriptions, snap.Subscriptions[i])
		}
	}
	for i := range snap.Transactions {
		if snap.Transactions[i].SubscriptionID == psid {
			out.Transactions = append(out.Transactions, snap.Transactions[i])
		}
	}
	return out
}

// decideApply computes the decider transition for one local subscription from
// its per-subscription snapshot slice and wraps it as the finding's apply
// action (#665: the pull engine invokes the decider; no applier writes domain
// state). Nil when the decider has no evidence-justified move.
func decideApply(s *LocalSubscription, snap *RemoteSnapshot, now time.Time, opts diffOptions) *ApplyAction {
	state := SubscriptionState{
		Status:             s.Status,
		Rail:               s.Rail,
		HasPaymentMethod:   s.PaymentMethodID != nil,
		RailSubscriptionID: s.RailSubscriptionID,
		PeriodEnd:          s.CurrentPeriodEndsAt,
		NextRetryScheduled: s.NextRetryAt != nil,
	}
	d := Decide(state, EvidenceBundle{Snapshot: perSubscriptionSnapshot(snap, s.RailSubscriptionID), EvidenceFloor: opts.EvidenceFloor}, now, 0)
	if d.Kind == TransitionNone {
		return nil
	}
	// The pull path never imports charges through the decider — PS-4 owns
	// charge backfill with full correlation.
	d.Backfill = nil
	return &ApplyAction{Decide: &DecideAction{SubscriptionID: s.ID, Decision: d}}
}

// compareStatuses diffs one matched (local, remote) subscription pair into
// PS-2/PS-3 findings; the enforce instruction is a decider invocation.
func compareStatuses(provider Provider, snap *RemoteSnapshot, s *LocalSubscription, r *RemoteSubscription, now time.Time, opts diffOptions) []Finding {
	if r.Status == SubscriptionStatusUnknown {
		return nil // the provider declared nothing comparable
	}

	localLive := s.IsLive()
	remoteDead := r.Status == SubscriptionStatusCancelled || r.Status == SubscriptionStatusExpired

	switch {
	case localLive && remoteDead:
		// PS-2: rail terminated it, local still bills/grants. Decider:
		// provider-confirmed dead → cancel-with-certainty.
		f := Finding{
			Provider:          provider,
			Type:              FindingLocalActiveRemoteDead,
			SubjectKey:        s.ID.String(),
			Severity:          SeverityHigh,
			Status:            FindingStatusReconcileRequired,
			LocalEvidence:     localSubEvidence(s),
			RemoteEvidence:    remoteSubEvidence(r),
			RecommendedAction: "rail reports this subscription " + string(r.Status) + "; enforce cancels it locally and revokes its subscription-sourced entitlements",
			Apply:             decideApply(s, snap, now, opts),
		}
		if intent := deletionIntent(s); intent != nil {
			f.IntentEvidence = intent
		}
		return []Finding{f}

	case !localLive && remoteLive(r.Status):
		// PS-3 (inverse direction): local says dead, rail still bills.
		// Never auto-resurrect a locally-terminated subscription; this is
		// either a recorded-but-unexecuted delete (intent annotation) or an
		// orphan the admin must cancel AT the rail.
		f := Finding{
			Provider:          provider,
			Type:              FindingStatusMismatch,
			SubjectKey:        s.ID.String(),
			Severity:          SeverityMedium,
			Status:            FindingStatusAdminRequired,
			RequiresAdmin:     true,
			LocalEvidence:     localSubEvidence(s),
			RemoteEvidence:    remoteSubEvidence(r),
			RecommendedAction: "subscription is terminated locally but the rail still bills it; cancel it AT the rail (remote action, admin-only)",
		}
		if intent := deletionIntent(s); intent != nil {
			f.IntentEvidence = intent
			f.Status = FindingStatusReconcileRequired
			f.RequiresAdmin = false
			f.RecommendedAction = deletionIntentAction
		}
		return []Finding{f}

	case localLive && remoteLive(r.Status) && string(r.Status) != s.Status:
		// PS-3: both live, different flavor (active vs past_due). The decider
		// chooses the transition: roster alive w/ future boundary → adopt the
		// period END only; verified charge → renew; declared failure →
		// dunning within the window, cancel-with-certainty beyond. A pending
		// local row has no decider transition (the signup/confirm path or
		// pending_stale owns it) — the divergence is still surfaced.
		f := Finding{
			Provider:          provider,
			Type:              FindingStatusMismatch,
			SubjectKey:        s.ID.String(),
			Severity:          SeverityMedium,
			Status:            FindingStatusReconcileRequired,
			LocalEvidence:     localSubEvidence(s),
			RemoteEvidence:    remoteSubEvidence(r),
			RecommendedAction: fmt.Sprintf("rail status %q vs local %q; enforce applies the evidence-gated transition", r.Status, s.Status),
			Apply:             decideApply(s, snap, now, opts),
		}
		if intent := deletionIntent(s); intent != nil {
			f.IntentEvidence = intent
		}
		return []Finding{f}

	case localLive && remoteLive(r.Status):
		// Same status: check period drift.
		if r.NextBillingAt != nil && s.CurrentPeriodEndsAt != nil {
			drift := r.NextBillingAt.Sub(*s.CurrentPeriodEndsAt)
			if drift < 0 {
				drift = -drift
			}
			if drift > 48*time.Hour {
				return []Finding{{
					Provider:          provider,
					Type:              FindingStatusMismatch,
					SubjectKey:        s.ID.String(),
					Severity:          SeverityMedium,
					Status:            FindingStatusReconcileRequired,
					LocalEvidence:     localSubEvidence(s),
					RemoteEvidence:    remoteSubEvidence(r),
					RecommendedAction: fmt.Sprintf("period end drifts %.0fh from the rail's next billing date; enforce re-anchors the period END from provider truth (a verified charge renews; adoption never grants access)", drift.Hours()),
					Apply:             decideApply(s, snap, now, opts),
				}}
			}
		}
	}
	return nil
}

// diffDuplicates covers PS-8: one subject carrying multiple live
// subscriptions at the rail (same tier locally, or several unmatched
// remote subscriptions on the same identity).
func diffDuplicates(provider Provider, idx *localIndex, ridx *remoteIndex) []Finding {
	type group struct {
		key   string
		subs  []*RemoteSubscription
		local []*LocalSubscription
	}
	groups := map[string]*group{}

	addGroup := func(key string, r *RemoteSubscription, l *LocalSubscription) {
		g, ok := groups[key]
		if !ok {
			g = &group{key: key}
			groups[key] = g
		}
		g.subs = append(g.subs, r)
		if l != nil {
			g.local = append(g.local, l)
		}
	}

	for _, r := range ridx.subByPSID {
		if !remoteLive(r.Status) {
			continue
		}
		if l, ok := idx.byPSID[r.RailSubscriptionID]; ok {
			// Matched: duplicates within one (subject, tier group).
			addGroup("subject:"+l.CustomerID.String()+"|tier:"+l.TierGroup, r, l)
			continue
		}
		// Unmatched: duplicates on the same rail-side identity + plan.
		ident := strings.ToLower(strings.TrimSpace(r.Email))
		if ident == "" {
			ident = r.CustomerID
		}
		if ident == "" {
			continue
		}
		addGroup("remote:"+ident+"|plan:"+r.PlanID, r, nil)
	}

	var findings []Finding
	for _, g := range groups {
		if len(g.subs) < 2 {
			continue
		}
		remoteEv := map[string]any{"live_remote_subscriptions": len(g.subs)}
		var subEv []map[string]any
		for _, r := range g.subs {
			subEv = append(subEv, remoteSubEvidence(r))
		}
		remoteEv["subscriptions"] = subEv
		var localEv map[string]any
		if len(g.local) > 0 {
			var l []map[string]any
			for _, s := range g.local {
				l = append(l, localSubEvidence(s))
			}
			localEv = map[string]any{"matched_subscriptions": l}
		}
		findings = append(findings, Finding{
			Provider:          provider,
			Type:              FindingDuplicateSubscriptions,
			SubjectKey:        g.key,
			Severity:          SeverityHigh,
			Status:            FindingStatusAdminRequired,
			RequiresAdmin:     true,
			LocalEvidence:     localEv,
			RemoteEvidence:    remoteEv,
			RecommendedAction: "one subject is billed by multiple live rail subscriptions; pick the keeper, cancel the duplicate AT the rail and refund the double charge (remote action, admin-only)",
		})
	}
	sort.Slice(findings, func(i, j int) bool { return findings[i].SubjectKey < findings[j].SubjectKey })
	return findings
}

// diffTransactions covers PS-4 (missing charge), PS-5 (unrecorded refund) and
// PS-6 (chargeback vs live subscription).
func diffTransactions(provider Provider, snap *RemoteSnapshot, idx *localIndex, corr *correlator, paymentsByTxnID map[string]*LocalPayment, now time.Time) []Finding {
	caps := snap.Capabilities
	var findings []Finding

	for i := range snap.Transactions {
		t := &snap.Transactions[i]
		switch t.Type {
		case TransactionTypeSale:
			if !t.Success || t.TransactionID == "" {
				continue
			}
			if _, ok := paymentsByTxnID[t.TransactionID]; ok {
				continue
			}
			// #714 wallet-scan discoveries route on their verdict envelope
			// (before the amount gate: unpriced discoveries must still park).
			if f, ok := makeSolanaDiscoveryPS4(provider, t); ok {
				findings = append(findings, f)
				continue
			}
			if t.AmountCents <= 0 {
				continue
			}
			findings = append(findings, makePS4(provider, t, corr, now))

		case TransactionTypeRefund:
			if !caps.Refunds || !t.Success || t.TransactionID == "" {
				continue
			}
			if f, ok := makePS5(provider, t, corr, paymentsByTxnID, now); ok {
				findings = append(findings, f)
			}

		case TransactionTypeChargeback:
			if !caps.Chargebacks {
				continue
			}
			if f, ok := makePS6(provider, t, corr, paymentsByTxnID); ok {
				findings = append(findings, f)
			}
		}
	}
	return findings
}

func makePS4(provider Provider, t *RemoteTransaction, corr *correlator, now time.Time) Finding {
	sub, how, ambiguous := corr.subForTxn(t)
	f := Finding{
		Provider:       provider,
		Type:           FindingChargeMissingLocal,
		SubjectKey:     t.TransactionID,
		Severity:       SeverityHigh,
		Status:         FindingStatusReconcileRequired,
		RemoteEvidence: remoteTxnEvidence(t),
	}
	switch {
	case sub == nil:
		f.Status = FindingStatusAdminRequired
		f.RequiresAdmin = true
		if ambiguous {
			f.RecommendedAction = "rail charge has no local payment and its identity matches MULTIPLE local candidates; resolve manually (never guess)"
		} else {
			f.RecommendedAction = "rail charge has no local payment and no local identity could be resolved; investigate manually"
		}
	case sub.PriceID == nil:
		f.Status = FindingStatusAdminRequired
		f.RequiresAdmin = true
		f.LocalEvidence = localSubEvidence(sub)
		f.RecommendedAction = "rail charge correlates to a local subscription without a price; backfill manually"
	default:
		f.LocalEvidence = localSubEvidence(sub)
		f.LocalEvidence["correlated_via"] = how
		f.RecommendedAction = "enforce backfills the missing openrails.payments row (deduped on rail+transaction_id)"
		subID := sub.ID
		action := &BackfillPaymentAction{
			Rail:           sub.Rail,
			TransactionID:  t.TransactionID,
			AmountCents:    t.AmountCents,
			Currency:       strings.ToLower(t.Currency),
			PurchasedAt:    t.OccurredAt,
			PriceID:        *sub.PriceID,
			SubscriptionID: &subID,
			CustomerID:     sub.CustomerID,
			Metadata: map[string]any{
				"reconcile_backfill": true,
				"correlated_via":     how,
				"provider":           string(provider),
			},
		}
		// Grant entitlements when the charge's period is still running.
		if sub.IsLive() && len(sub.EntitlementNames) > 0 && sub.CurrentPeriodEndsAt != nil && sub.CurrentPeriodEndsAt.After(now) {
			start := now
			if sub.CurrentPeriodStartsAt != nil {
				start = *sub.CurrentPeriodStartsAt
			}
			action.Grant = &GrantEntitlementsAction{
				SubscriptionID: sub.ID,
				CustomerID:     sub.CustomerID,
				Entitlements:   sub.EntitlementNames,
				StartAt:        start,
				EndAt:          sub.CurrentPeriodEndsAt,
			}
			f.RecommendedAction += "; the charge's period is current, so missing subscription entitlements are granted too"
		}
		f.Apply = &ApplyAction{BackfillPayment: action}
	}
	return f
}

// decodeSolanaDiscovery extracts the #714 wallet-scan verdict envelope from a
// transaction's Raw; nil when the transaction is not a wallet-scan discovery.
func decodeSolanaDiscovery(raw json.RawMessage) *solanaDiscovery {
	if len(raw) == 0 {
		return nil
	}
	var wrap struct {
		D *solanaDiscovery `json:"solana_discovery"`
	}
	if err := json.Unmarshal(raw, &wrap); err != nil {
		return nil
	}
	return wrap.D
}

// discoveredNotLocalRaw reports the #714 program-scan marker: the remote
// subscription exists on-chain with no local mirror row.
func discoveredNotLocalRaw(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var wrap struct {
		D bool `json:"discovered_not_local"`
	}
	_ = json.Unmarshal(raw, &wrap)
	return wrap.D
}

// makeSolanaDiscoveryPS4 routes a #714 wallet-scan discovery (memo-recognized
// merchant-wallet charge with no local payment). Verify-not-decline:
//   - fetcher verdict "park" (any memo/local-record/transfer disagreement,
//     duplicate local-id) => requires_review, never applied;
//   - clean but unpriced (asset is not a registry USD stablecoin) => parked
//     too — the payments mirror is fiat, so the operator prices it manually;
//   - clean one-off with resolved checkout-session identity => backfill via
//     the same idempotent payment lane the other rails use, money from the
//     on-chain transfer only;
//   - clean pull => ok=false, the generic correlator lane owns it (matches by
//     subscription PDA; unknown subscriptions park there).
func makeSolanaDiscoveryPS4(provider Provider, t *RemoteTransaction) (Finding, bool) {
	d := decodeSolanaDiscovery(t.Raw)
	if d == nil {
		return Finding{}, false
	}
	if d.Verdict == "clean" && d.Kind == solanaDiscoveryKindPull {
		return Finding{}, false
	}

	ev := remoteTxnEvidence(t)
	ev["memo_local_id"] = d.MemoLocalID
	ev["discovered_via"] = "wallet_scan"
	f := Finding{
		Provider:       provider,
		Type:           FindingChargeMissingLocal,
		SubjectKey:     t.TransactionID,
		Severity:       SeverityHigh,
		Status:         FindingStatusReconcileRequired,
		RemoteEvidence: ev,
	}

	park := func(reason string) (Finding, bool) {
		ev["park_reason"] = reason
		f.Status = FindingStatusAdminRequired
		f.RequiresAdmin = true
		f.RecommendedAction = "memo-recognized on-chain payment failed verification (" + reason + "); operator triage — recovery never auto-lands on a mismatch (verify-not-decline)"
		return f, true
	}

	if d.Verdict != "clean" {
		return park(d.ParkReason)
	}
	// Clean one-off.
	if t.AmountCents <= 0 {
		return park("recognized purchase is unpriced (transfer asset is not a registry USD stablecoin); price it from the Raw transfer truth and import manually")
	}
	customerID, err := uuid.Parse(d.CustomerID)
	if err != nil {
		return park("discovery envelope carries no resolved customer identity")
	}
	priceID, err := uuid.Parse(d.PriceID)
	if err != nil {
		return park("discovery envelope carries no resolved price identity")
	}

	f.LocalEvidence = map[string]any{
		"checkout_session_id": d.MemoLocalID,
		"customer_id":         customerID.String(),
		"price_id":            priceID.String(),
		"correlated_via":      "purchase_memo",
	}
	f.RecommendedAction = "memo-recognized one-off purchase verified against its checkout session (recipient, mint and amount agree); enforce backfills the missing openrails.payments row with money from the on-chain transfer"
	f.Apply = &ApplyAction{BackfillPayment: &BackfillPaymentAction{
		Rail:          string(ProviderSolana),
		TransactionID: t.TransactionID,
		AmountCents:   t.AmountCents,
		Currency:      strings.ToLower(t.Currency),
		PurchasedAt:   t.OccurredAt,
		PriceID:       priceID,
		CustomerID:    customerID,
		Metadata: map[string]any{
			"reconcile_backfill":  true,
			"correlated_via":      "purchase_memo",
			"discovered_via":      "wallet_scan",
			"checkout_session_id": d.MemoLocalID,
			"provider":            string(provider),
		},
	}}
	return f, true
}

func makePS5(provider Provider, t *RemoteTransaction, corr *correlator, paymentsByTxnID map[string]*LocalPayment, now time.Time) (Finding, bool) {
	bc := decodeBreadcrumbs(t.Raw)

	// Identify the local original payment: an explicit charge link (Stripe
	// re_ -> ch_), or the refund sharing the original's transaction id (NMI
	// refund actions ride the original transaction record).
	var original *LocalPayment
	if bc.Charge != "" {
		original = paymentsByTxnID[bc.Charge]
	}
	sameID := paymentsByTxnID[t.TransactionID]
	if original == nil && sameID != nil {
		original = sameID
	}

	// Already recorded? Either a dedicated refund row exists under the
	// refund's own transaction id (negative amount / refunded link), or the
	// original payment is already marked refunded.
	if sameID != nil && (sameID.AmountCents < 0 || sameID.RefundedPaymentID != nil || sameID.Status == "refunded") {
		return Finding{}, false
	}
	if sameID != nil && original != nil && sameID.ID == original.ID && original.Status == "refunded" {
		return Finding{}, false
	}
	if sameID != nil && sameID != original {
		// The refund's transaction id exists locally as something else
		// entirely — treat as recorded to avoid double-counting.
		return Finding{}, false
	}
	if original != nil && original.Status == "refunded" {
		return Finding{}, false
	}

	f := Finding{
		Provider:       provider,
		Type:           FindingRefundUnrecorded,
		SubjectKey:     t.TransactionID,
		Severity:       SeverityHigh,
		Status:         FindingStatusReconcileRequired,
		RemoteEvidence: remoteTxnEvidence(t),
	}

	if original == nil {
		f.Status = FindingStatusAdminRequired
		f.RequiresAdmin = true
		f.RecommendedAction = "rail refund references no locally-known payment; investigate manually"
		return f, true
	}

	f.LocalEvidence = map[string]any{
		"payment_id":     original.ID.String(),
		"customer_id":    original.CustomerID.String(),
		"transaction_id": original.TransactionID,
		"amount_cents":   original.AmountCents,
		"status":         original.Status,
	}
	f.RecommendedAction = "enforce records the refund locally (negative payment row + original marked refunded). Revoking any entitlement the refunded payment granted is a human decision — review in the admin queue if warranted"

	originalID := original.ID
	action := &RecordRefundAction{
		Rail:              original.Rail,
		TransactionID:     t.TransactionID,
		AmountCents:       t.AmountCents,
		Currency:          strings.ToLower(t.Currency),
		PurchasedAt:       t.OccurredAt,
		RefundedPaymentID: &originalID,
		SubscriptionID:    original.SubscriptionID,
		CustomerID:        original.CustomerID,
		Metadata: map[string]any{
			"reconcile_refund_backfill": true,
			"provider":                  string(provider),
		},
	}
	if sub := original.SubscriptionID; sub != nil {
		if s, ok := corr.local.byID[*sub]; ok && s.PriceID != nil {
			action.PriceID = *s.PriceID
		}
	}
	if action.PriceID == uuid.Nil {
		// Without a price the insert can't satisfy NOT NULL; mark the
		// original refunded only.
		action.MarkRefundedOnly = true
	}
	if t.TransactionID == original.TransactionID {
		// NMI refunds ride the original transaction record; inserting would
		// collide with the sale row. Mark the original refunded instead.
		action.MarkRefundedOnly = true
	}
	f.Apply = &ApplyAction{RecordRefund: action}
	return f, true
}

func makePS6(provider Provider, t *RemoteTransaction, corr *correlator, paymentsByTxnID map[string]*LocalPayment) (Finding, bool) {
	sub, how, _ := corr.subForTxn(t)
	if sub == nil {
		// Try via the disputed charge's local payment.
		bc := decodeBreadcrumbs(t.Raw)
		if bc.Charge != "" {
			if p := paymentsByTxnID[bc.Charge]; p != nil && p.SubscriptionID != nil {
				if s, ok := corr.local.byID[*p.SubscriptionID]; ok {
					sub, how = s, "disputed_charge"
				}
			}
		}
	}
	if sub == nil || !sub.IsLive() {
		return Finding{}, false // chargeback against a dead/unknown subscription is history, not drift
	}
	localEv := localSubEvidence(sub)
	localEv["correlated_via"] = how
	return Finding{
		Provider:          provider,
		Type:              FindingChargebackActiveSub,
		SubjectKey:        t.TransactionID,
		Severity:          SeverityCritical,
		Status:            FindingStatusAdminRequired,
		RequiresAdmin:     true,
		LocalEvidence:     localEv,
		RemoteEvidence:    remoteTxnEvidence(t),
		RecommendedAction: "chargeback received while the subscription is still active; review, then terminate + revoke and/or fight the dispute (remote action, admin-only)",
	}, true
}

// diffPaymentMethods covers PS-7.
func diffPaymentMethods(provider Provider, local *LocalState, ridx *remoteIndex, traits providerTraits) []Finding {
	var findings []Finding
	for i := range local.PaymentMethods {
		pm := &local.PaymentMethods[i]
		if pm.RailCustomerRef == "" {
			continue
		}
		remote, ok := ridx.remoteByRailCustomerRef[pm.RailCustomerRef]
		if !ok {
			if !traits.paymentMethodsExhaustive {
				continue // partial vault listing: absence proves nothing
			}
			findings = append(findings, Finding{
				Provider:   provider,
				Type:       FindingPaymentMethodMismatch,
				SubjectKey: pm.RailCustomerRef,
				Severity:   SeverityMedium,
				Status:     FindingStatusReconcileRequired,
				LocalEvidence: map[string]any{
					"payment_method_id": pm.ID.String(),
					"customer_id":       pm.CustomerID.String(),
					"vault_id":          pm.RailCustomerRef,
					"last_four":         pm.LastFour,
					"expiry_date":       pm.ExpiryDate,
				},
				RemoteEvidence:    map[string]any{"absent_from_vault": true},
				RecommendedAction: "stored payment method no longer exists in the rail vault; future charges with it will fail — re-collect the card or remove the method (manual)",
			})
			continue
		}
		remoteLast4 := strings.TrimSpace(remote.CardLast4)
		remoteExp := normalizeExpiry(remote.CardExpiry)
		localExp := normalizeExpiry(pm.ExpiryDate)
		last4Differs := remoteLast4 != "" && pm.LastFour != "" && remoteLast4 != pm.LastFour
		expDiffers := remoteExp != "" && localExp != "" && remoteExp != localExp
		missingLocal := (pm.LastFour == "" && remoteLast4 != "") || (localExp == "" && remoteExp != "")
		if !last4Differs && !expDiffers && !missingLocal {
			continue
		}
		findings = append(findings, Finding{
			Provider:   provider,
			Type:       FindingPaymentMethodMismatch,
			SubjectKey: pm.RailCustomerRef,
			Severity:   SeverityMedium,
			Status:     FindingStatusReconcileRequired,
			LocalEvidence: map[string]any{
				"payment_method_id": pm.ID.String(),
				"customer_id":       pm.CustomerID.String(),
				"vault_id":          pm.RailCustomerRef,
				"last_four":         pm.LastFour,
				"expiry_date":       pm.ExpiryDate,
			},
			RemoteEvidence: map[string]any{
				"card_last4":  remote.CardLast4,
				"card_expiry": remote.CardExpiry,
				"email":       remote.Email,
			},
			RecommendedAction: "stored card metadata disagrees with the rail vault (likely an account-updater change); enforce adopts the rail record",
			Apply: &ApplyAction{AdoptPaymentMethod: &AdoptPaymentMethodAction{
				PaymentMethodID: pm.ID,
				LastFour:        remoteLast4,
				ExpiryDate:      remote.CardExpiry,
			}},
		})
	}
	return findings
}

// normalizeExpiry reduces "10/27", "1027", "10-27" to "1027" for comparison.
func normalizeExpiry(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}
