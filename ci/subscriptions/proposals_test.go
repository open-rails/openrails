//go:build e2e && integration

package subscriptions_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/hosttools"
	"github.com/open-rails/openrails/internal/nmimock"
)

// allowDestructive turns the kill switch on for this merchant without arming
// enforcing pulls: provider reads still apply nothing they would overwrite.
func (w *world) allowDestructive() {
	w.t.Helper()
	ctx := w.t.Context()
	_, err := w.pool.Exec(ctx, w.q(`UPDATE billing.destructive_action_switch SET enabled = true, updated_by = 'e2e'`))
	require.NoError(w.t, err)
	_, err = w.pool.Exec(ctx, w.q(`INSERT INTO billing.merchant_destructive_policy (merchant_id, destructive_actions_enabled, updated_by, reason)
		SELECT id, true, 'e2e', 'reviewed' FROM billing.merchants WHERE slug = $1
		ON CONFLICT (merchant_id) DO UPDATE SET destructive_actions_enabled = true, enforce_armed_at = NULL`), w.slug)
	require.NoError(w.t, err)
}

// proposalsCLI is `openrails proposals` against this world, from a process
// started with the given provider_write_mode, on the world's clock.
func (w *world) proposalsCLI(writeMode string) hosttools.ProposalsOptions {
	cfg := &config.Config{ProviderWriteMode: writeMode,
		DB: &config.DBConfig{URL: w.dsn}, Database: config.DatabaseConfig{Schema: w.schema, RiverSchema: w.schema}}
	return hosttools.ProposalsOptions{Config: cfg, PGXPool: w.pool, MerchantID: w.client[embedded].MerchantID(),
		Actor: "e2e", Format: "json", Clock: w.clock}
}

func (w *world) approveAs(writeMode string, set hosttools.ProposalSet) (billing.ProposalResults, error) {
	w.t.Helper()
	opts := w.proposalsCLI(writeMode)
	var out strings.Builder
	opts.Out = &out
	err := hosttools.ProposalsApprove(w.t.Context(), opts, set, "reviewed against the provider")
	w.t.Logf("proposals approve:\n%s", out.String())
	if err != nil {
		return billing.ProposalResults{}, err
	}
	var res billing.ProposalResults
	require.NoError(w.t, json.Unmarshal([]byte(out.String()), &res), out.String())
	return res, nil
}

func (w *world) approve(ids ...string) billing.ProposalResults {
	w.t.Helper()
	res, err := w.approveAs(config.ProviderWriteModeFull, hosttools.ProposalSet{IDs: ids})
	require.NoError(w.t, err)
	return res
}

func (w *world) dismiss(notes string, ids ...string) (billing.ProposalResults, error) {
	w.t.Helper()
	opts := w.proposalsCLI(config.ProviderWriteModeFull)
	var out strings.Builder
	opts.Out = &out
	if err := hosttools.ProposalsDismiss(w.t.Context(), opts, hosttools.ProposalSet{IDs: ids}, notes); err != nil {
		return billing.ProposalResults{}, err
	}
	var res billing.ProposalResults
	require.NoError(w.t, json.Unmarshal([]byte(out.String()), &res), out.String())
	return res, nil
}

// proposal is one finding's id (fnd_…), status and change kind.
type proposal struct {
	id, status, resolution, kind, resolvedBy string
	run                                      *uuid.UUID
}

