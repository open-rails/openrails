//go:build integration

// Dual-transport conformance test (#338/#485) — the point of the unified SDK,
// now run against TWO REAL servers over REAL HTTP (no stubs).
//
// The reusable integrationharness boots both surfaces over the same migrated
// dbtest Postgres:
//
//   - Server 1, EMBEDDED no-auth HOST (≈ doujins minus auth): embed.New +
//     the real embedded /v1/service/* surface on httptest, behind the REAL
//     ServiceCredentialRequired middleware wired to a TRUSTING resolver (no auth).
//   - Server 2, STANDALONE real server + real AuthKit: the actual standalone gin
//     server with the control plane attached, authenticated by a REAL minted
//     API key resolved through AuthKit core (#481 role-based authz).
//
// The SAME operation script then runs through BOTH clients (openrails.NewRemote
// against each surface) on separate payer subjects, and the observable results
// must be EQUAL after normalizing ids/timestamps. Every divergence is an adapter
// bug: fix the adapter, never the assertion.
package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/dbtest"
	"github.com/open-rails/openrails/internal/integrationharness"
	"github.com/open-rails/openrails/internal/modules/money"
	"github.com/open-rails/openrails/internal/modules/productaccess"
)

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
	Invoker         string
}

func observeTxn(t *testing.T, label string, txn *openrails.CreditTransaction) obsTxn {
	t.Helper()
	require.NotNil(t, txn, label)
	require.NotEqual(t, uuid.Nil, txn.ID, "%s: txn id", label)
	require.NotEqual(t, uuid.Nil, txn.CustomerID, "%s: txn customer_id", label)
	require.False(t, txn.CreatedAt.IsZero(), "%s: created_at", label)
	o := obsTxn{
		Amount:          txn.Amount,
		TransactionType: txn.TransactionType,
		Status:          txn.Status,
		Source:          txn.Source,
		Invoker:         txn.Invoker,
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
	Allowed    bool
}

func observeBudgets(ws []openrails.BudgetWindow) []obsBudget {
	out := make([]obsBudget, 0, len(ws))
	for _, w := range ws {
		out = append(out, obsBudget{
			Key: w.Key, Limit: w.Limit, Used: w.Used, Reserved: w.Reserved,
			Remaining: w.Remaining, HasReset: w.ResetAfterSeconds > 0,
			HasResetAt: !w.ResetAt.IsZero(), Allowed: w.Allowed,
		})
	}
	return out
}

type obsWindow struct {
	Unit      string
	Limit     int64
	Remaining int64
}

type obsBudgetPolicyWindow struct {
	Key           string
	WindowSeconds int64
	Limit         int64
}

type obsBudgetPolicy struct {
	Scope   string
	RoleID  string
	Windows []obsBudgetPolicyWindow
}

type obsMerchantProfile struct {
	DisplayName string
	LogoURL     string
	FromEmail   string
	SupportURL  string
}

func observeBudgetPolicies(ps []openrails.InvokerSpendLimit) []obsBudgetPolicy {
	out := make([]obsBudgetPolicy, 0, len(ps))
	for _, p := range ps {
		o := obsBudgetPolicy{Scope: p.Scope, RoleID: p.RoleID}
		for _, w := range p.Windows {
			o.Windows = append(o.Windows, obsBudgetPolicyWindow{
				Key: w.Key, WindowSeconds: w.WindowSeconds, Limit: w.Limit,
			})
		}
		out = append(out, o)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Scope != out[j].Scope {
			return out[i].Scope < out[j].Scope
		}
		return out[i].RoleID < out[j].RoleID
	})
	return out
}

type obsAdmit struct {
	Allowed              bool
	BlockedBy            string
	DenyCode             string
	HasRetry             bool
	HasReservation       bool
	HasBudgetReservation bool
	Budget               []obsBudget
}

func observeAdmit(res *openrails.AdmitResponse) obsAdmit {
	o := obsAdmit{
		Allowed:              res.Allowed,
		BlockedBy:            res.BlockedBy,
		DenyCode:             res.DenyCode,
		HasRetry:             res.RetryAfterSeconds > 0,
		HasReservation:       res.HoldExpiresAt != nil,
		HasBudgetReservation: res.BudgetReservationID != "",
		Budget:               observeBudgets(res.BudgetWindows),
	}
	return o
}

