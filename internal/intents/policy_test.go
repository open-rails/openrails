package intents

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/ccbill"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
	"github.com/open-rails/openrails/pkg/merchant"
)

type fakeMode struct{ readonly, limited bool }

func (m fakeMode) IsProviderReadOnly() bool { return m.readonly }
func (m fakeMode) IsLimitedMode() bool      { return m.limited || m.readonly }

var (
	modeFull     = fakeMode{}
	modeLimited  = fakeMode{limited: true}
	modeReadonly = fakeMode{readonly: true}
)

func TestBackoffDoublesToCapAndDefendsInputs(t *testing.T) {
	p := BackoffPolicy{Base: 2 * time.Minute, Cap: 2 * time.Hour}
	for attempts, want := range map[int32]time.Duration{
		-3: 2 * time.Minute, 0: 2 * time.Minute, 1: 2 * time.Minute, 2: 4 * time.Minute,
		6: 64 * time.Minute, 7: 2 * time.Hour, 50: 2 * time.Hour,
	} {
		assert.Equal(t, want, p.Delay(attempts), "attempts=%d", attempts)
	}
	assert.Equal(t, time.Minute, BackoffPolicy{}.Delay(1), "zero policy falls back to defaults")
	assert.Equal(t, time.Hour, BackoffPolicy{}.Delay(30))
}

// IDEM-9: origin x mode, and an unknown origin or mode parks.
func TestGateExecutionFailsClosed(t *testing.T) {
	origins := []Origin{OriginUser, OriginAdmin, OriginSystem}
	for _, tc := range []struct {
		name    string
		mode    ModeView
		blocked map[Origin]bool
	}{
		{"full", modeFull, map[Origin]bool{}},
		{"limited", modeLimited, map[Origin]bool{OriginSystem: true}},
		{"readonly", modeReadonly, map[Origin]bool{OriginUser: true, OriginAdmin: true, OriginSystem: true}},
		{"nil mode", nil, map[Origin]bool{OriginUser: true, OriginAdmin: true, OriginSystem: true}},
	} {
		for _, origin := range origins {
			blocked, reason := GateExecution(tc.mode, origin)
			assert.Equal(t, tc.blocked[origin], blocked, "%s/%s", tc.name, origin)
			assert.Equal(t, blocked, reason != "", "%s/%s: a park always carries its reason", tc.name, origin)
		}
	}
	_, reason := GateExecution(nil, OriginUser)
	assert.Contains(t, reason, "operating mode is unknown")
	blocked, reason := GateExecution(modeFull, Origin("robot"))
	assert.True(t, blocked)
	assert.Contains(t, reason, "robot")
}

func TestDestructiveClassificationAndBudget(t *testing.T) {
	assert.Equal(t, []string{
		TypeCCBillCancelSubscription, TypeHyperSwitchMethodDelete, TypeNMIDeleteSubscription,
		TypeNMIEngineTakeover, TypeNMIProviderCutover, TypeNMIPaymentMethodDelete,
	}, DestructiveIntentTypes(), "sorted, complete breaker-gated set")
	for _, typ := range []string{subscriptions.TypeManualRebill, TypeNMIPaymentSourceUpdate, TypeNMIRefund, TypeStripeRefund, TypeStripeCancelSubscription} {
		assert.False(t, IsDestructiveIntentType(typ), typ)
	}
	for active, want := range map[int64]int64{0: 25, 100: 25, 2_499: 25, 2_500: 25, 2_600: 26, 100_000: 1_000} {
		assert.Equal(t, want, DestructiveBudget(active), "active=%d", active)
	}
}