// proposalFor reads the finding of findingType raised for subject.
func (w *world) proposalFor(findingType, subject string) proposal {
	w.t.Helper()
	var p proposal
	var id uuid.UUID
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT id, status, coalesce(resolution, ''), coalesce(change->>'kind', ''), coalesce(resolved_by, ''), approval_run_id
		FROM billing.reconciliation_findings WHERE finding_type = $1 AND subject_key = $2`), findingType, subject).
		Scan(&id, &p.status, &p.resolution, &p.kind, &p.resolvedBy, &p.run), "%s %s", findingType, subject)
	p.id = billing.FindingID(id).String()
	return p
}

func outcomes(res billing.ProposalResults) map[string]billing.ProposalOutcome {
	out := map[string]billing.ProposalOutcome{}
	for _, r := range res.Data {
		out[r.ID.String()] = r.Outcome
	}
	return out
}

func (w *world) advisoryPull() {
	w.t.Helper()
	require.NoError(w.t, w.cliPullAs(config.ProviderWriteModeFull, "nmi", false, false), "advisory CLI pull")
}

// paymentsFor counts the local payments of a provider transaction.
func (w *world) paymentsFor(transactionID string) (n int, amount int64) {
	w.t.Helper()
	require.NoError(w.t, w.pool.QueryRow(w.t.Context(), w.q(`SELECT count(*), coalesce(sum(amount), 0) FROM billing.payments WHERE transaction_id = $1`), transactionID).Scan(&n, &amount))
	return n, amount
}

// A schedule NMI ended becomes a cancel proposal; nothing changes until a
// person approves it. Approving cancels through the decider with no provider
// write, records who approved and opens one approval run; undo-run reverses
// it. ResolveFinding refuses a pull.* finding.
func TestApproveCancelProposal(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.allowDestructive()
	gone, kept := importLegacy(t, w, "nmi", embedded), importLegacy(t, w, "nmi", embedded)
	w.converge()
	w.refreshProviders()
	w.nmi.DeleteSchedule(gone.railSub)
	w.advance(time.Hour)
	w.refreshProviders()

	p := w.proposalFor("pull.subscription.dead", gone.sub.UUID().String())
	require.Equal(t, "cancel_subscription", p.kind)
	require.Contains(t, []string{"reconcile_required", "requires_review"}, p.status)
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, gone.sub).Status, "a provider read applies nothing it would overwrite")

	writes := len(w.nmiWrites())
	res := w.approve(p.id)
	require.Equal(t, billing.ProposalApproved, outcomes(res)[p.id], "%+v", res)
	require.NotNil(t, res.RunID)
	sub := w.subscription(embedded, gone.sub)
	require.Equal(t, billing.SubscriptionCanceled, sub.Status)
	require.NotNil(t, sub.CanceledAt)
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, kept.sub).Status)
	require.Len(t, w.nmiWrites(), writes, "approving a cancel writes nothing to the provider")

	p = w.proposalFor("pull.subscription.dead", gone.sub.UUID().String())
	require.Equal(t, "fixed", p.status)
	require.Equal(t, "approved", p.resolution)
	require.Equal(t, "cli:e2e", p.resolvedBy, "the approver is recorded")
	require.NotNil(t, p.run)
	require.Equal(t, res.RunID.UUID(), *p.run)
	var kind, actor string
	require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT kind, actor FROM billing.maintenance_runs WHERE id = $1`), *p.run).Scan(&kind, &actor))
	require.Equal(t, "approval", kind)
	require.Equal(t, "cli:e2e", actor)

	status, body := w.staffJSON(http.MethodPost, "/v1/merchant/findings/"+p.id+"/resolve", map[string]any{"outcome": "ignore", "notes": "x"})
	require.Equal(t, http.StatusConflict, status, "%v", body)
	require.Equal(t, "finding_requires_merchant_config", errorCode(body))

	again := w.approve(p.id)
	require.Equal(t, billing.ProposalAlreadyResolved, outcomes(again)[p.id])
	require.Nil(t, again.RunID, "nothing applied, no run")

	w.undoRun(*p.run)
	sub = w.subscription(embedded, gone.sub)
	require.Equal(t, billing.SubscriptionActive, sub.Status, "undo-run restores the subscription")
	require.Nil(t, sub.CanceledAt)
	require.True(t, gone.c.entitled(gone.ent), "and its access")
}

// Approving after the subject moved, or on evidence older than seven days for
// an end of access, supersedes the proposal and applies nothing.
func TestApproveSupersededWhenSubjectMoved(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.allowDestructive()
	// A third member keeps NMI's roster non-empty: only a roster with rows
	// proves the other two absent.
	moved, stale, _ := importLegacy(t, w, "nmi", embedded), importLegacy(t, w, "nmi", embedded), importLegacy(t, w, "nmi", embedded)
	w.converge()
	w.refreshProviders()
	w.nmi.DeleteSchedule(moved.railSub)
	w.nmi.DeleteSchedule(stale.railSub)
	w.advance(time.Hour)
	w.refreshProviders()
	movedP := w.proposalFor("pull.subscription.dead", moved.sub.UUID().String())
	staleP := w.proposalFor("pull.subscription.dead", stale.sub.UUID().String())
	require.Equal(t, "cancel_subscription", movedP.kind)

	// Staff cancel it before anyone approves: the row the proposal was
	// computed against moved.
	_, err := w.client[embedded].CancelSubscription(t.Context(), moved.sub, billing.CancelSubscriptionParams{Reason: "customer asked"})
	require.NoError(t, err)
	w.settle()
	before := w.subscription(embedded, moved.sub)
	require.Equal(t, billing.SubscriptionCanceled, before.Status)

	res := w.approve(movedP.id)
	require.Equal(t, billing.ProposalSuperseded, outcomes(res)[movedP.id], "%+v", res)
	require.Nil(t, res.RunID, "nothing applied, no run")
	after := w.subscription(embedded, moved.sub)
	require.Equal(t, before.CanceledAt, after.CanceledAt, "the moved row is left as it is")
	require.Equal(t, before.CancelFeedback, after.CancelFeedback)
	require.Equal(t, "superseded", w.proposalFor("pull.subscription.dead", moved.sub.UUID().String()).resolution)

	w.advance(8 * day)
	res = w.approve(staleP.id)
	require.Equal(t, billing.ProposalSuperseded, outcomes(res)[staleP.id], "%+v", res)
	require.Contains(t, res.Data[0].Finding.Evidence["resolution"], "superseded")
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, stale.sub).Status, "a stale end of access is never applied")
}

