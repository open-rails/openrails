package reconcile

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// localIndex precomputes the lookups the diff checks share.
type localIndex struct {
	byPSID       map[string]*LocalSubscription
	byID         map[uuid.UUID]*LocalSubscription
	byEmail      map[string][]*LocalSubscription
	bySubject    map[uuid.UUID][]*LocalSubscription
	entsBySource map[uuid.UUID][]*LocalEntitlement
	pmByVault    map[string]*LocalPaymentMethod
	pmByID       map[uuid.UUID]*LocalPaymentMethod
}

func buildLocalIndex(local *LocalState) *localIndex {
	idx := &localIndex{
		byPSID:       map[string]*LocalSubscription{},
		byID:         map[uuid.UUID]*LocalSubscription{},
		byEmail:      map[string][]*LocalSubscription{},
		bySubject:    map[uuid.UUID][]*LocalSubscription{},
		entsBySource: map[uuid.UUID][]*LocalEntitlement{},
		pmByVault:    map[string]*LocalPaymentMethod{},
		pmByID:       map[uuid.UUID]*LocalPaymentMethod{},
	}
	for i := range local.Subscriptions {
		s := &local.Subscriptions[i]
		idx.byID[s.ID] = s
		idx.bySubject[s.TenantSubjectID] = append(idx.bySubject[s.TenantSubjectID], s)
		if s.ProcessorSubscriptionID != "" {
			// Prefer the live row when several local rows share one processor
			// subscription id (supersede history).
			if prev, ok := idx.byPSID[s.ProcessorSubscriptionID]; !ok || (!prev.IsLive() && s.IsLive()) {
				idx.byPSID[s.ProcessorSubscriptionID] = s
			}
		}
		if email := strings.ToLower(strings.TrimSpace(s.UserEmail)); email != "" {
			idx.byEmail[email] = append(idx.byEmail[email], s)
		}
	}
	for i := range local.Entitlements {
		ent := &local.Entitlements[i]
		idx.entsBySource[ent.SourceID] = append(idx.entsBySource[ent.SourceID], ent)
	}
	for i := range local.PaymentMethods {
		pm := &local.PaymentMethods[i]
		idx.pmByVault[pm.VaultID] = pm
		idx.pmByID[pm.ID] = pm
	}
	return idx
}

