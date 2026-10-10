package checkout

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/config"
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
		require.ErrorIs(t, err, ErrCheckoutAttemptValidation, "%v", bad)
	}
	_, err = parseSolanaPlanTerms(nil)
	require.Error(t, err)

	partial := &models.Price{}
	partial.SetRailConfig(models.RailSolana, map[string]string{"plan_id": "7", "period_hours": "720", "mint_symbol": "USDC"})
	require.False(t, priceHasSolanaRecurring(partial), "an unpriced plan is not recurring")
}

// Which sessions surface a solana_pay_url, and the flow is declared, never
// inferred.
func TestSolanaSessionFlows(t *testing.T) {
	mk := func(rail models.Rail, mode models.CheckoutAttemptMode, flow string) *models.CheckoutAttempt {
		return &models.CheckoutAttempt{ID: uuid.New(), Status: models.CheckoutAttemptStatusRequiresAction, Rail: rail, Mode: mode, RailState: map[string]any{"flow": flow}}
	}
	for _, tc := range []struct {
		name    string
		session *models.CheckoutAttempt
		payURL  bool
	}{
		{"recurring subscribe", mk(models.RailSolana, models.CheckoutAttemptModeSubscription, "transaction_request"), true},
		{"one-off transaction request", mk(models.RailSolana, models.CheckoutAttemptModeOneOff, "transaction_request"), true},
		{"wallet-connected subscribe", mk(models.RailSolana, models.CheckoutAttemptModeSubscription, "subscription"), false},
		{"transfer request", mk(models.RailSolana, models.CheckoutAttemptModeOneOff, "transfer_request"), false},
		{"non-solana rail", mk(models.RailStripe, models.CheckoutAttemptModeSubscription, "transaction_request"), false},
		{"nil", nil, false},
	} {
		require.Equal(t, tc.payURL, solanaSessionUsesPayURL(tc.session), tc.name)
	}

	for _, tc := range []struct{ base, prefix string }{
		{"https://api.test.com", "solana:https://api.test.com/v1/checkout-attempts/"},
		{"https://api.test.com/billing", "solana:https://api.test.com/billing/v1/checkout-attempts/"},
		{"https://api.test.com/", "solana:https://api.test.com/v1/checkout-attempts/"},
	} {
		svc := &CheckoutAttemptService{config: &config.Config{PublicBillingBaseURL: tc.base}, rails: solanaRails()}
		session := mk(models.RailSolana, models.CheckoutAttemptModeOneOff, "transaction_request")
		url := svc.sessionToResponse(session).Payment.SolanaPayURL
		require.Equal(t, fmt.Sprintf("%s%s/solana-pay", tc.prefix, billing.CheckoutAttemptID(session.ID)), url, tc.base)
	}

	require.True(t, isSolanaTransferRequestFlow(mk(models.RailSolana, "", " Transfer_Request ")))
	require.False(t, isSolanaTransferRequestFlow(mk(models.RailSolana, "", "")), "a missing flow is never defaulted")
	require.False(t, isSolanaTransferRequestFlow(mk(models.RailSolana, "", "transaction_request")))
	require.False(t, isSolanaTransferRequestFlow(nil))

}

