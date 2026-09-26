package checkout

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/catalog"
	"github.com/open-rails/openrails/internal/modules/entitlements"
	"github.com/open-rails/openrails/internal/modules/payments"
	solanamodule "github.com/open-rails/openrails/internal/modules/solana"
	"github.com/open-rails/openrails/internal/modules/subscriptions"
)

// SettleSolanaTransfer is the only path that credits a Solana Pay purchase,
// for the poller and the client confirm alike. In one transaction it locks the
// reference, classifies the transfer, credits the checkout when the transfer
// settles it, and records the signature. Replays of a signature return its
// receipt; a second transfer to a paid, late or closed checkout is recorded
// for review. ErrSignatureClaimed: the signature already settled another
// reference, and nothing was written.
func (s *CheckoutSessionService) SettleSolanaTransfer(ctx context.Context, reference string, t solanamodule.ObservedTransfer) (*solanamodule.Receipt, error) {
	if s.db == nil {
		return nil, errors.New("checkout settlement database unavailable")
	}
	var out *solanamodule.Receipt
	err := s.db.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		d := s.db.NewWithPgxTx(tx)
		ledger := solanamodule.NewPayLedger(d)
		ref, err := ledger.Lock(ctx, reference)
		if err != nil {
			return err
		}
		if ref.Kind != string(solanamodule.ReferencePurchase) {
			return fmt.Errorf("solana reference %s is not a purchase", reference)
		}
		if seen, err := ledger.Receipt(ctx, reference, t.Signature); err != nil || seen != nil {
			out = seen
			return err
		}
		session, err := NewCheckoutSessionRepo(d).GetByID(ctx, ref.CheckoutSessionID)
		if err != nil {
			return err
		}
		now := s.now()
		receipt := solanamodule.Receipt{
			Reference: reference, Signature: t.Signature, SessionID: session.ID,
			TokenMint: getStringField(session.RailState, "token_mint"), ExpectedAmount: getUint64Field(session.RailState, "token_amount"),
			ReceivedAmount: t.Amount, Payer: t.Payer, LandedAt: t.LandedAt,
		}
		open := session.Status == models.CheckoutSessionStatusCreated || session.Status == models.CheckoutSessionStatusRequiresAction || session.Status == models.CheckoutSessionStatusExpired
		receipt.Disposition, receipt.ReviewReason = solanamodule.Decide(ref, open, receipt.ExpectedAmount, t, now)
		if receipt.Disposition == solanamodule.Credited {
			paymentID, err := s.creditSolanaPurchase(ctx, d, session, reference, t)
			if err != nil {
				return err
			}
			receipt.PaymentID = &paymentID
			if confirmed, err := ledger.Confirm(ctx, reference, t.Signature, now); err != nil || !confirmed {
				return errors.Join(err, fmt.Errorf("solana reference %s left pending state under lock", reference))
			}
		}
		if err := ledger.Record(ctx, receipt, now); err != nil {
			return err
		}
		if err := ledger.RaiseReview(ctx, receipt, session.CustomerID.String(), now); err != nil {
			return err
		}
		out = &receipt
		return nil
	})
	return out, err
}

// creditSolanaPurchase registers the checkout's payment from its accepted
// terms inside the settlement transaction.
func (s *CheckoutSessionService) creditSolanaPurchase(ctx context.Context, d *db.DB, session *models.CheckoutSession, reference string, t solanamodule.ObservedTransfer) (uuid.UUID, error) {
	if terms, err := purchaseTerms(session); err != nil || terms == nil {
		return uuid.Nil, errors.Join(err, errors.New("solana checkout has no accepted purchase terms"))
	}
	purchase := NewCheckoutPurchaseService(catalog.NewPriceService(d), catalog.NewProductService(d), payments.NewPaymentService(d, s.clock), entitlements.NewEntitlementService(d, s.clock), nil, s.clock)
	purchase.SubscriptionService = subscriptions.NewSubscriptionService(d, purchase.PriceService, purchase.ProductService, nil, s.clock)
	result, err := purchase.RegisterPurchase(db.WithPSPID(ctx, session.PspID), &payments.RegisterPurchaseRequest{
		CheckoutSessionID: session.ID,
		UserID:            session.CustomerID.String(),
		PriceID:           *session.PriceID,
		Rail:              string(models.RailSolana),
		TransactionID:     t.Signature,
		Amount:            *session.Amount,
		Currency:          *session.Currency,
		PurchasedAt:       t.LandedAt,
		WalletPurchase:    true,
		Metadata: map[string]any{
			"solana_reference":    reference,
			"checkout_session_id": session.ID.String(),
			"solana_payer_wallet": strings.TrimSpace(t.Payer),
			"solana_token_symbol": getStringField(session.RailState, "token_symbol"),
			"solana_token_mint":   getStringField(session.RailState, "token_mint"),
			"solana_token_amount": getUint64Field(session.RailState, "token_amount"),
			"solana_received":     t.Amount,
			"solana_recipient":    getStringField(session.RailState, "recipient"),
		},
		AttemptKind: payments.AttemptInitial,
	})
	if err != nil {
		return uuid.Nil, err
	}
	return result.PaymentID, nil
}