func TestRateCeilingIsPinnedAndFailsClosed(t *testing.T) {
	// Hardcoded safeguards, not config: drift must fail the build.
	assert.Equal(t, time.Hour, RateCeilingWindow)
	assert.Equal(t, []int{5, 15, 50, 3, 8, 25}, []int{
		PerActorHourlyCeiling, PerMerchantHourlyCeiling, PerMerchantSystemHourlyCeiling,
		perActorWarnThreshold, perMerchantWarnThreshold, systemMerchantWarnThreshold,
	})
	findingType := regexp.MustCompile(`^(pull|derive|life|consistency)\.[a-z0-9_]+(\.[a-z0-9_]+)?$`)
	for _, ft := range []string{RateCeilingTrippedFindingType, RateCeilingWarningFindingType, HeldBulkFindingType} {
		assert.Regexp(t, findingType, ft, "must satisfy reconciliation_findings' type CHECK")
	}

	ctx, now := context.Background(), time.Now().UTC()
	destructive := DestructiveIntentTypes()[0]
	broken := NewRateCeiling(db.NewWithPgxTx(nil))
	var absent *RateCeiling
	for _, tc := range []struct {
		name    string
		gate    *RateCeiling
		params  CheckParams
		refused bool
	}{
		{"nil gate, user", absent, CheckParams{Actor: "a", MerchantID: uuid.New(), IntentType: destructive, Origin: OriginUser}, true},
		{"nil gate, system", absent, CheckParams{MerchantID: uuid.New(), IntentType: destructive, Origin: OriginSystem}, true},
		{"broken db, user", broken, CheckParams{Actor: "a", MerchantID: uuid.New(), IntentType: destructive, Origin: OriginUser}, true},
		{"broken db, admin", broken, CheckParams{Actor: "a", MerchantID: uuid.New(), IntentType: destructive, Origin: OriginAdmin}, true},
		{"broken db, system", broken, CheckParams{MerchantID: uuid.New(), IntentType: destructive, Origin: OriginSystem}, true},
		{"no merchant, user", broken, CheckParams{Actor: "a", IntentType: destructive, Origin: OriginUser}, true},
		{"no merchant, system", broken, CheckParams{IntentType: destructive, Origin: OriginSystem}, true},
		{"non-destructive passes a broken gate", broken, CheckParams{Actor: "a", MerchantID: uuid.New(), IntentType: TypeNMIPaymentSourceUpdate, Origin: OriginUser}, false},
		{"non-destructive passes a nil gate", absent, CheckParams{MerchantID: uuid.New(), IntentType: TypeNMIRefund, Origin: OriginSystem}, false},
	} {
		err := tc.gate.Check(ctx, tc.params, now)
		if !tc.refused {
			assert.NoError(t, err, tc.name)
			continue
		}
		require.Error(t, err, tc.name)
		// An evaluation failure is a hard error, never a client-facing "rate limited".
		assert.False(t, errors.Is(err, ErrRateCeilingTripped), tc.name)
		var rce *RateCeilingError
		assert.False(t, errors.As(err, &rce), tc.name)
	}

	assert.Equal(t, "explicit", ResolveActor(ctx, "explicit"))
	assert.Empty(t, ResolveActor(ctx, ""), "no principal: system/background path")
}

func TestOperatorResolutionNeedsAttributionAndExactlyOneEvidence(t *testing.T) {
	anchor := time.Date(2026, 7, 1, 12, 0, 0, 0, time.FixedZone("x", 3600))
	for _, tc := range []struct {
		name string
		r    Resolution
		ok   bool
		key  string
	}{
		{"reference", Resolution{ProviderReference: " txn_1 ", Actor: "op", Reason: "seen"}, true, "provider_reference"},
		{"not executed", Resolution{NotExecuted: true, Actor: "op", Reason: "r"}, true, "not_executed"},
		{"abandon", Resolution{Abandon: true, Actor: "op", Reason: "r"}, true, "abandon"},
		{"requalify", Resolution{RequalifyAccount: "acct", Actor: "op", Reason: "r"}, true, "requalify_account"},
		{"anchor", Resolution{BillingAnchor: anchor, Actor: "op", Reason: "r"}, true, "billing_anchor"},
		{"no evidence", Resolution{Actor: "op", Reason: "r"}, false, ""},
		{"two evidences", Resolution{ProviderReference: "x", NotExecuted: true, Actor: "op", Reason: "r"}, false, ""},
		{"blank actor", Resolution{NotExecuted: true, Actor: "  ", Reason: "r"}, false, ""},
		{"blank reason", Resolution{NotExecuted: true, Actor: "op", Reason: " "}, false, ""},
		{"blank reference is no evidence", Resolution{ProviderReference: "  ", Actor: "op", Reason: "r"}, false, ""},
	} {
		got, err := tc.r.normalized()
		if !tc.ok {
			assert.ErrorIs(t, err, ErrResolutionInvalid, tc.name)
			continue
		}
		require.NoError(t, err, tc.name)
		rec := got.Record(anchor)
		assert.Contains(t, rec, tc.key, tc.name)
		assert.Equal(t, "op", rec["actor"])
	}
	got, err := Resolution{ProviderReference: " txn_1 ", Actor: " op ", Reason: " seen "}.normalized()
	require.NoError(t, err)
	assert.Equal(t, "txn_1", got.Record(anchor)["provider_reference"])
	got, err = Resolution{BillingAnchor: anchor, Actor: "op", Reason: "r"}.normalized()
	require.NoError(t, err)
	assert.Equal(t, time.UTC, got.BillingAnchor.Location())
}