// A poller-verified (settled) payment outlives the quote window; without that
// proof expiry stands, and failed/canceled are never clock outcomes.
func TestSucceedTransition(t *testing.T) {
	for _, tc := range []struct {
		status  models.CheckoutAttemptStatus
		settled bool
		proceed bool
		err     error
	}{
		{models.CheckoutAttemptStatusRequiresAction, false, true, nil},
		{models.CheckoutAttemptStatusCreated, true, true, nil},
		{models.CheckoutAttemptStatusSucceeded, true, false, nil},
		{models.CheckoutAttemptStatusExpired, false, false, ErrCheckoutAttemptConflict},
		{models.CheckoutAttemptStatusExpired, true, true, nil},
		{models.CheckoutAttemptStatusFailed, true, false, ErrCheckoutAttemptConflict},
		{models.CheckoutAttemptStatusCanceled, true, false, ErrCheckoutAttemptConflict},
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
func (noopSolanaTransactions) ReferenceHasOurTransfer(context.Context, string, string, string, uuid.UUID) (bool, error) {
	return false, errors.New("unexpected signature read")
}

// Build and confirm use only the persisted quote; a session missing any part
// of it is refused before anything reaches the chain.
func TestSolanaSessionUsesPersistedQuote(t *testing.T) {
	quoted := func() *models.CheckoutAttempt {
		ref := solanaReference
		return &models.CheckoutAttempt{
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

	svc := &CheckoutAttemptService{rails: solanaRails(), solanaTransactionService: noopSolanaTransactions{}, checkoutService: &capturingExecutor{}}
	confirm := &CheckoutAttemptConfirmRequest{Payment: CheckoutAttemptConfirmPayment{Signature: "sig"}}
	for _, tc := range []struct {
		name   string
		mutate func(*models.CheckoutAttempt)
		want   string
	}{
		{"token amount", func(s *models.CheckoutAttempt) { delete(s.RailState, "token_amount") }, "token_amount"},
		{"recipient", func(s *models.CheckoutAttempt) { delete(s.RailState, "recipient") }, "recipient missing"},
		{"reference", func(s *models.CheckoutAttempt) { s.Reference = nil }, "reference missing"},
		{"mint", func(s *models.CheckoutAttempt) {
			s.RailState["token_mint"] = "OtherMint1111111111111111111111111111111111"
		}, "token_mint mismatch"},
		{"native SOL mint for a token", func(s *models.CheckoutAttempt) { s.RailState["token_mint"] = solanamodule.WrappedSOLMint }, "native SOL mint"},
		{"wallet", func(s *models.CheckoutAttempt) { s.RailState["payer"] = "someone-else" }, "wallet does not match"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := quoted()
			tc.mutate(s)
			req := *confirm
			req.Payment.Wallet = "payer_wallet"
			_, err := svc.confirmSolanaSession(context.Background(), s, &req, &UserIdentity{ID: s.CustomerID.String()})
			require.ErrorIs(t, err, ErrCheckoutAttemptValidation)
			require.ErrorContains(t, err, tc.want)
		})
	}
	for _, key := range []string{"token_amount", "token_mint", "recipient"} {
		s := quoted()
		delete(s.RailState, key)
		_, err := solanaBuildRequestFromSession(s, "payer_wallet", "USDC")
		require.ErrorIs(t, err, ErrCheckoutAttemptValidation, key)
	}
}

// Poller-driven confirms have no request PSP; they pin the session's so the
// rows they write are attributable, and never invent one.
func TestPollerConfirmContextPinsSessionPSP(t *testing.T) {
	svc := &CheckoutAttemptService{}
	psp := uuid.New()
	require.Equal(t, psp, db.PSPIDFromContext(svc.pollerConfirmContext(context.Background(), &models.CheckoutAttempt{PspID: psp})))
	require.Equal(t, uuid.Nil, db.PSPIDFromContext(svc.pollerConfirmContext(context.Background(), nil)))
	require.Equal(t, uuid.Nil, db.PSPIDFromContext(svc.pollerConfirmContext(context.Background(), &models.CheckoutAttempt{})))
}

// A settlement error is retried only when another attempt can get past it —
// however deeply it is wrapped; anything else is recorded for review.
func TestTransientSettleError(t *testing.T) {
	pg := func(code string) error { return &pgconn.PgError{Code: code} }
	for err, transient := range map[error]bool{
		pg("40001"): true, pg("40P01"): true, pg("55P03"): true, pg("23505"): true, pg("57014"): true, pg("08006"): true,
		fmt.Errorf("settle: %w", pg("55P03")):                         true,
		errors.Join(errors.New("rollback"), pg("40001")):              true,
		fmt.Errorf("deep: %w", fmt.Errorf("x: %w", pg("23505"))):      true,
		context.DeadlineExceeded:                                      true,
		pg("23514"):                                                   false,
		pg("22P02"):                                                   false,
		errors.New("payment transaction belongs to a different user"): false,
		ErrPaymentTransactionTaken:                                    false,
	} {
		require.Equal(t, transient, transientSettleError(err), "%v", err)
	}
}