type obsAccount struct {
	Currency              string
	BillingMode           string
	BalanceAmount         int64
	HeldAmount            int64
	AvailableAmount       int64
	OutstandingOwedAmount int64
	PayerMatches          bool
}

type obsProductAccess struct {
	ProductMatches bool
	CustomerIDSet  bool
	ProductSlugSet bool
	SourceType     string
	Status         string
	HasPaymentID   bool
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
	Admit3 obsAdmit // allowed: budget still has room
	Admit4 obsAdmit // denied: budget window exhausted

	BudgetStatus []obsBudget

	// Hierarchical budget-scope policies on the unified client (#473).
	InvokerSpendLimits []obsBudgetPolicy

	// Merchant-configured flat delegated-invoker wasted-spend windows: the
	// configured limit + over-budget state observed via AbuseUsage.
	InvokerWastedUsage []openrails.AbuseUsageWindow
	MerchantProfile    obsMerchantProfile

	SettingsAccount obsAccount

	Usage []openrails.UsageRollupRow

	Txns      []obsTxn
	TxnsTotal int

	RevenueTotal int64
	RevenueDays  []int64

	ErrWithdrawNoSource obsErr
	ErrDepositBadType   obsErr
	ErrBalanceBadID     obsErr
	ErrRollupBadGroup   obsErr
	ErrAdmitNegative    obsErr

	// Cross-payer batch admission (#335).
	BatchVerdicts []obsBatchVerdict
	ErrBatchEmpty obsErr

	// Entitlements by external (issuer, subjects) identity — always batch (#354).
	Entitlements              []obsEntitlement
	SingleEntitlements        []obsEntitlement
	HasEntitlement            bool
	HasMissingEntitlement     bool
	EntitlementsUnknownEmpty  bool
	EntitlementsKeyCount      int
	ErrEntitlementsNoSubjects obsErr
	ErrEntitlementsOverCap    obsErr

	ProductAccess         []obsProductAccess
	HasProductAccess      bool
	HasNoProductAccess    bool
	ErrProductAccessBadID obsErr
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
			PayerMatches: e.CustomerID == payer.String(),
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
	payer    uuid.UUID
	invoker  string
	currency string
	resource string // per-side: ResourceRevenueDaily is cross-payer within the tenant
	side     string
	from, to time.Time
	pool     *pgxpool.Pool
	// issuer/subject are the payer's EXTERNAL identity (its
	// openrails.customers row) for the entitlements steps.
	issuer  string
	subject string
	product uuid.UUID
}

