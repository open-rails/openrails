package checkout

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/railresolve"
)

const (
	devnetUSDCMint  = "4zMMC9srt5Ri5X14GAgXhaHii3GnPAEERYPJgZJDncDU"
	recipientWallet = "DzGLHdTfgHCYh8v3qNGJHn85CyX7aeFmqoUdVRBYkWMh"
	solanaReference = "11111111111111111111111111111112"
)

func solanaRails() railresolve.FixedSet {
	return railresolve.FixedSet{"solana": {Rail: models.RailSolana, Solana: &config.SolanaRailConfig{
		Network: "devnet", Tokens: map[string]config.TokenConfig{"USDC": {Mint: devnetUSDCMint}},
	}}}
}

func TestSolanaPlanTerms(t *testing.T) {
	plan := map[string]string{"plan_id": "7", "mint_symbol": " USDC ", "amount_base_units": "10000000", "period_hours": "720", "created_at": "1700000000"}
	terms, err := parseSolanaPlanTerms(plan)
	require.NoError(t, err)
	require.Equal(t, solanaPlanTerms{planID: 7, mintSymbol: "USDC", amount: 10_000_000, period: 720, createdAt: 1_700_000_000}, terms)

	price := &models.Price{}
	price.SetRailConfig(models.RailSolana, plan)
	require.True(t, priceHasSolanaRecurring(price))
	require.False(t, priceHasSolanaRecurring(nil))
	require.False(t, priceHasSolanaRecurring(&models.Price{}))

	for _, bad := range [][2]string{{"plan_id", "x"}, {"amount_base_units", "0"}, {"amount_base_units", ""}, {"period_hours", "0"}, {"period_hours", "-1"}} {
		cfg := map[string]string{}
		for k, v := range plan {
			cfg[k] = v
		}
		cfg[bad[0]] = bad[1]
		_, err := parseSolanaPlanTerms(cfg)
		require.ErrorIs(t, err, ErrCheckoutSessionValidation, "%v", bad)
	}
	_, err = parseSolanaPlanTerms(nil)
	require.Error(t, err)

	partial := &models.Price{}
	partial.SetRailConfig(models.RailSolana, map[string]string{"plan_id": "7", "period_hours": "720", "mint_symbol": "USDC"})
	require.False(t, priceHasSolanaRecurring(partial), "an unpriced plan is not recurring")
}

// Which sessions surface a solana_pay_url, and the flow is declared, never
// inferred (or#893).
func TestSolanaSessionFlows(t *testing.T) {
	mk := func(rail models.Rail, mode models.CheckoutSessionMode, flow string) *models.CheckoutSession {
		return &models.CheckoutSession{ID: uuid.New(), Status: models.CheckoutSessionStatusRequiresAction, Rail: rail, Mode: mode, RailState: map[string]any{"flow": flow}}
	}
	for _, tc := range []struct {
		name    string
		session *models.CheckoutSession
		payURL  bool
	}{
		{"recurring subscribe", mk(models.RailSolana, models.CheckoutSessionModeSubscription, "transaction_request"), true},
		{"one-off transaction request", mk(models.RailSolana, models.CheckoutSessionModeOneOff, "transaction_request"), true},
		{"cancel lifecycle", mk(models.RailSolana, models.CheckoutSessionModeSolanaCancel, ""), true},
		{"tier-change lifecycle", mk(models.RailSolana, models.CheckoutSessionModeSolanaTierChange, ""), true},
		{"wallet-connected subscribe", mk(models.RailSolana, models.CheckoutSessionModeSubscription, "subscription"), false},
		{"transfer request", mk(models.RailSolana, models.CheckoutSessionModeOneOff, "transfer_request"), false},
		{"non-solana rail", mk(models.RailStripe, models.CheckoutSessionModeSubscription, "transaction_request"), false},
		{"nil", nil, false},
	} {
		require.Equal(t, tc.payURL, solanaSessionUsesPayURL(tc.session), tc.name)
	}

	for _, tc := range []struct{ base, prefix string }{
		{"https://api.test.com", "solana:https://api.test.com/v1/checkout/"},
		{"https://api.test.com/billing", "solana:https://api.test.com/billing/v1/checkout/"},
		{"https://api.test.com/", "solana:https://api.test.com/v1/checkout/"},
	} {
		svc := &CheckoutSessionService{config: &config.Config{PublicBillingBaseURL: tc.base}, rails: solanaRails()}
		session := mk(models.RailSolana, models.CheckoutSessionModeOneOff, "transaction_request")
		url := svc.sessionToResponse(session).Payment.SolanaPayURL
		require.Equal(t, fmt.Sprintf("%s%s/solana-pay", tc.prefix, openrails.CheckoutSessionID(session.ID)), url, tc.base)
	}

	require.True(t, isSolanaTransferRequestFlow(mk(models.RailSolana, "", " Transfer_Request ")))
	require.False(t, isSolanaTransferRequestFlow(mk(models.RailSolana, "", "")), "a missing flow is never defaulted")
	require.False(t, isSolanaTransferRequestFlow(mk(models.RailSolana, "", "transaction_request")))
	require.False(t, isSolanaTransferRequestFlow(nil))

	notLanded := fmt.Errorf("recurring: subscription %s not found on-chain — did the wallet run the atomic subscribe?", "PdA")
	require.True(t, isSolanaSubscribeNotLandedErr(fmt.Errorf("wrap: %w", notLanded)))
	require.False(t, isSolanaSubscribeNotLandedErr(errors.New("create membership: db down")))
	require.False(t, isSolanaSubscribeNotLandedErr(nil))
}