// The kill switch holds ends-access changes and a readonly posture holds
// money movement; each stays open while the other ids of the same call
// apply. A charge after cancellation is refunded only on approval, once.
func TestApproveHeldByKillSwitchOrReadonly(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	gone, renewed, late := importLegacy(t, w, "nmi", embedded), importLegacy(t, w, "nmi", embedded), importLegacy(t, w, "nmi", embedded)
	w.converge()
	w.refreshProviders()
	_, err := w.client[embedded].CancelSubscription(t.Context(), late.sub, billing.CancelSubscriptionParams{Reason: "account deleted", AccountDeletion: true})
	require.NoError(t, err)
	w.settle()
	require.Equal(t, billing.SubscriptionCanceled, w.subscription(embedded, late.sub).Status)
	w.advance(time.Minute)
	w.nmi.DeleteSchedule(gone.railSub)
	missed := w.nmi.AddScheduleSale(renewed.railSub, w.clock.Now())
	afterCancel := w.nmi.AddScheduleSale(late.railSub, w.clock.Now())
	w.advance(time.Minute)
	w.advisoryPull()

	cancelP := w.proposalFor("pull.subscription.dead", gone.sub.UUID().String())
	chargeP := w.proposalFor("pull.charge.missing", missed.TransactionID)
	lateChargeP := w.proposalFor("pull.charge.missing", afterCancel.TransactionID)
	refundP := w.proposalFor("pull.charge.after_cancel", afterCancel.TransactionID)
	require.Equal(t, "refund_charge_after_cancel", refundP.kind)
	refunds := func() (n int, cents int64) {
		require.NoError(t, w.pool.QueryRow(t.Context(), w.q(`SELECT count(*), coalesce(sum((payload->>'amount_cents')::bigint), 0) FROM billing.provider_intents
			WHERE payment_id = (SELECT id FROM billing.payments WHERE transaction_id = $1) AND intent_type LIKE '%refund%'`), afterCancel.TransactionID).Scan(&n, &cents))
		return n, cents
	}

	// The kill switch ships off.
	res := w.approve(cancelP.id, chargeP.id)
	require.Equal(t, billing.ProposalHeld, outcomes(res)[cancelP.id], "%+v", res)
	require.Equal(t, billing.CodeDestructiveActionsHeld, res.Data[0].Error.Code)
	require.Equal(t, billing.ProposalApproved, outcomes(res)[chargeP.id], "%+v", res)
	require.Equal(t, billing.SubscriptionActive, w.subscription(embedded, gone.sub).Status)
	require.Contains(t, []string{"reconcile_required", "requires_review"}, w.proposalFor("pull.subscription.dead", gone.sub.UUID().String()).status, "a held proposal stays open")
	n, amount := w.paymentsFor(missed.TransactionID)
	require.Equal(t, 1, n)
	require.EqualValues(t, 9_990_000, amount, "exact micros")

	res, err = w.approveAs(config.ProviderWriteModeReadOnly, hosttools.ProposalSet{IDs: []string{lateChargeP.id, refundP.id}})
	require.NoError(t, err)
	require.Equal(t, billing.ProposalApproved, outcomes(res)[lateChargeP.id], "%+v", res)
	require.Equal(t, billing.ProposalHeld, outcomes(res)[refundP.id], "%+v", res)
	require.Equal(t, billing.CodeProviderWritesHeld, res.Data[1].Error.Code)
	n, _ = refunds()
	require.Zero(t, n, "a charge after cancellation is refunded only on approval")
	require.Contains(t, w.openFindings("pull.charge.after_cancel"), afterCancel.TransactionID)

	res = w.approve(refundP.id)
	require.Equal(t, billing.ProposalApproved, outcomes(res)[refundP.id], "%+v", res)
	n, cents := refunds()
	require.Equal(t, 1, n)
	require.EqualValues(t, 999, cents, "the full charge, in NMI cents")
	res = w.approve(refundP.id)
	require.Equal(t, billing.ProposalAlreadyResolved, outcomes(res)[refundP.id])
	n, _ = refunds()
	require.Equal(t, 1, n, "approving twice queues no second refund")
}