// IDEM-3: keys are content-addressed, stable, and separate distinct operations.
func TestIdempotencyKeysAreContentAddressed(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	assert.Equal(t, NMIDeleteIdempotencyKey(a, b, "t"), NMIDeleteIdempotencyKey(a, b, "t"))
	assert.NotEqual(t, NMIDeleteIdempotencyKey(a, b, "t"), NMIDeleteIdempotencyKey(a, b, "replacement"))

	assert.Equal(t, RefundIdempotencyKey(a, "key"), RefundIdempotencyKey(a, " key "))
	assert.NotEqual(t, RefundIdempotencyKey(a, "key"), RefundIdempotencyKey(a, "other"))
	assert.NotEqual(t, RefundIdempotencyKey(a, "key"), RefundIdempotencyKey(b, "key"))

	period := time.Date(2026, 6, 1, 0, 0, 0, 123456000, time.UTC)
	first := subscriptions.ManualRebillIdempotencyKey(a, period, "nmi", 0)
	assert.Equal(t, first, subscriptions.ManualRebillIdempotencyKey(a, period, "NMI", 0))
	assert.NotEqual(t, first, subscriptions.ManualRebillIdempotencyKey(a, period.Add(time.Microsecond), "nmi", 0))
	second := subscriptions.ManualRebillIdempotencyKey(a, period, "nmi", 1)
	assert.NotEqual(t, first, second)
	assert.Equal(t, subscriptions.ObligationOrderReference(a, period), subscriptions.ObligationOrderReference(a, period.In(time.FixedZone("x", 3600))),
		"every attempt of one period shares its order")
	assert.NotEqual(t, subscriptions.ObligationOrderReference(a, period), subscriptions.ObligationOrderReference(a, period.Add(time.Microsecond)))
	assert.NotEqual(t, subscriptions.ObligationOrderReference(a, period), subscriptions.ObligationOrderReference(b, period))

	key := InvoiceCollectionRetryKey(a, "client")
	assert.True(t, InvoiceCollectionRetryKeyValid(a, key))
	assert.Equal(t, key, InvoiceCollectionRetryKey(a, "client"))
	assert.NotEqual(t, key, InvoiceCollectionRetryKey(a, "other"))
	for _, bad := range []struct {
		invoice uuid.UUID
		key     string
	}{{b, key}, {uuid.Nil, key}, {a, key[:len(key)-32] + strings.ToUpper(key[len(key)-32:])}, {a, key[:len(key)-2]}, {a, "client"}} {
		assert.False(t, InvoiceCollectionRetryKeyValid(bad.invoice, bad.key), bad.key)
	}

	for rail, want := range map[models.Rail]string{models.RailStripe: TypeStripeRefund, models.RailNMI: TypeNMIRefund} {
		typ, provider, k, err := RefundIntentFor(&models.Payment{ID: a, Rail: rail}, "key")
		require.NoError(t, err)
		assert.Equal(t, []string{want, string(rail), RefundIdempotencyKey(a, "key")}, []string{typ, provider, k})
	}
	_, _, _, err := RefundIntentFor(&models.Payment{ID: a, Rail: models.RailCCBill}, "key")
	assert.ErrorIs(t, err, ccbill.ErrRefundUnsupported)
	_, _, _, err = RefundIntentFor(nil, "key")
	assert.Error(t, err)
}

func TestMutationEvidenceIsScrubbedAndPrunedToPointers(t *testing.T) {
	body, err := json.Marshal(scrubMutationEvidence(map[string]any{
		"remote_id":      "sub_123",
		"security_key":   "raw-secret",
		"message":        "failed security_key=raw-secret card_number=4111111111111111",
		"nested":         map[string]any{"authorization": "Bearer token", "status": "failed"},
		"list":           []any{"token=abc123", "ok"},
		"privateKeyFile": "/tmp/key.pem",
	}))
	require.NoError(t, err)
	for _, leaked := range []string{"raw-secret", "4111111111111111", "Bearer token", "abc123", "/tmp/key.pem"} {
		assert.NotContains(t, string(body), leaked)
	}
	for _, kept := range []string{"sub_123", "failed", "ok"} {
		assert.Contains(t, string(body), kept)
	}

	assert.Equal(t, map[string]any{"transaction_id": "t", "response_code": 100},
		slimEvidence(map[string]any{"transaction_id": "t", "response_code": 100, "raw": "x"}))
	assert.Nil(t, slimEvidence(map[string]any{"raw": "x"}), "no pointer keys: NULL, not {}")
	assert.Nil(t, slimEvidence(nil))
}

func TestRegistryRejectsDuplicateAndNil(t *testing.T) {
	h := &fakeHandler{typ: "a"}
	reg := NewRegistry(h)
	assert.Same(t, h, reg.Lookup("a").(*fakeHandler))
	assert.Nil(t, reg.Lookup("missing"))
	assert.Nil(t, (*Registry)(nil).Lookup("a"))
	assert.ElementsMatch(t, []string{"a"}, reg.Types())
	assert.PanicsWithValue(t, "intents: duplicate handler for intent type a", func() { reg.Register(&fakeHandler{typ: "a"}) })
	assert.Panics(t, func() { reg.Register(nil) })
}

func TestEnqueueRequiresMatchingMerchantBeforePersistence(t *testing.T) {
	store := NewStore(nil) // any persistence attempt would panic
	a, b := merchant.ID(uuid.New()), uuid.New()
	p := EnqueueParams{MerchantID: b, IntentType: TypeNMIDeleteSubscription}
	_, err := store.Enqueue(context.Background(), p)
	assert.ErrorIs(t, err, merchant.ErrNoMerchant)
	_, err = store.Enqueue(merchant.WithID(context.Background(), a), p)
	assert.ErrorContains(t, err, "merchant does not match context")
}