// A poller-verified (settled) payment outlives the quote window; without that
// proof expiry stands, and failed/canceled are never clock outcomes.
func TestSucceedTransition(t *testing.T) {
	for _, tc := range []struct {
		status  models.CheckoutSessionStatus
		settled bool
		proceed bool
		err     error
	}{
		{models.CheckoutSessionStatusRequiresAction, false, true, nil},
		{models.CheckoutSessionStatusCreated, true, true, nil},
		{models.CheckoutSessionStatusSucceeded, true, false, nil},
		{models.CheckoutSessionStatusExpired, false, false, ErrCheckoutSessionConflict},
		{models.CheckoutSessionStatusExpired, true, true, nil},
		{models.CheckoutSessionStatusFailed, true, false, ErrCheckoutSessionConflict},
		{models.CheckoutSessionStatusCanceled, true, false, ErrCheckoutSessionConflict},
	} {
		proceed, err := succeedTransition(tc.status, tc.settled)
		require.Equal(t, tc.proceed, proceed, "%s settled=%v", tc.status, tc.settled)
		require.ErrorIs(t, err, tc.err, "%s settled=%v", tc.status, tc.settled)
	}
}

type noopSolanaTransactions struct{}

func (noopSolanaTransactions) BuildPaymentTransactionFromQuote(context.Context, *solanamodule.PaymentTransactionBuildRequest) (*solanamodule.TransactionBuildResponse, error) {
	return nil, errors.New("unexpected build")
}
func (noopSolanaTransactions) ObserveTransfer(context.Context, solanaint.ObserveTransferRequest) (*solanaint.TransferObservation, error) {
	return nil, errors.New("unexpected observe")
}
func (noopSolanaTransactions) BlockHeight(context.Context) (uint64, error) {
	return 0, errors.New("unexpected block height")
}
func (noopSolanaTransactions) ReferenceHasTransfers(context.Context, string) (bool, error) {
	return false, errors.New("unexpected signature read")
}