// runScript executes the identical operation sequence through one client and
// returns the normalized observations.
func runScript(t *testing.T, ctx context.Context, c openrails.Client, env scriptEnv) scriptResult {
	t.Helper()
	var r scriptResult
	payerID := env.payer.String()
	pid := openrails.CustomerID(env.payer)

	// 1) Deposit 1,000,000 internal amount units.
	depositSrc := uuid.New()
	dep, err := c.DepositCredits(ctx, openrails.DepositCreditsRequest{
		CustomerID:  &pid,
		Invoker:     env.invoker,
		Currency:    env.currency,
		Amount:      1_000_000,
		Source:      "conformance",
		SourceID:    &depositSrc,
		Description: "seed",
	})
	require.NoError(t, err, "%s deposit", env.side)
	r.Deposit = observeTxn(t, env.side+" deposit", dep)

	// 2) Admit 10,000 (the request hold) then capture 8,000. Holds are Redis (#513);
	// a direct-payer admit is balance-gated, so no delegated budget grant is needed.
	ad1, err := c.Admit(ctx, openrails.AdmitRequest{
		CustomerID:      payerID,
		Invoker:         env.invoker,
		InvokerType:     "payer",
		Currency:        env.currency,
		EstimatedAmount: 10_000,
		Source:          "conformance",
		RequestID:       "hold-1",
	})
	require.NoError(t, err, "%s admit-hold", env.side)
	require.True(t, ad1.Allowed, "%s admit-hold allowed", env.side)
	r.HoldOK = ad1.Allowed

	cap1, err := c.CaptureHold(ctx, openrails.CaptureHoldRequest{RequestID: "hold-1", Amount: 8_000})
	require.NoError(t, err, "%s capture-hold", env.side)
	r.Capture = observeTxn(t, env.side+" capture", cap1)

	// 3) Withdraw 5,000.
	withdrawSrc := uuid.New()
	wd, err := c.WithdrawCredits(ctx, openrails.WithdrawCreditsRequest{
		CustomerID: &pid,
		Invoker:    env.invoker,
		Currency:   env.currency,
		Amount:     5_000,
		Source:     "conformance",
		SourceID:   &withdrawSrc,
	})
	require.NoError(t, err, "%s withdraw", env.side)
	r.Withdraw = observeTxn(t, env.side+" withdraw", wd)

	// 4) Explicit delegated-spend grant: a 10,000-unit fixed hourly money
	// window (#337 cadence on the wire). The tier policy below remains only as a
	// legacy BudgetStatus fixture; service admit reads the explicit role budget
	// policy.
	roleID := uuid.New().String()
	roleUUID, err := uuid.Parse(roleID)
	require.NoError(t, err, "%s parse role id", env.side)
	require.NoError(t, c.SetPayerSpendLimits(ctx, payerID, openrails.PayerSpendLimitInput{
		Tier: "conf",
		BudgetWindows: []openrails.BudgetWindowInput{
			{Key: "hourly", WindowSeconds: 3600, Limit: 10_000},
		},
	}), "%s set-tier-policy", env.side)
	require.NoError(t, c.SetInvokerSpendLimits(ctx, payerID, openrails.InvokerSpendLimitInput{
		Scope:  "role",
		RoleID: roleID,
		Windows: []openrails.SpendLimitWindow{
			{Key: "hourly", WindowSeconds: 3600, Limit: 10_000},
		},
	}), "%s set-subject-budget-policy (admit role)", env.side)

	// 5) Admit #1: estimated amount 5,000 -> allowed (budgets and ledger are
	// both in the currency's internal precision, 1:1 — the old /10 was the pre-#337 millicent
	// conversion, fixed as a 10x under-reservation bug).
	a1, err := c.Admit(ctx, openrails.AdmitRequest{
		CustomerID:      payerID,
		Invoker:         env.invoker,
		InvokerType:     openrails.InvokerTypeDelegated,
		Tier:            "conf",
		Resource:        env.resource,
		Currency:        env.currency,
		EstimatedAmount: 5_000,
		RequestID:       env.side + "-admit-1",
		Source:          "admit",
		Roles:           []uuid.UUID{roleUUID},
	})
	require.NoError(t, err, "%s admit#1", env.side)
	r.Admit1 = observeAdmit(a1)
	require.True(t, a1.Allowed, "%s admit#1 must be allowed", env.side)
	require.NotNil(t, a1.HoldExpiresAt, "%s admit#1 hold expiry", env.side)

	// 6) Settle the admission hold at the full estimate, recording usage (#311).
	require.NoError(t, c.Capture(ctx, env.side+"-admit-1", 5_000, &openrails.CaptureUsage{
		EventType: "invoke",
		Resource:  env.resource,
	}), "%s capture admission hold", env.side)

	// 7) Admit #2: estimate 6,000 (5,000 already consumed of the 10,000
	// limit) -> denied on the budget axis.
	a2, err := c.Admit(ctx, openrails.AdmitRequest{
		CustomerID:      payerID,
		Invoker:         env.invoker,
		InvokerType:     openrails.InvokerTypeDelegated,
		Tier:            "conf",
		Resource:        env.resource,
		Currency:        env.currency,
		EstimatedAmount: 6_000,
		RequestID:       env.side + "-admit-2",
		Source:          "admit",
		Roles:           []uuid.UUID{roleUUID},
	})
	require.NoError(t, err, "%s admit#2 (deny must be a verdict, not an error)", env.side)
	r.Admit2 = observeAdmit(a2)
	require.False(t, a2.Allowed, "%s admit#2 must be denied", env.side)
	require.Equal(t, "budget", a2.BlockedBy, "%s admit#2 blocked_by", env.side)

	// 8) Admit #3: estimate 5,000 fits the remaining hourly budget (5,000
	// already captured of 10,000).
	a3, err := c.Admit(ctx, openrails.AdmitRequest{
		CustomerID:      payerID,
		Invoker:         env.invoker,
		InvokerType:     openrails.InvokerTypeDelegated,
		Tier:            "conf",
		Resource:        env.resource,
		Currency:        env.currency,
		EstimatedAmount: 5_000,
		RequestID:       env.side + "-admit-3",
		Source:          "admit",
		Roles:           []uuid.UUID{roleUUID},
	})
	require.NoError(t, err, "%s admit#3", env.side)
	r.Admit3 = observeAdmit(a3)
	require.True(t, a3.Allowed, "%s admit#3 must be allowed", env.side)

	// 9) Admit #4: the in-flight 5,000 reservation plus captured 5,000 exhausts
	// the hourly budget, so even 1 more unit is denied on budget.
	a4, err := c.Admit(ctx, openrails.AdmitRequest{
		CustomerID:      payerID,
		Invoker:         env.invoker,
		InvokerType:     openrails.InvokerTypeDelegated,
		Tier:            "conf",
		Resource:        env.resource,
		Currency:        env.currency,
		EstimatedAmount: 1,
		RequestID:       env.side + "-admit-4",
		Source:          "admit",
		Roles:           []uuid.UUID{roleUUID},
	})
	require.NoError(t, err, "%s admit#4 (deny must be a verdict, not an error)", env.side)
	r.Admit4 = observeAdmit(a4)
	require.False(t, a4.Allowed, "%s admit#4 must be denied", env.side)
	require.Equal(t, "budget", a4.BlockedBy, "%s admit#4 blocked_by", env.side)

	// 10) Budget status from the STORED tier policy.
	bs, err := c.BudgetStatus(ctx, payerID, env.invoker, money.DefaultCurrency, "conf")
	require.NoError(t, err, "%s budget-status", env.side)
	r.BudgetStatus = observeBudgets(bs)
	require.Len(t, bs, 1, "%s budget-status windows", env.side)

	// 11) Balance + account snapshot (after all money movement).
	bal, err := c.Balance(ctx, payerID)
	require.NoError(t, err, "%s balance", env.side)
	r.Balance = *bal

	acct, err := c.GetCreditAccount(ctx, payerID, env.currency)
	require.NoError(t, err, "%s get-credit-account", env.side)
	r.Account = observeAccount(acct, payerID)

	// 12) Account settings set + read-back.
	mode := "prepaid"
	maxDay := int64(100_000_000)
	alert := 50
	setAcct, err := c.SetCreditAccountSettings(ctx, payerID, env.currency, openrails.AccountSettingsInput{
		BillingMode:       &mode,
		MaxSpendPerDay:    &maxDay,
		AlertThresholdPct: &alert,
	})
	require.NoError(t, err, "%s set-account-settings", env.side)
	r.SettingsAccount = observeAccount(setAcct, payerID)

	// 13) Usage rollup by resource (the #311 usage event recorded by Capture).
	rows, err := c.UsageRollup(ctx, payerID, money.DefaultCurrency, env.from, env.to, "resource")
	require.NoError(t, err, "%s usage-rollup", env.side)
	for i := range rows {
		if rows[i].Key == env.resource {
			rows[i].Key = "RES" // resource strings are per-side; normalize
		}
	}
	r.Usage = rows

	// 14) Transaction history passthrough JSON.
	rawTxns, err := c.ListCreditTransactions(ctx, payerID, env.currency, 50)
	require.NoError(t, err, "%s list-transactions", env.side)
	var decoded struct {
		Transactions []struct {
			ID              uuid.UUID `json:"id"`
			CustomerID      uuid.UUID `json:"customer_id"`
			Invoker         string    `json:"invoker"`
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
		require.Equal(t, env.payer, txn.CustomerID, "%s txn payer", env.side)
		require.False(t, txn.CreatedAt.IsZero(), "%s txn created_at", env.side)
		r.Txns = append(r.Txns, obsTxn{
			Amount:          txn.Amount,
			TransactionType: txn.TransactionType,
			Status:          txn.Status,
			Source:          txn.Source,
			Invoker:         txn.Invoker,
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
	rev, err := c.ResourceRevenueDaily(ctx, env.resource, money.DefaultCurrency, env.from.Unix(), env.to.Unix())
	require.NoError(t, err, "%s resource-revenue", env.side)
	r.RevenueTotal = rev.RevenueAmount
	for _, d := range rev.Daily {
		require.NotEmpty(t, d.Date, "%s revenue date", env.side)
		r.RevenueDays = append(r.RevenueDays, d.Amount)
	}

	// 16) Release is idempotent (#513): releasing an unknown / TTL-expired / garbage
	// request id is a no-op on BOTH transports (the hold reservation is gone, so there
	// is nothing to free — never an error).
	require.NoError(t, c.Release(ctx, uuid.NewString()), "%s release unknown hold is idempotent", env.side)
	require.NoError(t, c.Release(ctx, "not-a-uuid"), "%s release garbage id is idempotent", env.side)

	_, err = c.WithdrawCredits(ctx, openrails.WithdrawCreditsRequest{
		CustomerID: &pid,
		Invoker:    env.invoker,
		Currency:   env.currency,
		Amount:     1,
		Source:     "conformance",
		SourceID:   nil, // binding-required on the wire
	})
	r.ErrWithdrawNoSource = observeErr(t, env.side+" withdraw without source_id", err)

	badSrc := uuid.New()
	_, err = c.DepositCredits(ctx, openrails.DepositCreditsRequest{
		CustomerID: &pid,
		Invoker:    env.invoker,
		Currency:   "conformance_missing_currency",
		Amount:     1,
		Source:     "conformance",
		SourceID:   &badSrc,
	})
	r.ErrDepositBadType = observeErr(t, env.side+" deposit unknown credit type", err)

	_, err = c.Balance(ctx, "not-a-uuid")
	r.ErrBalanceBadID = observeErr(t, env.side+" balance bad payer id", err)

	_, err = c.UsageRollup(ctx, payerID, money.DefaultCurrency, env.from, env.to, "bogus")
	r.ErrRollupBadGroup = observeErr(t, env.side+" rollup bad group_by", err)

	_, err = c.Admit(ctx, openrails.AdmitRequest{
		CustomerID:      payerID,
		Invoker:         env.invoker,
		Currency:        env.currency,
		EstimatedAmount: -1,
		RequestID:       env.side + "-admit-neg",
	})
	r.ErrAdmitNegative = observeErr(t, env.side+" admit negative estimate", err)

	// 17) Cross-payer batch admission (#335): allowed + money-denied + bad payer + negative estimate — per-item
	// isolation, the batch call itself succeeds.
	verdicts, err := c.AdmitBatch(ctx, []openrails.AdmitRequest{
		{CustomerID: payerID, Invoker: env.invoker, InvokerType: openrails.InvokerTypePayer, Currency: env.currency, EstimatedAmount: 1_000, RequestID: env.side + "-batch-1", Source: "admit"},
		{CustomerID: payerID, Invoker: env.invoker, InvokerType: openrails.InvokerTypePayer, Currency: env.currency, EstimatedAmount: 10_000_000_000, RequestID: env.side + "-batch-2"},
		{CustomerID: "not-a-uuid", Invoker: env.invoker, InvokerType: openrails.InvokerTypePayer, Currency: env.currency, EstimatedAmount: 1, RequestID: env.side + "-batch-3"},
		{CustomerID: payerID, Invoker: env.invoker, InvokerType: openrails.InvokerTypePayer, Currency: env.currency, EstimatedAmount: -1, RequestID: env.side + "-batch-4"},
	})
	require.NoError(t, err, "%s admit-batch", env.side)
	require.Len(t, verdicts, 4, "%s admit-batch verdicts are positional", env.side)
	r.BatchVerdicts = observeBatchVerdicts(verdicts)
	require.True(t, verdicts[0].Allowed(), "%s admit-batch item 0 must be admitted", env.side)
	require.NoError(t, c.Release(ctx, env.side+"-batch-1"), "%s release batch hold", env.side)

	// 18) Admit-batch error parity.
	_, err = c.AdmitBatch(ctx, nil)
	r.ErrBatchEmpty = observeErr(t, env.side+" admit-batch empty", err)

	// 20) Entitlements (#354, always batch): seeded subject + an unknown one
	// in one call (dupes deduped), an entry per requested subject, plus the
	// empty-subjects / over-cap validation errors (#555: merchant from the
	// credential, no issuer).
	ents, err := c.ListActiveEntitlements(ctx,
		[]string{env.subject, " " + env.subject, "ghost-" + env.subject}, time.Time{})
	require.NoError(t, err, "%s entitlements", env.side)
	r.Entitlements = observeEntitlements(ents[env.subject], env.payer)
	singleEnts, err := c.ListEntitlements(ctx, env.subject, time.Time{})
	require.NoError(t, err, "%s single entitlements", env.side)
	r.SingleEntitlements = observeEntitlements(singleEnts, env.payer)
	hasEntitlement, err := c.HasEntitlement(ctx, env.subject, "premium", time.Time{})
	require.NoError(t, err, "%s has entitlement", env.side)
	r.HasEntitlement = hasEntitlement
	hasMissingEntitlement, err := c.HasEntitlement(ctx, env.subject, "missing", time.Time{})
	require.NoError(t, err, "%s has missing entitlement", env.side)
	r.HasMissingEntitlement = hasMissingEntitlement
	ghostRecs, ghostPresent := ents["ghost-"+env.subject]
	r.EntitlementsUnknownEmpty = ghostPresent && len(ghostRecs) == 0
	r.EntitlementsKeyCount = len(ents)
	_, err = c.ListActiveEntitlements(ctx, []string{" ", ""}, time.Time{})
	r.ErrEntitlementsNoSubjects = observeErr(t, env.side+" entitlements empty subjects", err)
	overCap := make([]string, 501)
	for i := range overCap {
		overCap[i] = fmt.Sprintf("s-%d", i)
	}
	_, err = c.ListActiveEntitlements(ctx, overCap, time.Time{})
	r.ErrEntitlementsOverCap = observeErr(t, env.side+" entitlements over cap", err)

	productAccess, err := c.ListProductAccess(ctx, env.subject)
	require.NoError(t, err, "%s product access", env.side)
	r.ProductAccess = observeProductAccess(productAccess, env.product)
	r.HasProductAccess, err = c.HasProductAccess(ctx, env.subject, env.product.String())
	require.NoError(t, err, "%s has product access", env.side)
	r.HasNoProductAccess, err = c.HasProductAccess(ctx, env.subject, uuid.NewString())
	require.NoError(t, err, "%s has missing product access", env.side)
	_, err = c.HasProductAccess(ctx, env.subject, "not-a-uuid")
	r.ErrProductAccessBadID = observeErr(t, env.side+" product access bad product id", err)

	// 21) Hierarchical budget-scope policies on the unified client (#473): write a
	// subject self cap + a (subject, role) pool + a platform cap through all three
	// new methods, then read back ONLY the subject-owned ones (the platform cap
	// stays invisible to the subject surface). Runs LAST so the stored caps never
	// gate the admit steps above. role uuid is per-side -> normalized to compare.
	require.NoError(t, c.SetInvokerSpendLimits(ctx, payerID, openrails.InvokerSpendLimitInput{
		Scope:    "invoker",
		ScopeKey: "user:conformance",
		Windows: []openrails.SpendLimitWindow{
			{Key: "inv-daily", WindowSeconds: 86400, Limit: 50_000},
		},
	}), "%s set-invoker-spend-limit (invoker)", env.side)
	require.NoError(t, c.SetInvokerSpendLimits(ctx, payerID, openrails.InvokerSpendLimitInput{
		Scope:  "role",
		RoleID: roleID,
		Windows: []openrails.SpendLimitWindow{
			{Key: "role-hourly", WindowSeconds: 3600, Limit: 20_000},
		},
	}), "%s set-invoker-spend-limit (role)", env.side)
	pols, err := c.InvokerSpendLimits(ctx, payerID)
	require.NoError(t, err, "%s invoker-spend-limits", env.side)
	for i := range pols {
		if pols[i].Scope == "role" {
			pols[i].RoleID = "ROLE" // role uuid is per-side; normalize
		}
	}
	r.InvokerSpendLimits = observeBudgetPolicies(pols)
	require.Len(t, r.InvokerSpendLimits, 2, "%s invoker-spend-limits", env.side)

	// 22) Merchant-configured flat delegated-invoker wasted-spend window (#499): set
	// the merchant config ($1/15m), report $2 wasted, then observe via AbuseUsage that
	// the CONFIGURED limit is enforced (not DefaultInvokerWastedWindows). embed and
	// remote must produce identical observable state.
	wastedActor := "user:wasted-" + env.side
	require.NoError(t, c.SetMerchantConfiguration(ctx, openrails.MerchantConfigurationInput{
		Profile: &openrails.MerchantProfileInput{
			DisplayName: "Conformance Billing",
			LogoURL:     "https://example.com/logo.png",
			FromEmail:   "billing@example.com",
			SupportURL:  "https://example.com/support",
		},
		DelegatedInvokerWastedSpendWindows: []openrails.BudgetWindowInput{
			{Key: "burst", WindowSeconds: 900, Limit: 1_000_000},
		},
	}), "%s set-merchant-configuration", env.side)
	r.MerchantProfile = observeMerchantProfile(t, ctx, env.pool)
	_, err = c.ReportWastedSpend(ctx, openrails.WastedSpendReport{
		CustomerID: payerID,
		Invoker:    wastedActor,
		Currency:   money.DefaultCurrency,
		Amount:     2_000_000,
		Source:     "conformance",
		SourceID:   "wasted-" + env.side,
		Reason:     "conformance",
	})
	require.NoError(t, err, "%s report-wasted-spend", env.side)
	au, err := c.AbuseUsage(ctx, payerID, wastedActor, money.DefaultCurrency, "")
	require.NoError(t, err, "%s abuse-usage", env.side)
	require.Equal(t, money.DefaultCurrency, au.Currency, "%s abuse-usage currency", env.side)
	r.InvokerWastedUsage = au.InvokerWindows
	require.Len(t, r.InvokerWastedUsage, 1, "%s invoker-wasted-usage: configured single window", env.side)
	require.Equal(t, int64(1_000_000), r.InvokerWastedUsage[0].Limit, "%s invoker-wasted-usage: CONFIGURED limit, not default", env.side)
	require.True(t, r.InvokerWastedUsage[0].OverBudget, "%s invoker-wasted-usage: $2 over the configured $1", env.side)

	return r
}

func observeMerchantProfile(t *testing.T, ctx context.Context, pool *pgxpool.Pool) obsMerchantProfile {
	t.Helper()
	var raw []byte
	err := pool.QueryRow(ctx, `
		SELECT config
		FROM openrails.merchant_configurations
		WHERE merchant_id = $1
	`, dbtest.TestMerchantID).Scan(&raw)
	require.NoError(t, err)
	var cfg models.MerchantConfiguration
	require.NoError(t, json.Unmarshal(raw, &cfg))
	return obsMerchantProfile{
		DisplayName: cfg.Profile.DisplayName,
		LogoURL:     cfg.Profile.LogoURL,
		FromEmail:   cfg.Profile.FromEmail,
		SupportURL:  cfg.Profile.SupportURL,
	}
}

func observeAccount(a *openrails.CreditAccount, payerID string) obsAccount {
	return obsAccount{
		Currency:              a.Currency,
		BillingMode:           a.BillingMode,
		BalanceAmount:         a.BalanceAmount,
		HeldAmount:            a.HeldAmount,
		AvailableAmount:       a.AvailableAmount,
		OutstandingOwedAmount: a.OutstandingOwedAmount,
		PayerMatches:          a.CustomerID == payerID,
	}
}

func observeProductAccess(grants []openrails.ProductAccessGrant, productID uuid.UUID) []obsProductAccess {
	out := make([]obsProductAccess, 0, len(grants))
	for _, g := range grants {
		out = append(out, obsProductAccess{
			ProductMatches: g.ProductID == productID.String(),
			CustomerIDSet:  g.CustomerID != "",
			ProductSlugSet: g.ProductSlug != "",
			SourceType:     g.SourceType,
			Status:         g.Status,
			HasPaymentID:   g.PaymentID != nil,
		})
	}
	return out
}

func TestConformance_EmbeddedAndStandaloneAreObservablyIdentical(t *testing.T) {
	ctx := context.Background()

	// Currency is the built-in default after the #476 decouple (money validates
	// the unit); both clients key Balance off it.
	currency := money.DefaultCurrency

	// Two REAL servers over the same migrated Postgres + shared Redis.
	h := integrationharness.New(t, ctx)
	embedded := h.StartEmbeddedHost(currency) // Server 1: embedded no-auth host
	standalone := h.StartStandalone(currency) // Server 2: real standalone + AuthKit

	pool := h.Pool()
	embeddedClient := embedded.Client()
	standaloneClient := standalone.Client()

	const issuer = "conformance"
	productAccessSvc := productaccess.NewService(dbtest.OpenAppDB(t, dbtest.SharedPostgresDSN(t)))
	newPayer := func(side string) (uuid.UUID, string, uuid.UUID) {
		id := uuid.New()
		subject := id.String()
		// The external subject is UUID-only (#364) and product-access grants FK the
		// customer id, so this fixture pins customer.id == subject UUID.
		_, err := pool.Exec(ctx, `
			INSERT INTO openrails.customers (id, merchant_id, issuer, subject, created_at, last_seen_at)
			VALUES ($1, $2, $3, $4, now(), now())`,
			id, dbtest.TestMerchantID.UUID(), issuer, subject)
		require.NoError(t, err)
		// One active entitlement row for the entitlements steps.
		_, err = pool.Exec(ctx, `
			INSERT INTO openrails.entitlements (id, merchant_id, customer_id, entitlement, start_at, source_id, source_type)
			VALUES ($1, $2, $3, 'premium', now() - interval '1 hour', $4, 'admin')`,
			uuid.New(), dbtest.TestMerchantID.UUID(), id, uuid.New())
		require.NoError(t, err)
		productID := uuid.New()
		_, err = pool.Exec(ctx, `
			INSERT INTO openrails.products (id, merchant_id, slug, display_name)
			VALUES ($1, $2, $3, $4)`,
			productID, dbtest.TestMerchantID.UUID(), "conf-product-"+side+"-"+productID.String(), "Conformance Product "+side)
		require.NoError(t, err)
		_, _, err = productAccessSvc.GrantProductAccess(dbtest.WithTestMerchant(ctx), productaccess.GrantParams{
			UserID:     subject,
			ProductID:  productID,
			SourceType: models.ProductAccessSourceAdmin,
			SourceID:   "conformance:" + side + ":" + productID.String(),
		})
		require.NoError(t, err)
		return id, subject, productID
	}
	payerEmbedded, subjectEmbedded, productEmbedded := newPayer("embedded")
	payerStandalone, subjectStandalone, productStandalone := newPayer("standalone")

	from := time.Now().Add(-time.Hour).UTC()
	to := time.Now().Add(time.Hour).UTC()

	resEmbedded := runScript(t, ctx, embeddedClient, scriptEnv{
		payer: payerEmbedded, invoker: "user:conf", currency: currency,
		resource: "ep-embedded", side: "embedded", from: from, to: to,
		pool: pool, issuer: issuer, subject: subjectEmbedded, product: productEmbedded,
	})
	resStandalone := runScript(t, ctx, standaloneClient, scriptEnv{
		payer: payerStandalone, invoker: "user:conf", currency: currency,
		resource: "ep-standalone", side: "standalone", from: from, to: to,
		pool: pool, issuer: issuer, subject: subjectStandalone, product: productStandalone,
	})

	// THE assertion: identical observable behavior across the two real surfaces.
	require.Equal(t, resEmbedded, resStandalone,
		"embedded and standalone surfaces diverged — fix the adapter, never the assertion")

	// Release is idempotent on BOTH real transports (#513): releasing an unknown
	// request id is a no-op (the reservation is already gone), never an error.
	require.NoError(t, embeddedClient.Release(ctx, uuid.NewString()))
	require.NoError(t, standaloneClient.Release(ctx, uuid.NewString()))

	// Standalone real-auth contract: a bad bearer is rejected by the real
	// ServiceCredentialRequired middleware -> 401 -> ErrUnauthorized.
	badStandalone := standalone.Client(
		openrails.WithTokenProvider(func(context.Context) (string, error) { return "openrails_st_wrong_token", nil }),
	)
	_, err := badStandalone.Balance(ctx, payerStandalone.String())
	require.ErrorIs(t, err, openrails.ErrUnauthorized)
}
