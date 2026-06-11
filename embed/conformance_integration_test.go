//go:build integration

// Dual-transport conformance test (#338) — the point of the unified SDK.
//
// ONE embedded Runtime is built over the migrated dbtest Postgres (+ a Redis
// testcontainer for admission throughput). Its service-route surface is served
// via httptest with the REAL ServiceTokenRequired middleware and a permissive
// stub resolver (the standalone /v1/service/* routes authenticate with service
// tokens resolved by the control plane, NOT billingauth.Authenticator — so the
// stub-resolver pattern from tests/service_facade_parity_test.go is used).
//
// The SAME operation script then runs through BOTH clients — Runtime.Client()
// (embedded) and openrails.NewRemote (HTTP) — on separate tenant subjects, and
// the observable results must be EQUAL after normalizing ids/timestamps. Every
// divergence is an adapter bug: fix the adapter, never the assertion.
package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"sort"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	authcore "github.com/open-rails/authkit/core"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/embed"
	"github.com/open-rails/openrails/internal/controlplane"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/dbtest"
	ginmw "github.com/open-rails/openrails/internal/http/middleware/ginmw"
	ginroutes "github.com/open-rails/openrails/internal/http/routes/ginroutes"
	"github.com/open-rails/openrails/pkg/embedded"
	"github.com/open-rails/openrails/pkg/tenant"
)

const conformanceToken = "openrails_st_conformance_token"

// stubResolver accepts exactly conformanceToken as a tenant-wide PermAdmin
// service token for the default tenant (the same fixed-principal pattern the
// parity test uses), so the httptest server runs the REAL service-token
// middleware without a full AuthKit control plane.
type stubResolver struct{}

func (stubResolver) LooksLikeServiceToken(token string) bool { return token == conformanceToken }

func (stubResolver) ResolveServiceToken(_ context.Context, token string) (*controlplane.ResolvedServiceToken, error) {
	if token != conformanceToken {
		return nil, authcore.ErrInvalidAccessToken
	}
	return &controlplane.ResolvedServiceToken{
		AuthKitTenantSlug: "operator",
		TenantID:          tenant.DefaultID,
		TenantSlug:        tenant.DefaultSlug,
		Permissions:       []string{controlplane.PermAdmin},
		Resources:         []authcore.ServiceTokenResource{controlplane.TenantResource(tenant.DefaultID)},
	}, nil
}

// --- normalized observations ------------------------------------------------

type obsErr struct {
	Status       int
	Invalid      bool
	Unauthorized bool
	Denied       bool
	NotFound     bool
	Conflict     bool
	Internal     bool
	Insufficient bool
	Inactive     bool
}

func observeErr(t *testing.T, label string, err error) obsErr {
	t.Helper()
	require.Error(t, err, label)
	o := obsErr{
		Invalid:      errors.Is(err, openrails.ErrInvalid),
		Unauthorized: errors.Is(err, openrails.ErrUnauthorized),
		Denied:       errors.Is(err, openrails.ErrDenied),
		NotFound:     errors.Is(err, openrails.ErrNotFound),
		Conflict:     errors.Is(err, openrails.ErrConflict),
		Internal:     errors.Is(err, openrails.ErrInternal),
		Insufficient: errors.Is(err, openrails.ErrInsufficientCredits),
		Inactive:     errors.Is(err, openrails.ErrCreditTypeInactive),
	}
	var se *openrails.StatusError
	require.ErrorAs(t, err, &se, "%s: both transports must return *openrails.StatusError, got %v", label, err)
	o.Status = se.Status
	return o
}

type obsTxn struct {
	Amount          int64
	TransactionType string
	Status          string
	Source          string
	HasBalanceAfter bool
	BalanceAfter    int64
	HasCaptured     bool
	Captured        int64
	Actor           string
}

func observeTxn(t *testing.T, label string, txn *openrails.CreditTransaction) obsTxn {
	t.Helper()
	require.NotNil(t, txn, label)
	require.NotEqual(t, uuid.Nil, txn.ID, "%s: txn id", label)
	require.NotEqual(t, uuid.Nil, txn.TenantSubjectID, "%s: txn tenant_subject_id", label)
	require.False(t, txn.CreatedAt.IsZero(), "%s: created_at", label)
	o := obsTxn{
		Amount:          txn.Amount,
		TransactionType: txn.TransactionType,
		Status:          txn.Status,
		Source:          txn.Source,
		Actor:           txn.Actor,
	}
	if txn.BalanceAfter != nil {
		o.HasBalanceAfter, o.BalanceAfter = true, *txn.BalanceAfter
	}
	if txn.Captured != nil {
		o.HasCaptured, o.Captured = true, *txn.Captured
	}
	return o
}

type obsBudget struct {
	Key        string
	Limit      int64
	Used       int64
	Reserved   int64
	Remaining  int64
	HasReset   bool
	HasResetAt bool
	Cadence    string
	Allowed    bool
}