// A listed set applies only when --expect matches it, each id in its own
// transaction with exact micros; the call is refused whole for an unknown id
// or more than 100 ids.
func TestApproveListedBatch(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	var missed []nmimock.Sale
	for range 3 {
		l := importLegacy(t, w, "nmi", embedded)
		missed = append(missed, w.nmi.AddScheduleSale(l.railSub, w.clock.Now().Add(-time.Hour)))
	}
	w.converge()
	w.advisoryPull()
	set := hosttools.ProposalSet{Kind: "record_charge", Expect: 2}
	_, err := w.approveAs(config.ProviderWriteModeFull, set)
	require.ErrorContains(t, err, "--expect 2 does not match the 3")
	for _, sale := range missed {
		n, _ := w.paymentsFor(sale.TransactionID)
		require.Zero(t, n, "a mismatched set applies nothing")
	}

	set.Expect = 3
	res, err := w.approveAs(config.ProviderWriteModeFull, set)
	require.NoError(t, err)
	require.Len(t, res.Data, 3)
	require.NotNil(t, res.RunID, "one approval run for the call")
	var ids []string
	for _, r := range res.Data {
		require.Equal(t, billing.ProposalApproved, r.Outcome, "%+v", r)
		ids = append(ids, r.ID.String())
	}
	for _, sale := range missed {
		n, amount := w.paymentsFor(sale.TransactionID)
		require.Equal(t, 1, n)
		require.EqualValues(t, 9_990_000, amount)
	}
	res = w.approve(ids...)
	for _, r := range res.Data {
		require.Equal(t, billing.ProposalAlreadyResolved, r.Outcome)
	}

	_, err = w.approveAs(config.ProviderWriteModeFull, hosttools.ProposalSet{IDs: []string{ids[0], billing.FindingID(uuid.New()).String()}})
	require.ErrorContains(t, err, "is not a proposal")
	tooMany := make([]string, 101)
	for i := range tooMany {
		tooMany[i] = billing.FindingID(uuid.New()).String()
	}
	_, err = w.approveAs(config.ProviderWriteModeFull, hosttools.ProposalSet{IDs: tooMany})
	require.ErrorContains(t, err, "ids names 1 to 100 proposals")
}

// Dismissing needs notes. The same provider evidence never raises a dismissed
// proposal again; changed evidence does, and approving it writes the card
// version under the proposal's name.
func TestDismissedProposalStaysDismissed(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	e := enroll(t, w, "nmi", embedded)
	vault := w.vaultOf(e.method)
	w.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Card.Exp = "0331" })
	w.advisoryPull()
	p := w.proposalFor("pull.payment_method.mismatch", vault)
	require.Equal(t, "update_card", p.kind)

	_, err := w.dismiss("", p.id)
	require.ErrorContains(t, err, "notes are required")
	res, err := w.dismiss("customer confirmed the old card", p.id)
	require.NoError(t, err)
	require.Equal(t, billing.ProposalDismissed, outcomes(res)[p.id])
	require.Equal(t, "dismissed", w.proposalFor("pull.payment_method.mismatch", vault).resolution)

	w.advisoryPull()
	require.NotContains(t, w.openFindings("pull.payment_method.mismatch"), vault, "the same evidence raises nothing")

	w.nmi.EditVault(vault, func(v *nmimock.Vault) { v.Card.Exp = "0433" })
	w.advisoryPull()
	require.Contains(t, w.openFindings("pull.payment_method.mismatch"), vault, "changed evidence raises it again")
	res = w.approve(p.id)
	require.Equal(t, billing.ProposalApproved, outcomes(res)[p.id], "%+v", res)
	require.Equal(t, []string{"customer_save/saved", "provider_read/updated"}, w.methodVersions(e.method))
	require.Equal(t, "4/2033", w.methodRow(e.method, "card_exp_month::text || '/' || card_exp_year::text"))
}