// Build and confirm use only the persisted quote; a session missing any part
// of it is refused before anything reaches the chain.
func TestSolanaSessionUsesPersistedQuote(t *testing.T) {
	quoted := func() *models.CheckoutSession {
		ref := solanaReference
		return &models.CheckoutSession{
			ID: uuid.New(), CustomerID: uuid.New(), PriceID: new(uuid.New()), Amount: new(int64(10_000)), Currency: new("USD"), Reference: &ref,
			RailState: map[string]any{"token_symbol": "USDC", "token_mint": devnetUSDCMint, "token_amount": uint64(100_000_000), "recipient": recipientWallet},
		}
	}
	s := quoted()
	req, err := solanaBuildRequestFromSession(s, "payer_wallet", "USDC")
	require.NoError(t, err)
	require.Equal(t, uint64(100_000_000), req.TokenAmount)
	require.Equal(t, devnetUSDCMint, req.TokenMint)
	require.Equal(t, recipientWallet, req.Recipient)
	require.Equal(t, s.CustomerID.String(), req.UserID)
	require.Equal(t, s.ID, req.SessionID)
	require.Equal(t, int64(10_000), req.Amount)

	svc := &CheckoutSessionService{rails: solanaRails(), solanaTransactionService: noopSolanaTransactions{}, checkoutService: &capturingExecutor{}}
	confirm := &CheckoutSessionConfirmRequest{Payment: CheckoutSessionConfirmPayment{Signature: "sig"}}
	for _, tc := range []struct {
		name   string
		mutate func(*models.CheckoutSession)
		want   string
	}{
		{"token amount", func(s *models.CheckoutSession) { delete(s.RailState, "token_amount") }, "token_amount"},
		{"recipient", func(s *models.CheckoutSession) { delete(s.RailState, "recipient") }, "recipient missing"},
		{"reference", func(s *models.CheckoutSession) { s.Reference = nil }, "reference missing"},
		{"mint", func(s *models.CheckoutSession) {
			s.RailState["token_mint"] = "OtherMint1111111111111111111111111111111111"
		}, "token_mint mismatch"},
		{"native SOL mint for a token", func(s *models.CheckoutSession) { s.RailState["token_mint"] = solanamodule.WrappedSOLMint }, "native SOL mint"},
		{"wallet", func(s *models.CheckoutSession) { s.RailState["payer"] = "someone-else" }, "wallet does not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := quoted()
			tc.mutate(s)
			req := *confirm
			req.Payment.Wallet = "payer_wallet"
			_, err := svc.confirmSolanaSession(context.Background(), s, &req, &UserIdentity{ID: s.CustomerID.String()})
			require.ErrorIs(t, err, ErrCheckoutSessionValidation)
			require.ErrorContains(t, err, tc.want)
		})
	}
	for _, key := range []string{"token_amount", "token_mint", "recipient"} {
		s := quoted()
		delete(s.RailState, key)
		_, err := solanaBuildRequestFromSession(s, "payer_wallet", "USDC")
		require.ErrorIs(t, err, ErrCheckoutSessionValidation, key)
	}
}

func TestSolanaLifecycleState(t *testing.T) {
	subID, newPrice := uuid.New(), uuid.New()
	cancel := (&CheckoutSessionService{}).buildLifecycleState(&solanaLifecycleState{mode: models.CheckoutSessionModeSolanaCancel, subscriptionID: subID, productName: "Pro"})
	require.Equal(t, string(models.CheckoutSessionModeSolanaCancel), cancel["flow"])
	require.Equal(t, subID.String(), cancel["subscription_id"])
	require.NotContains(t, cancel, "new_price_id")

	change := (&CheckoutSessionService{}).buildLifecycleState(&solanaLifecycleState{
		mode: models.CheckoutSessionModeSolanaTierChange, subscriptionID: subID,
		tierChange: &resolvedSolanaLifecycleTierChange{newPriceID: newPrice, isUpgrade: true, firstChargeBaseUnits: 31_330_000,
			newTerms: solanaPlanTerms{planID: 99, mintSymbol: "USDC", amount: 50_000_000, period: 720, createdAt: 1_700_000_000}},
	})
	require.Equal(t, newPrice.String(), change["new_price_id"])
	require.Equal(t, "31330000", change["tier_first_charge_base_units"], "base units persist as decimal strings")
	require.Equal(t, uint64(50_000_000), getUint64Field(change, "tier_new_amount_base_units"))
	require.True(t, getBoolField(change, "tier_is_upgrade"))
}

// Poller-driven confirms have no request PSP; they pin the session's so the
// rows they write are attributable (or#893), and never invent one.
func TestPollerConfirmContextPinsSessionPSP(t *testing.T) {
	svc := &CheckoutSessionService{}
	psp := uuid.New()
	require.Equal(t, psp, db.PSPIDFromContext(svc.pollerConfirmContext(context.Background(), &models.CheckoutSession{PspID: psp})))
	require.Equal(t, uuid.Nil, db.PSPIDFromContext(svc.pollerConfirmContext(context.Background(), nil)))
	require.Equal(t, uuid.Nil, db.PSPIDFromContext(svc.pollerConfirmContext(context.Background(), &models.CheckoutSession{})))
}