// remoteIndex precomputes snapshot lookups.
type remoteIndex struct {
	subByPSID     map[string]*RemoteSubscription
	subsByCust    map[string][]*RemoteSubscription
	vaultByID     map[string]*RemoteVaultEntry
	liveSubByPSID map[string]bool
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
		subByPSID:     map[string]*RemoteSubscription{},
		subsByCust:    map[string][]*RemoteSubscription{},
		vaultByID:     map[string]*RemoteVaultEntry{},
		liveSubByPSID: map[string]bool{},
	}
	for i := range snap.Subscriptions {
		r := &snap.Subscriptions[i]
		if r.ProcessorSubscriptionID == "" {
			continue
		}
		// On duplicate snapshot entries for one processor subscription id
		// (CCBill: ACTIVEMEMBERS roster + a windowed termination event), the
		// point-in-time roster entry (live) wins.
		if prev, ok := idx.subByPSID[r.ProcessorSubscriptionID]; !ok || (!remoteLive(prev.Status) && remoteLive(r.Status)) {
			idx.subByPSID[r.ProcessorSubscriptionID] = r
		}
		if r.CustomerID != "" {
			idx.subsByCust[r.CustomerID] = append(idx.subsByCust[r.CustomerID], r)
		}
	}
	for psid, r := range idx.subByPSID {
		idx.liveSubByPSID[psid] = remoteLive(r.Status)
	}
	for i := range snap.VaultEntries {
		v := &snap.VaultEntries[i]
		if v.CustomerVaultID != "" {
			idx.vaultByID[v.CustomerVaultID] = v
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
	// 1. Direct processor subscription id (CCBill, Solana).
	if t.SubscriptionID != "" {
		if s, ok := c.local.byPSID[t.SubscriptionID]; ok {
			return s, "processor_subscription_id", false
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
	for _, vaultID := range []string{bc.CustomerVaultID, bc.Customer} {
		if vaultID == "" {
			continue
		}
		if pm, ok := c.local.pmByVault[vaultID]; ok {
			subs := c.local.bySubject[pm.TenantSubjectID]
			if s, ok := uniqueSub(subs); ok {
				return s, "vault_id", false
			}
			if len(subs) > 1 {
				ambiguous = true
			}
		}
		// 4. Same customer id on a remote subscription whose processor
		// subscription id matches locally (Stripe charges: customer ->
		// subscription roster -> local).
		var candidates []*LocalSubscription
		for _, r := range c.remote.subsByCust[vaultID] {
			if s, ok := c.local.byPSID[r.ProcessorSubscriptionID]; ok {
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
		"subscription_id":           s.ID.String(),
		"tenant_subject_id":         s.TenantSubjectID.String(),
		"status":                    s.Status,
		"processor":                 s.Processor,
		"processor_subscription_id": s.ProcessorSubscriptionID,
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
		"processor_subscription_id": r.ProcessorSubscriptionID,
		"status":                    string(r.Status),
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
// when a local subscription carries a recorded-but-unexecuted processor
// delete marker.
func deletionIntent(s *LocalSubscription) map[string]any {
	if s.DeletionScheduledAt == nil {
		return nil
	}
	return map[string]any{
		"deletion_scheduled_at": s.DeletionScheduledAt.Format(time.RFC3339),
		"explanation":           "local cancellation recorded a deferred processor delete that has not executed yet; the boot rescan replays the recorded delete",
	}
}

const deletionIntentAction = "no admin action needed: boot rescan replays the recorded delete (deletion_scheduled_at marker present)"

// diffProvider runs every capability-gated check of the PS-1..PS-9 taxonomy
// for one provider snapshot vs local state and returns the findings, each
// carrying its enforce instruction where one is safe.
func diffProvider(provider Provider, snap *RemoteSnapshot, local *LocalState, localPayments []LocalPayment, now time.Time) []Finding {
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
		findings = append(findings, diffSubscriptions(provider, snap, idx, ridx, traits, now)...)
		findings = append(findings, diffDuplicates(provider, idx, ridx)...)
	}
	if caps.Transactions {
		findings = append(findings, diffTransactions(provider, snap, idx, corr, paymentsByTxnID, now)...)
	}
	if caps.Vault {
		findings = append(findings, diffVault(provider, local, ridx, traits)...)
	}
	// PS-9 is local<->local consistency scoped to this provider's
	// subscriptions; it needs no remote capability.
	findings = append(findings, diffEntitlements(provider, local, idx, now)...)

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].Type != findings[j].Type {
			return findings[i].Type < findings[j].Type
		}
		return findings[i].SubjectKey < findings[j].SubjectKey
	})
	return findings
}

// diffSubscriptions covers PS-1, PS-2 and PS-3.
func diffSubscriptions(provider Provider, snap *RemoteSnapshot, idx *localIndex, ridx *remoteIndex, traits providerTraits, now time.Time) []Finding {
	var findings []Finding

	// Remote -> local: PS-1 and presence-based PS-2/PS-3.
	for psid, r := range ridx.subByPSID {
		localSub, matched := idx.byPSID[psid]
		if !matched {
			if !remoteLive(r.Status) {
				// A terminated remote subscription we never knew is historical
				// noise, not actionable drift.
				continue
			}
			findings = append(findings, makePS1(provider, r, idx))
			continue
		}

		// Matched pair: status comparison.
		findings = append(findings, compareStatuses(provider, localSub, r, now)...)
	}

	// Local -> remote: absence-based PS-2 (NMI: the recurring report only
	// lists live subscriptions, so a locally-live linked subscription that is
	// absent is dead at the processor). Guarded upstream by the circuit
	// breaker.
	if traits.absenceMeansCancelled {
		for _, s := range idx.byPSID {
			if !s.IsLive() {
				continue
			}
			if _, present := ridx.subByPSID[s.ProcessorSubscriptionID]; present {
				continue
			}
			f := Finding{
				Provider:      provider,
				Type:          FindingLocalActiveRemoteDead,
				SubjectKey:    s.ID.String(),
				Severity:      SeverityHigh,
				Status:        FindingStatusOpen,
				LocalEvidence: localSubEvidence(s),
				RemoteEvidence: map[string]any{
					"absent_from_recurring_report": true,
					"processor_subscription_id":    s.ProcessorSubscriptionID,
				},
				RecommendedAction: "processor no longer bills this subscription; enforce cancels it locally and revokes its subscription-sourced entitlements",
				Apply: &ApplyAction{CancelLocal: &CancelLocalAction{
					SubscriptionID: s.ID,
					CancelType:     "expired",
					Reason:         "reconcile: absent from " + string(provider) + " recurring report",
				}},
			}
			if intent := deletionIntent(s); intent != nil {
				f.IntentEvidence = intent
			}
			findings = append(findings, f)
		}
	}

	return findings
}

func makePS1(provider Provider, r *RemoteSubscription, idx *localIndex) Finding {
	remoteEv := remoteSubEvidence(r)
	if len(r.Raw) > 0 {
		var raw any
		if err := json.Unmarshal(r.Raw, &raw); err == nil {
			remoteEv["raw"] = raw
		}
	}
	f := Finding{
		Provider:       provider,
		Type:           FindingRemoteSubMissingLocal,
		SubjectKey:     r.ProcessorSubscriptionID,
		Severity:       SeverityCritical,
		Status:         FindingStatusAdminPending,
		RequiresAdmin:  true,
		RemoteEvidence: remoteEv,
		RecommendedAction: "the processor is billing a subscription OpenRails does not know; investigate and either import it locally or cancel it at the processor (remote action, admin-only). No auto-create in v1",
	}
	// Email fallback: surface candidates so the admin sees the likely owner,
	// but never auto-link (zero or multiple matches = human decision).
	if email := strings.ToLower(strings.TrimSpace(r.Email)); email != "" {
		if candidates := idx.byEmail[email]; len(candidates) > 0 {
			matches := make([]map[string]any, 0, len(candidates))
			for _, c := range candidates {
				matches = append(matches, map[string]any{
					"subscription_id":           c.ID.String(),
					"tenant_subject_id":         c.TenantSubjectID.String(),
					"status":                    c.Status,
					"processor_subscription_id": c.ProcessorSubscriptionID,
				})
			}
			f.LocalEvidence = map[string]any{"email_candidates": matches}
			if len(candidates) == 1 {
				f.RecommendedAction = "the processor bills a subscription unknown locally, but exactly one local subscription shares its email (see local_evidence); verify and link/import manually. No auto-create in v1"
			}
		}
	}
	return f
}

// compareStatuses diffs one matched (local, remote) subscription pair into
// PS-2/PS-3 findings.
func compareStatuses(provider Provider, s *LocalSubscription, r *RemoteSubscription, now time.Time) []Finding {
	if r.Status == SubscriptionStatusUnknown {
		return nil // the provider declared nothing comparable
	}

	localLive := s.IsLive()
	remoteDead := r.Status == SubscriptionStatusCancelled || r.Status == SubscriptionStatusExpired

	switch {
	case localLive && remoteDead:
		// PS-2: processor terminated it, local still bills/grants.
		cancelType := "expired"
		f := Finding{
			Provider:       provider,
			Type:           FindingLocalActiveRemoteDead,
			SubjectKey:     s.ID.String(),
			Severity:       SeverityHigh,
			Status:         FindingStatusOpen,
			LocalEvidence:  localSubEvidence(s),
			RemoteEvidence: remoteSubEvidence(r),
			RecommendedAction: "processor reports this subscription " + string(r.Status) + "; enforce cancels it locally and revokes its subscription-sourced entitlements",
			Apply: &ApplyAction{CancelLocal: &CancelLocalAction{
				SubscriptionID: s.ID,
				CancelType:     cancelType,
				Reason:         fmt.Sprintf("reconcile: %s reports %s", provider, r.Status),
			}},
		}
		if intent := deletionIntent(s); intent != nil {
			f.IntentEvidence = intent
		}
		return []Finding{f}

	case !localLive && remoteLive(r.Status):
		// PS-3 (inverse direction): local says dead, processor still bills.
		// Never auto-resurrect a locally-terminated subscription; this is
		// either a recorded-but-unexecuted delete (intent annotation) or an
		// orphan the admin must cancel AT the processor.
		f := Finding{
			Provider:       provider,
			Type:           FindingStatusMismatch,
			SubjectKey:     s.ID.String(),
			Severity:       SeverityMedium,
			Status:         FindingStatusAdminPending,
			RequiresAdmin:  true,
			LocalEvidence:  localSubEvidence(s),
			RemoteEvidence: remoteSubEvidence(r),
			RecommendedAction: "subscription is terminated locally but the processor still bills it; cancel it AT the processor (remote action, admin-only)",
		}
		if intent := deletionIntent(s); intent != nil {
			f.IntentEvidence = intent
			f.Status = FindingStatusOpen
			f.RequiresAdmin = false
			f.RecommendedAction = deletionIntentAction
		}
		return []Finding{f}

	case localLive && remoteLive(r.Status) && string(r.Status) != s.Status:
		// PS-3: both live, different flavor (active vs past_due, pending vs
		// active). Adopt the processor's status; past_due adoption needs a
		// period end (DB constraint), guarded here.
		adopt := &AdoptStatusAction{
			SubscriptionID: s.ID,
			Status:         string(r.Status),
			PeriodEndsAt:   r.NextBillingAt,
			PeriodStartsAt: r.LastBilledAt,
		}
		var apply *ApplyAction
		if r.Status == SubscriptionStatusPastDue && r.NextBillingAt == nil && s.CurrentPeriodEndsAt == nil {
			apply = nil // cannot satisfy chk_past_due_has_period_end
		} else {
			apply = &ApplyAction{AdoptStatus: adopt}
		}
		f := Finding{
			Provider:       provider,
			Type:           FindingStatusMismatch,
			SubjectKey:     s.ID.String(),
			Severity:       SeverityMedium,
			Status:         FindingStatusOpen,
			LocalEvidence:  localSubEvidence(s),
			RemoteEvidence: remoteSubEvidence(r),
			RecommendedAction: fmt.Sprintf("adopt processor status %q (local %q) and its period timestamps", r.Status, s.Status),
			Apply:          apply,
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
					Provider:       provider,
					Type:           FindingStatusMismatch,
					SubjectKey:     s.ID.String(),
					Severity:       SeverityMedium,
					Status:         FindingStatusOpen,
					LocalEvidence:  localSubEvidence(s),
					RemoteEvidence: remoteSubEvidence(r),
					RecommendedAction: fmt.Sprintf("period end drifts %.0fh from the processor's next billing date; adopt the processor's period timestamps", drift.Hours()),
					Apply: &ApplyAction{AdoptStatus: &AdoptStatusAction{
						SubscriptionID: s.ID,
						Status:         s.Status,
						PeriodStartsAt: r.LastBilledAt,
						PeriodEndsAt:   r.NextBillingAt,
					}},
				}}
			}
		}
	}
	return nil
}