func observeBudgets(ws []openrails.BudgetWindow) []obsBudget {
	out := make([]obsBudget, 0, len(ws))
	for _, w := range ws {
		out = append(out, obsBudget{
			Key: w.Key, Limit: w.Limit, Used: w.Used, Reserved: w.Reserved,
			Remaining: w.Remaining, HasReset: w.ResetAfterSeconds > 0,
			HasResetAt: !w.ResetAt.IsZero(), Cadence: w.Cadence, Allowed: w.Allowed,
		})
	}
	return out
}

type obsWindow struct {
	Unit      string
	Limit     int64
	Remaining int64
}

type obsAdmit struct {
	Allowed              bool
	BlockedBy            string
	BlockedUnit          string
	DenyCode             string
	HasRetry             bool
	HasReservation       bool
	HasBudgetReservation bool
	Windows              []obsWindow
	Budget               []obsBudget
}

func observeAdmit(res *openrails.AdmitResponse) obsAdmit {
	o := obsAdmit{
		Allowed:              res.Allowed,
		BlockedBy:            res.BlockedBy,
		BlockedUnit:          res.BlockedUnit,
		DenyCode:             res.DenyCode,
		HasRetry:             res.RetryAfterSeconds > 0,
		HasReservation:       res.ReservationID != "",
		HasBudgetReservation: res.BudgetReservationID != "",
		Budget:               observeBudgets(res.BudgetWindows),
	}
	for _, w := range res.Windows {
		o.Windows = append(o.Windows, obsWindow{Unit: w.Unit, Limit: w.Limit, Remaining: w.Remaining})
	}
	return o
}

type obsAccount struct {
	CreditType            string
	BillingMode           string
	BalanceMicros         int64
	HeldMicros            int64
	AvailableMicros       int64
	OutstandingOwedMicros int64
	PayerMatches          bool
}

// scriptResult is everything one transport observed. The two transports'
// results must be require.Equal.
type scriptResult struct {
	Deposit  obsTxn
	Balance  openrails.BalanceResponse
	Account  obsAccount
	HoldOK   bool
	Capture  obsTxn
	Withdraw obsTxn

	Admit1 obsAdmit // allowed (hold + budget reservation)
	Admit2 obsAdmit // denied: budget exhausted
	Admit3 obsAdmit // denied: throughput window

	BudgetCheck  []obsBudget
	BudgetStatus []obsBudget

	SettingsAccount obsAccount

	Usage []openrails.UsageRollupRow

	Txns      []obsTxn
	TxnsTotal int

	RevenueTotal int64
	RevenueDays  []int64

	ErrReleaseUnknown   obsErr
	ErrReleaseGarbage   obsErr
	ErrHoldInsufficient obsErr
	ErrWithdrawNoSource obsErr
	ErrDepositBadType   obsErr
	ErrBalanceBadID     obsErr
	ErrRollupBadGroup   obsErr
	ErrAdmitNegative    obsErr

	// Prepaid credit windows + batch admission (#335).
	WindowOpen   obsWindowState
	WindowSettle []obsSettle
	WindowRefill obsWindowState
	WindowClose  obsWindowState
	SettleClosed []obsSettle

	BatchVerdicts []obsBatchVerdict

	ErrWindowOpenInsufficient obsErr
	ErrWindowOpenNoAmount     obsErr
	ErrRefillNoArgs           obsErr
	ErrRefillUnknown          obsErr
	ErrCloseUnknown           obsErr
	ErrSettleEmpty            obsErr
	ErrBatchEmpty             obsErr

	// Entitlements by external (issuer, subject) identity.
	Entitlements            []obsEntitlement
	EntitlementsUnknown     int
	ErrEntitlementsNoIssuer obsErr

	// Batch entitlements (#354).
	BatchEntitlements            []obsEntitlement
	BatchUnknownEmpty            bool
	BatchKeyCount                int
	ErrBatchEntitlementsNoIssuer obsErr
	ErrBatchEntitlementsEmpty    obsErr
	ErrBatchEntitlementsOverCap  obsErr
}

type obsEntitlement struct {
	Entitlement  string
	SourceType   string
	HasEnd       bool
	PayerMatches bool
}

func observeEntitlements(ents []openrails.EntitlementRecord, payer uuid.UUID) []obsEntitlement {
	out := make([]obsEntitlement, 0, len(ents))
	for _, e := range ents {
		out = append(out, obsEntitlement{
			Entitlement:  e.Entitlement,
			SourceType:   e.SourceType,
			HasEnd:       e.EndAt != nil,
			PayerMatches: e.TenantSubjectID == payer.String(),
		})
	}
	return out
}

type obsWindowState struct {
	Held         int64
	Settled      int64
	Status       string
	PayerMatches bool
	HasExpiry    bool
}