// diffDuplicates covers PS-8: one subject carrying multiple live
// subscriptions at the processor (same tier locally, or several unmatched
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
		if l, ok := idx.byPSID[r.ProcessorSubscriptionID]; ok {
			// Matched: duplicates within one (subject, tier group).
			addGroup("subject:"+l.TenantSubjectID.String()+"|tier:"+l.TierGroup, r, l)
			continue
		}
		// Unmatched: duplicates on the same processor-side identity + plan.
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
			Provider:       provider,
			Type:           FindingDuplicateSubscriptions,
			SubjectKey:     g.key,
			Severity:       SeverityHigh,
			Status:         FindingStatusAdminPending,
			RequiresAdmin:  true,
			LocalEvidence:  localEv,
			RemoteEvidence: remoteEv,
			RecommendedAction: "one subject is billed by multiple live processor subscriptions; pick the keeper, cancel the duplicate AT the processor and refund the double charge (remote action, admin-only)",
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
			if !t.Success || t.AmountCents <= 0 || t.TransactionID == "" {
				continue
			}
			if _, ok := paymentsByTxnID[t.TransactionID]; ok {
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
		Status:         FindingStatusOpen,
		RemoteEvidence: remoteTxnEvidence(t),
	}
	switch {
	case sub == nil:
		f.Status = FindingStatusAdminPending
		f.RequiresAdmin = true
		if ambiguous {
			f.RecommendedAction = "processor charge has no local payment and its identity matches MULTIPLE local candidates; resolve manually (never guess)"
		} else {
			f.RecommendedAction = "processor charge has no local payment and no local identity could be resolved; investigate manually"
		}
	case sub.PriceID == nil:
		f.Status = FindingStatusAdminPending
		f.RequiresAdmin = true
		f.LocalEvidence = localSubEvidence(sub)
		f.RecommendedAction = "processor charge correlates to a local subscription without a price; backfill manually"
	default:
		f.LocalEvidence = localSubEvidence(sub)
		f.LocalEvidence["correlated_via"] = how
		f.RecommendedAction = "enforce backfills the missing billing.payments row (deduped on processor+transaction_id)"
		subID := sub.ID
		action := &BackfillPaymentAction{
			Processor:       sub.Processor,
			TransactionID:   t.TransactionID,
			AmountCents:     t.AmountCents,
			Currency:        strings.ToLower(t.Currency),
			PurchasedAt:     t.OccurredAt,
			PriceID:         *sub.PriceID,
			SubscriptionID:  &subID,
			TenantSubjectID: sub.TenantSubjectID,
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
				SubscriptionID:  sub.ID,
				TenantSubjectID: sub.TenantSubjectID,
				Entitlements:    sub.EntitlementNames,
				StartAt:         start,
				EndAt:           sub.CurrentPeriodEndsAt,
			}
			f.RecommendedAction += "; the charge's period is current, so missing subscription entitlements are granted too"
		}
		f.Apply = &ApplyAction{BackfillPayment: action}
	}
	return f
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
		Status:         FindingStatusOpen,
		RemoteEvidence: remoteTxnEvidence(t),
	}

	if original == nil {
		f.Status = FindingStatusAdminPending
		f.RequiresAdmin = true
		f.RecommendedAction = "processor refund references no locally-known payment; investigate manually"
		return f, true
	}

	f.LocalEvidence = map[string]any{
		"payment_id":        original.ID.String(),
		"tenant_subject_id": original.TenantSubjectID.String(),
		"transaction_id":    original.TransactionID,
		"amount_cents":      original.AmountCents,
		"status":            original.Status,
	}
	f.RecommendedAction = "enforce records the refund locally (negative payment row + original marked refunded). Revoking any entitlement the refunded payment granted is a human decision — review in the admin queue if warranted"

	originalID := original.ID
	action := &RecordRefundAction{
		Processor:         original.Processor,
		TransactionID:     t.TransactionID,
		AmountCents:       t.AmountCents,
		Currency:          strings.ToLower(t.Currency),
		PurchasedAt:       t.OccurredAt,
		RefundedPaymentID: &originalID,
		SubscriptionID:    original.SubscriptionID,
		TenantSubjectID:   original.TenantSubjectID,
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
		Provider:       provider,
		Type:           FindingChargebackActiveSub,
		SubjectKey:     t.TransactionID,
		Severity:       SeverityCritical,
		Status:         FindingStatusAdminPending,
		RequiresAdmin:  true,
		LocalEvidence:  localEv,
		RemoteEvidence: remoteTxnEvidence(t),
		RecommendedAction: "chargeback received while the subscription is still active; review, then terminate + revoke and/or fight the dispute (remote action, admin-only)",
	}, true
}

// diffVault covers PS-7.
func diffVault(provider Provider, local *LocalState, ridx *remoteIndex, traits providerTraits) []Finding {
	var findings []Finding
	for i := range local.PaymentMethods {
		pm := &local.PaymentMethods[i]
		if pm.VaultID == "" {
			continue
		}
		remote, ok := ridx.vaultByID[pm.VaultID]
		if !ok {
			if !traits.vaultExhaustive {
				continue // partial vault listing: absence proves nothing
			}
			findings = append(findings, Finding{
				Provider:   provider,
				Type:       FindingVaultMismatch,
				SubjectKey: pm.VaultID,
				Severity:   SeverityMedium,
				Status:     FindingStatusOpen,
				LocalEvidence: map[string]any{
					"payment_method_id": pm.ID.String(),
					"tenant_subject_id": pm.TenantSubjectID.String(),
					"vault_id":          pm.VaultID,
					"last_four":         pm.LastFour,
					"expiry_date":       pm.ExpiryDate,
				},
				RemoteEvidence:    map[string]any{"absent_from_vault": true},
				RecommendedAction: "stored payment method no longer exists in the processor vault; future charges with it will fail — re-collect the card or remove the method (manual)",
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
			Type:       FindingVaultMismatch,
			SubjectKey: pm.VaultID,
			Severity:   SeverityMedium,
			Status:     FindingStatusOpen,
			LocalEvidence: map[string]any{
				"payment_method_id": pm.ID.String(),
				"tenant_subject_id": pm.TenantSubjectID.String(),
				"vault_id":          pm.VaultID,
				"last_four":         pm.LastFour,
				"expiry_date":       pm.ExpiryDate,
			},
			RemoteEvidence: map[string]any{
				"card_last4":  remote.CardLast4,
				"card_expiry": remote.CardExpiry,
				"email":       remote.Email,
			},
			RecommendedAction: "stored card metadata disagrees with the processor vault (likely an account-updater change); enforce adopts the processor record",
			Apply: &ApplyAction{AdoptVault: &AdoptVaultAction{
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

// diffEntitlements covers PS-9 in both directions, restricted to
// SUBSCRIPTION-sourced entitlements (admin grants are untouchable comps;
// grace windows are their own source type and are left alone).
func diffEntitlements(provider Provider, local *LocalState, idx *localIndex, now time.Time) []Finding {
	var findings []Finding
	for i := range local.Subscriptions {
		s := &local.Subscriptions[i]
		liveEnts := map[string]bool{}
		var liveCount int
		for _, ent := range idx.entsBySource[s.ID] {
			if ent.StartAt.After(now) {
				continue
			}
			if ent.EndAt != nil && !ent.EndAt.After(now) {
				continue
			}
			liveEnts[ent.Entitlement] = true
			liveCount++
		}

		switch {
		case s.Status == "active" && len(s.EntitlementNames) > 0:
			var missing []string
			for _, name := range s.EntitlementNames {
				if !liveEnts[name] {
					missing = append(missing, name)
				}
			}
			if len(missing) == 0 {
				continue
			}
			sort.Strings(missing)
			start := now
			if s.CurrentPeriodStartsAt != nil {
				start = *s.CurrentPeriodStartsAt
			}
			findings = append(findings, Finding{
				Provider:   provider,
				Type:       FindingEntitlementMismatch,
				SubjectKey: s.ID.String(),
				Severity:   SeverityHigh,
				Status:     FindingStatusOpen,
				LocalEvidence: map[string]any{
					"subscription_id":      s.ID.String(),
					"tenant_subject_id":    s.TenantSubjectID.String(),
					"status":               s.Status,
					"missing_entitlements": missing,
					"direction":            "grant",
				},
				RecommendedAction: "active subscription is missing subscription-sourced entitlements; enforce grants them for the current period",
				Apply: &ApplyAction{GrantEntitlements: &GrantEntitlementsAction{
					SubscriptionID:  s.ID,
					TenantSubjectID: s.TenantSubjectID,
					Entitlements:    missing,
					StartAt:         start,
					EndAt:           s.CurrentPeriodEndsAt,
				}},
			})

		case !s.IsLive() && liveCount > 0:
			names := make([]string, 0, len(liveEnts))
			for name := range liveEnts {
				names = append(names, name)
			}
			sort.Strings(names)
			findings = append(findings, Finding{
				Provider:   provider,
				Type:       FindingEntitlementMismatch,
				SubjectKey: s.ID.String(),
				Severity:   SeverityHigh,
				Status:     FindingStatusOpen,
				LocalEvidence: map[string]any{
					"subscription_id":   s.ID.String(),
					"tenant_subject_id": s.TenantSubjectID.String(),
					"status":            s.Status,
					"live_entitlements": names,
					"direction":         "revoke",
				},
				RecommendedAction: "subscription is not live but still grants live subscription-sourced entitlements; enforce revokes them (admin grants and grace windows untouched)",
				Apply: &ApplyAction{RevokeEntitlements: &RevokeEntitlementsAction{
					SubscriptionID: s.ID,
					Reason:         "reconcile: subscription " + s.Status + ", entitlement source not live",
				}},
			})
		}
	}
	return findings
}