func observeWindow(t *testing.T, label string, w *openrails.CreditWindow, payer uuid.UUID) obsWindowState {
	t.Helper()
	require.NotNil(t, w, label)
	require.NotEqual(t, uuid.Nil, w.WindowID, "%s: window id", label)
	return obsWindowState{
		Held:         w.HeldMicros,
		Settled:      w.SettledMicros,
		Status:       w.Status,
		PayerMatches: w.PayerTenantID == payer,
		HasExpiry:    !w.ExpiresAt.IsZero(),
	}
}

type obsSettle struct {
	OK            bool
	Replayed      bool
	Error         string
	HasTxn        bool
	WindowMatches bool
}

func observeSettles(results []openrails.WindowSettleResult, windowID uuid.UUID) []obsSettle {
	out := make([]obsSettle, 0, len(results))
	for _, r := range results {
		out = append(out, obsSettle{
			OK:            r.OK,
			Replayed:      r.Replayed,
			Error:         r.Error,
			HasTxn:        r.TransactionID != nil,
			WindowMatches: r.WindowID == windowID,
		})
	}
	return out
}

type obsBatchVerdict struct {
	Status    int
	Error     string
	HasResult bool
	Result    obsAdmit
}

func observeBatchVerdicts(verdicts []openrails.AdmitBatchVerdict) []obsBatchVerdict {
	out := make([]obsBatchVerdict, 0, len(verdicts))
	for _, v := range verdicts {
		o := obsBatchVerdict{Status: v.Status, Error: v.Error, HasResult: v.Result != nil}
		if v.Result != nil {
			o.Result = observeAdmit(v.Result)
		}
		out = append(out, o)
	}
	return out
}

type scriptEnv struct {
	payer      uuid.UUID
	actor      string
	creditType string
	resource   string // per-side: ResourceRevenueDaily is cross-payer within the tenant
	side       string
	from, to   time.Time
	// issuer/subject are the payer's EXTERNAL identity (its
	// billing.tenant_subjects row) for the entitlements steps.
	issuer  string
	subject string
}

// runScript executes the identical operation sequence through one client and
// returns the normalized observations.
func runScript(t *testing.T, ctx context.Context, c openrails.Client, env scriptEnv) scriptResult {
	t.Helper()
	var r scriptResult
	payerID := env.payer.String()
	pid := openrails.PayerTenantID(env.payer)

	// 1) Deposit 1,000,000 micros.
	depositSrc := uuid.New()
	dep, err := c.DepositCredits(ctx, openrails.DepositCreditsRequest{
		PayerTenantID: &pid,
		Actor:         env.actor,
		CreditType:    env.creditType,
		Amount:        1_000_000,
		Source:        "conformance",
		SourceID:      &depositSrc,
		Description:   "seed",
	})
	require.NoError(t, err, "%s deposit", env.side)
	r.Deposit = observeTxn(t, env.side+" deposit", dep)

	// 2) Hold 10,000 then capture 8,000.
	hold, err := c.HoldCredits(ctx, openrails.HoldCreditsRequest{
		PayerTenantID: &pid,
		Actor:         env.actor,
		CreditType:    env.creditType,
		Amount:        10_000,
		Source:        "conformance",
		SourceID:      "hold-1",
		ExpiresAt:     time.Now().Add(10 * time.Minute),
	})
	require.NoError(t, err, "%s hold", env.side)
	require.NotEqual(t, uuid.Nil, hold.ID, "%s hold id", env.side)
	require.Equal(t, int64(10_000), hold.Amount, "%s hold amount", env.side)
	r.HoldOK = hold.Status == "active"

	cap1, err := c.CaptureHold(ctx, openrails.CaptureHoldRequest{HoldID: hold.ID, Amount: 8_000})
	require.NoError(t, err, "%s capture-hold", env.side)
	r.Capture = observeTxn(t, env.side+" capture", cap1)

	// 3) Withdraw 5,000.
	withdrawSrc := uuid.New()
	wd, err := c.WithdrawCredits(ctx, openrails.WithdrawCreditsRequest{
		PayerTenantID: &pid,
		Actor:         env.actor,
		CreditType:    env.creditType,
		Amount:        5_000,
		Source:        "conformance",
		SourceID:      &withdrawSrc,
	})
	require.NoError(t, err, "%s withdraw", env.side)
	r.Withdraw = observeTxn(t, env.side+" withdraw", wd)

	// 4) Tier policy: 2 requests/60s throughput + a 10,000-micro fixed
	// hourly money window (#337 cadence on the wire).
	require.NoError(t, c.SetTierPolicy(ctx, payerID, openrails.TierPolicyInput{
		Tier:              "conf",
		Windows:           []openrails.TierThroughputWindow{{Unit: "request", WindowSeconds: 60, Max: 2}},
		EntitledResources: []string{},
		BudgetWindows: []openrails.BudgetWindowInput{
			{Key: "hourly", WindowSeconds: 3600, LimitMicros: 10_000, Cadence: "fixed"},
		},
	}), "%s set-tier-policy", env.side)

	// 5) Admit #1: estimate 5,000 micros -> allowed (budgets and ledger are
	// BOTH micro-dollars, 1:1 — the old /10 was the pre-#337 millicent
	// conversion, fixed as a 10x under-reservation bug).
	a1, err := c.Admit(ctx, openrails.AdmitRequest{
		PayerTenantID:  payerID,
		Actor:          env.actor,
		Tier:           "conf",
		Resource:       env.resource,
		Amounts:        map[string]int64{"request": 1},
		CreditType:     env.creditType,
		EstimateMicros: 5_000,
		RequestID:      env.side + "-admit-1",
		Source:         "admit",
	})
	require.NoError(t, err, "%s admit#1", env.side)
	r.Admit1 = observeAdmit(a1)
	require.True(t, a1.Allowed, "%s admit#1 must be allowed", env.side)
	require.NotEmpty(t, a1.ReservationID, "%s admit#1 reservation", env.side)

	// 6) Settle the admission hold at the full estimate, recording usage (#311).
	require.NoError(t, c.Capture(ctx, a1.ReservationID, 5_000, &openrails.CaptureUsage{
		EventType: "invoke",
		Resource:  env.resource,
	}), "%s capture admission hold", env.side)

	// 7) Admit #2: estimate 6,000 (5,000 already consumed of the 10,000
	// limit) -> denied on the budget axis.
	a2, err := c.Admit(ctx, openrails.AdmitRequest{
		PayerTenantID:  payerID,
		Actor:          env.actor,
		Tier:           "conf",
		Resource:       env.resource,
		Amounts:        map[string]int64{"request": 1},
		CreditType:     env.creditType,
		EstimateMicros: 6_000,
		RequestID:      env.side + "-admit-2",
		Source:         "admit",
	})
	require.NoError(t, err, "%s admit#2 (deny must be a verdict, not an error)", env.side)
	r.Admit2 = observeAdmit(a2)
	require.False(t, a2.Allowed, "%s admit#2 must be denied", env.side)
	require.Equal(t, "budget", a2.BlockedBy, "%s admit#2 blocked_by", env.side)

	// 8) Admit #3: small estimate, but the 2-requests/60s throughput window is
	// already consumed by #1+#2 -> denied on the throughput axis (HTTP 429 on
	// the remote transport; still a verdict).
	a3, err := c.Admit(ctx, openrails.AdmitRequest{
		PayerTenantID:  payerID,
		Actor:          env.actor,
		Tier:           "conf",
		Resource:       env.resource,
		Amounts:        map[string]int64{"request": 1},
		CreditType:     env.creditType,
		EstimateMicros: 100,
		RequestID:      env.side + "-admit-3",
		Source:         "admit",
	})
	require.NoError(t, err, "%s admit#3 (deny must be a verdict, not an error)", env.side)
	r.Admit3 = observeAdmit(a3)
	require.Equal(t, "throughput", a3.BlockedBy, "%s admit#3 blocked_by", env.side)

	// 9) Budget check against caller-supplied windows: fixed + session cadence
	// round-trip (#337).
	bw, err := c.BudgetCheck(ctx, payerID, env.actor, []openrails.BudgetWindowInput{
		{Key: "hourly", WindowSeconds: 3600, LimitMicros: 10_000, Cadence: "fixed"},
		{Key: "sess", WindowSeconds: 600, LimitMicros: 2_000, Cadence: "session"},
	}, 0)
	require.NoError(t, err, "%s budget-check", env.side)
	r.BudgetCheck = observeBudgets(bw)
	require.Len(t, bw, 2, "%s budget-check windows", env.side)
	require.Equal(t, "fixed", bw[0].Cadence, "%s budget-check cadence round-trip (fixed)", env.side)
	require.Equal(t, "session", bw[1].Cadence, "%s budget-check cadence round-trip (session)", env.side)
	require.False(t, bw[0].ResetAt.IsZero(), "%s budget-check fixed window reset_at must round-trip", env.side)

	// 10) Budget status from the STORED tier policy.
	bs, err := c.BudgetStatus(ctx, payerID, env.actor, "conf")
	require.NoError(t, err, "%s budget-status", env.side)
	r.BudgetStatus = observeBudgets(bs)
	require.Len(t, bs, 1, "%s budget-status windows", env.side)
	require.Equal(t, "fixed", bs[0].Cadence, "%s budget-status cadence", env.side)

	// 11) Balance + account snapshot (after all money movement).
	bal, err := c.Balance(ctx, payerID)
	require.NoError(t, err, "%s balance", env.side)
	r.Balance = *bal

	acct, err := c.GetCreditAccount(ctx, payerID, env.creditType)
	require.NoError(t, err, "%s get-credit-account", env.side)
	r.Account = observeAccount(acct, payerID)

	// 12) Account settings set + read-back.
	mode := "prepaid"
	maxDay := int64(100_000_000)
	alert := 50
	setAcct, err := c.SetCreditAccountSettings(ctx, payerID, env.creditType, openrails.AccountSettingsInput{
		BillingMode:          &mode,
		MaxSpendPerDayMicros: &maxDay,
		AlertThresholdPct:    &alert,
	})
	require.NoError(t, err, "%s set-account-settings", env.side)
	r.SettingsAccount = observeAccount(setAcct, payerID)

	// 13) Usage rollup by resource (the #311 usage event recorded by Capture).
	rows, err := c.UsageRollup(ctx, payerID, env.from, env.to, "resource")
	require.NoError(t, err, "%s usage-rollup", env.side)
	for i := range rows {
		if rows[i].Key == env.resource {
			rows[i].Key = "RES" // resource strings are per-side; normalize
		}
	}
	r.Usage = rows

	// 14) Transaction history passthrough JSON.
	rawTxns, err := c.ListCreditTransactions(ctx, payerID, env.creditType, 50)
	require.NoError(t, err, "%s list-transactions", env.side)
	var decoded struct {
		Transactions []struct {
			ID              uuid.UUID `json:"id"`
			TenantSubjectID uuid.UUID `json:"tenant_subject_id"`
			Actor           string    `json:"actor"`
			Amount          int64     `json:"amount"`
			TransactionType string    `json:"transaction_type"`
			Status          string    `json:"status"`
			Source          string    `json:"source"`
			CreatedAt       time.Time `json:"created_at"`
		} `json:"transactions"`
		Total int `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rawTxns, &decoded), "%s decode transactions", env.side)
	r.TxnsTotal = decoded.Total
	for _, txn := range decoded.Transactions {
		require.NotEqual(t, uuid.Nil, txn.ID, "%s txn id", env.side)
		require.Equal(t, env.payer, txn.TenantSubjectID, "%s txn payer", env.side)
		require.False(t, txn.CreatedAt.IsZero(), "%s txn created_at", env.side)
		r.Txns = append(r.Txns, obsTxn{
			Amount:          txn.Amount,
			TransactionType: txn.TransactionType,
			Status:          txn.Status,
			Source:          txn.Source,
			Actor:           txn.Actor,
		})
	}
	sort.Slice(r.Txns, func(i, j int) bool {
		a, b := r.Txns[i], r.Txns[j]
		if a.TransactionType != b.TransactionType {
			return a.TransactionType < b.TransactionType
		}
		if a.Amount != b.Amount {
			return a.Amount < b.Amount
		}
		return a.Status < b.Status
	})

	// 15) Per-resource daily revenue (#410).
	rev, err := c.ResourceRevenueDaily(ctx, env.resource, env.from.Unix(), env.to.Unix())
	require.NoError(t, err, "%s resource-revenue", env.side)
	r.RevenueTotal = rev.RevenueMicros
	for _, d := range rev.Daily {
		require.NotEmpty(t, d.Date, "%s revenue date", env.side)
		r.RevenueDays = append(r.RevenueDays, d.AmountMicros)
	}

	// 16) Error cases — same sentinel via errors.Is on both transports.
	r.ErrReleaseUnknown = observeErr(t, env.side+" release unknown hold",
		c.Release(ctx, uuid.NewString()))
	r.ErrReleaseGarbage = observeErr(t, env.side+" release garbage id",
		c.Release(ctx, "not-a-uuid"))

	_, err = c.HoldCredits(ctx, openrails.HoldCreditsRequest{
		PayerTenantID: &pid,
		Actor:         env.actor,
		CreditType:    env.creditType,
		Amount:        10_000_000_000,
		Source:        "conformance",
		SourceID:      "hold-too-big",
		ExpiresAt:     time.Now().Add(10 * time.Minute),
	})
	r.ErrHoldInsufficient = observeErr(t, env.side+" hold insufficient", err)

	_, err = c.WithdrawCredits(ctx, openrails.WithdrawCreditsRequest{
		PayerTenantID: &pid,
		Actor:         env.actor,
		CreditType:    env.creditType,
		Amount:        1,
		Source:        "conformance",
		SourceID:      nil, // binding-required on the wire
	})
	r.ErrWithdrawNoSource = observeErr(t, env.side+" withdraw without source_id", err)

	badSrc := uuid.New()
	_, err = c.DepositCredits(ctx, openrails.DepositCreditsRequest{
		PayerTenantID: &pid,
		Actor:         env.actor,
		CreditType:    "conformance_missing_credit_type",
		Amount:        1,
		Source:        "conformance",
		SourceID:      &badSrc,
	})
	r.ErrDepositBadType = observeErr(t, env.side+" deposit unknown credit type", err)

	_, err = c.Balance(ctx, "not-a-uuid")
	r.ErrBalanceBadID = observeErr(t, env.side+" balance bad payer id", err)

	_, err = c.UsageRollup(ctx, payerID, env.from, env.to, "bogus")
	r.ErrRollupBadGroup = observeErr(t, env.side+" rollup bad group_by", err)

	_, err = c.Admit(ctx, openrails.AdmitRequest{
		PayerTenantID:  payerID,
		Actor:          env.actor,
		CreditType:     env.creditType,
		EstimateMicros: -1,
		RequestID:      env.side + "-admit-neg",
	})
	r.ErrAdmitNegative = observeErr(t, env.side+" admit negative estimate", err)

	// 17) Prepaid credit windows (#335): open -> settle (ok + idempotent replay
	// + over-settle + nil window + unknown window) -> refill -> close ->
	// settle-after-close.
	win, err := c.OpenWindow(ctx, openrails.OpenWindowRequest{
		PayerTenantID: payerID,
		Actor:         env.actor,
		CreditType:    env.creditType,
		AmountMicros:  50_000,
		TTLSeconds:    600,
	})
	require.NoError(t, err, "%s open-window", env.side)
	r.WindowOpen = observeWindow(t, env.side+" open-window", win, env.payer)
	require.Equal(t, "open", win.Status, "%s opened window status", env.side)

	unknownWindow := uuid.New()
	settled, err := c.SettleWindowItems(ctx, []openrails.WindowSettleItem{
		{WindowID: win.WindowID, RequestID: env.side + "-settle-1", AmountMicros: 20_000, Actor: env.actor,
			Usage: &openrails.WindowSettleUsage{EventType: "invoke", Resource: env.resource}},
		{WindowID: win.WindowID, RequestID: env.side + "-settle-1", AmountMicros: 20_000},      // replay
		{WindowID: win.WindowID, RequestID: env.side + "-settle-2", AmountMicros: 999_999_999}, // over-settle
		{RequestID: env.side + "-settle-3", AmountMicros: 5},                                   // nil window -> invalid_item
		{WindowID: unknownWindow, RequestID: env.side + "-settle-4", AmountMicros: 5},          // unknown window
	})
	require.NoError(t, err, "%s settle (per-item failures must not fail the flush)", env.side)
	require.Len(t, settled, 5, "%s settle results are positional", env.side)
	r.WindowSettle = observeSettles(settled, win.WindowID)
	require.True(t, settled[0].OK && !settled[0].Replayed, "%s settle item 0", env.side)
	require.True(t, settled[1].OK && settled[1].Replayed, "%s settle item 1 must be an idempotent replay", env.side)

	rw, err := c.RefillWindow(ctx, win.WindowID, 10_000, 600)
	require.NoError(t, err, "%s refill-window", env.side)
	r.WindowRefill = observeWindow(t, env.side+" refill-window", rw, env.payer)

	cw, err := c.CloseWindow(ctx, win.WindowID)
	require.NoError(t, err, "%s close-window", env.side)
	r.WindowClose = observeWindow(t, env.side+" close-window", cw, env.payer)
	require.Equal(t, "closed", cw.Status, "%s closed window status", env.side)

	afterClose, err := c.SettleWindowItems(ctx, []openrails.WindowSettleItem{
		{WindowID: win.WindowID, RequestID: env.side + "-settle-5", AmountMicros: 1},
	})
	require.NoError(t, err, "%s settle after close", env.side)
	r.SettleClosed = observeSettles(afterClose, win.WindowID)

	// 18) Cross-payer batch admission (#335): allowed (client-side credit-type
	// default) + money-denied + bad payer + negative estimate — per-item
	// isolation, the batch call itself succeeds.
	verdicts, err := c.AdmitBatch(ctx, []openrails.AdmitRequest{
		{PayerTenantID: payerID, Actor: env.actor, EstimateMicros: 1_000, RequestID: env.side + "-batch-1", Source: "admit"},
		{PayerTenantID: payerID, Actor: env.actor, CreditType: env.creditType, EstimateMicros: 10_000_000_000, RequestID: env.side + "-batch-2"},
		{PayerTenantID: "not-a-uuid", Actor: env.actor, CreditType: env.creditType, EstimateMicros: 1, RequestID: env.side + "-batch-3"},
		{PayerTenantID: payerID, Actor: env.actor, CreditType: env.creditType, EstimateMicros: -1, RequestID: env.side + "-batch-4"},
	})
	require.NoError(t, err, "%s admit-batch", env.side)
	require.Len(t, verdicts, 4, "%s admit-batch verdicts are positional", env.side)
	r.BatchVerdicts = observeBatchVerdicts(verdicts)
	require.True(t, verdicts[0].Allowed(), "%s admit-batch item 0 must be admitted", env.side)
	require.NoError(t, c.Release(ctx, verdicts[0].Result.ReservationID), "%s release batch hold", env.side)

	// 19) Window/batch error parity.
	_, err = c.OpenWindow(ctx, openrails.OpenWindowRequest{
		PayerTenantID: payerID, Actor: env.actor, CreditType: env.creditType, AmountMicros: 10_000_000_000,
	})
	r.ErrWindowOpenInsufficient = observeErr(t, env.side+" open-window insufficient", err)
	_, err = c.OpenWindow(ctx, openrails.OpenWindowRequest{
		PayerTenantID: payerID, Actor: env.actor, CreditType: env.creditType,
	})
	r.ErrWindowOpenNoAmount = observeErr(t, env.side+" open-window without amount", err)
	_, err = c.RefillWindow(ctx, unknownWindow, 0, 0)
	r.ErrRefillNoArgs = observeErr(t, env.side+" refill without amount or ttl", err)
	_, err = c.RefillWindow(ctx, unknownWindow, 1_000, 60)
	r.ErrRefillUnknown = observeErr(t, env.side+" refill unknown window", err)
	_, err = c.CloseWindow(ctx, unknownWindow)
	r.ErrCloseUnknown = observeErr(t, env.side+" close unknown window", err)
	_, err = c.SettleWindowItems(ctx, nil)
	r.ErrSettleEmpty = observeErr(t, env.side+" settle empty batch", err)
	_, err = c.AdmitBatch(ctx, nil)
	r.ErrBatchEmpty = observeErr(t, env.side+" admit-batch empty", err)

	// 20) Entitlements by external (issuer, subject): the seeded active row, an
	// unknown subject (the wire's 200 [] — empty, never an error), and the
	// missing-issuer validation error.
	ents, err := c.ListActiveEntitlements(ctx, env.issuer, env.subject, time.Time{})
	require.NoError(t, err, "%s entitlements", env.side)
	r.Entitlements = observeEntitlements(ents, env.payer)
	ghost, err := c.ListActiveEntitlements(ctx, env.issuer, "ghost-"+env.subject, time.Time{})
	require.NoError(t, err, "%s entitlements unknown subject", env.side)
	r.EntitlementsUnknown = len(ghost)
	_, err = c.ListActiveEntitlements(ctx, "", env.subject, time.Time{})
	r.ErrEntitlementsNoIssuer = observeErr(t, env.side+" entitlements without issuer", err)

	// 21) Batch entitlements (#354): seeded subject + an unknown one in one
	// call (dupes deduped), an entry per requested subject, plus the
	// no-issuer / empty-subjects / over-cap validation errors.
	batch, err := c.ListActiveEntitlementsBatch(ctx, env.issuer,
		[]string{env.subject, " " + env.subject, "ghost-" + env.subject}, time.Time{})
	require.NoError(t, err, "%s entitlements batch", env.side)
	r.BatchEntitlements = observeEntitlements(batch[env.subject], env.payer)
	ghostRecs, ghostPresent := batch["ghost-"+env.subject]
	r.BatchUnknownEmpty = ghostPresent && len(ghostRecs) == 0
	r.BatchKeyCount = len(batch)
	_, err = c.ListActiveEntitlementsBatch(ctx, "", []string{env.subject}, time.Time{})
	r.ErrBatchEntitlementsNoIssuer = observeErr(t, env.side+" entitlements batch without issuer", err)
	_, err = c.ListActiveEntitlementsBatch(ctx, env.issuer, []string{" ", ""}, time.Time{})
	r.ErrBatchEntitlementsEmpty = observeErr(t, env.side+" entitlements batch empty subjects", err)
	overCap := make([]string, 501)
	for i := range overCap {
		overCap[i] = fmt.Sprintf("s-%d", i)
	}
	_, err = c.ListActiveEntitlementsBatch(ctx, env.issuer, overCap, time.Time{})
	r.ErrBatchEntitlementsOverCap = observeErr(t, env.side+" entitlements batch over cap", err)

	return r
}

func observeAccount(a *openrails.CreditAccount, payerID string) obsAccount {
	return obsAccount{
		CreditType:            a.CreditType,
		BillingMode:           a.BillingMode,
		BalanceMicros:         a.BalanceMicros,
		HeldMicros:            a.HeldMicros,
		AvailableMicros:       a.AvailableMicros,
		OutstandingOwedMicros: a.OutstandingOwedMicros,
		PayerMatches:          a.PayerTenantID == payerID,
	}
}

func TestConformance_EmbeddedAndRemoteAreObservablyIdentical(t *testing.T) {
	ctx := context.Background()
	dsn := dbtest.SharedPostgresDSN(t)
	appDB := dbtest.OpenAppDB(t, dsn)
	pool := appDB.Pool()

	// Redis (admission throughput axis).
	rc, err := tcredis.Run(ctx, "redis:7-alpine")
	require.NoError(t, err)
	t.Cleanup(func() { _ = rc.Terminate(ctx) })
	redisURL, err := rc.ConnectionString(ctx)
	require.NoError(t, err)
	ropt, err := redis.ParseURL(redisURL)
	require.NoError(t, err)
	rdb := redis.NewClient(ropt)
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(ctx).Err())

	// ONE embedded runtime; both transports run over this single engine.
	cfg := &config.Config{Env: "dev", DB: &config.DBConfig{URL: dsn}}
	rt, err := embed.New(ctx, embed.Options{Options: embedded.Options{Config: cfg, Redis: rdb}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = rt.Close(context.Background()) })

	// Serve the full standalone service-route surface over httptest with the
	// real service-token middleware + a fixed-principal stub resolver.
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ginmw.ResolveTenant())
	ginroutes.RegisterServiceRoutes(
		router.Group("/v1/service"),
		rt.Embedded().App().Runtime,
		ginmw.ServiceTokenRequired(stubResolver{}),
		nil,
	)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	// Shared fixtures: one credit type, two payers (state must not collide).
	creditType := "conf_credits_" + uuid.NewString()[:8]
	ctID := uuid.New()
	_, err = gen.New(pool).CreateCreditType(ctx, gen.CreateCreditTypeParams{
		ID: ctID, Name: creditType, DisplayName: "Conformance Credits",
		Unit: "USD", DecimalPlaces: 2, IsActive: true, CreatedAt: time.Now().UTC(),
	})
	require.NoError(t, err)

	// Both clients carry the same default credit type (Balance keys off it).
	local := rt.Client(embed.WithCreditType(creditType))
	remote := openrails.NewRemote(srv.URL,
		openrails.WithTokenProvider(func(context.Context) (string, error) { return conformanceToken, nil }),
		openrails.WithCreditType(creditType),
		openrails.WithTimeout(30*time.Second),
	)

	const issuer = "conformance"
	newPayer := func(side string) (uuid.UUID, string) {
		id := uuid.New()
		subject := side + "-" + id.String()
		_, err := pool.Exec(ctx, `
			INSERT INTO billing.tenant_subjects (id, tenant_id, issuer, subject, created_at, last_seen_at)
			VALUES ($1, $2, $3, $4, now(), now())`,
			id, tenant.DefaultID.UUID(), issuer, subject)
		require.NoError(t, err)
		// One active entitlement row for the entitlements steps.
		_, err = pool.Exec(ctx, `
			INSERT INTO billing.entitlements (id, tenant_id, tenant_subject_id, entitlement, start_at, source_id, source_type)
			VALUES ($1, $2, $3, 'premium', now() - interval '1 hour', $4, 'admin')`,
			uuid.New(), tenant.DefaultID.UUID(), id, uuid.New())
		require.NoError(t, err)
		return id, subject
	}
	payerLocal, subjectLocal := newPayer("local")
	payerRemote, subjectRemote := newPayer("remote")

	from := time.Now().Add(-time.Hour).UTC()
	to := time.Now().Add(time.Hour).UTC()

	resLocal := runScript(t, ctx, local, scriptEnv{
		payer: payerLocal, actor: "user:conf", creditType: creditType,
		resource: "ep-local", side: "local", from: from, to: to,
		issuer: issuer, subject: subjectLocal,
	})
	resRemote := runScript(t, ctx, remote, scriptEnv{
		payer: payerRemote, actor: "user:conf", creditType: creditType,
		resource: "ep-remote", side: "remote", from: from, to: to,
		issuer: issuer, subject: subjectRemote,
	})

	// THE assertion: identical observable behavior across transports.
	require.Equal(t, resLocal, resRemote,
		"embedded and remote transports diverged — fix the adapter, never the assertion")

	// Transport-specific contract: a remote 5xx is ALSO ErrUnreachable (the
	// fail-policy axis); an embedded engine fault never is (no wire to lose).
	remoteErr := remote.Release(ctx, uuid.NewString())
	require.Error(t, remoteErr)
	require.ErrorIs(t, remoteErr, openrails.ErrUnreachable)
	require.ErrorIs(t, remoteErr, openrails.ErrInternal)
	localErr := local.Release(ctx, uuid.NewString())
	require.Error(t, localErr)
	require.NotErrorIs(t, localErr, openrails.ErrUnreachable)
	require.ErrorIs(t, localErr, openrails.ErrInternal)

	// Remote-only: a bad bearer is 401 -> ErrUnauthorized.
	badRemote := openrails.NewRemote(srv.URL,
		openrails.WithTokenProvider(func(context.Context) (string, error) { return "wrong-token", nil }),
	)
	_, err = badRemote.Balance(ctx, payerRemote.String())
	require.ErrorIs(t, err, openrails.ErrUnauthorized)
}
