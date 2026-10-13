package payment

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/modules/mandates"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
	"github.com/open-rails/openrails/internal/writeposture"
)

// SavePlatformCard verifies a collected card without taking money and saves it
// for this customer. It neither selects a default nor changes any subscription.
func (s *PurchaseService) SavePlatformCard(ctx context.Context, req billing.CreatePaymentMethodParams, user *UserIdentity) (SavedCard, error) {
	token := strings.TrimSpace(req.Token)
	collectedID, err := uuid.Parse(token)
	if err != nil || collectedID == uuid.Nil || req.Card != nil || req.PSPID.IsZero() {
		return SavedCard{}, ErrOrderAttemptValidation
	}
	token = collectedID.String()
	mid, err := merchant.Require(ctx)
	if err != nil {
		return SavedCard{}, err
	}
	customer, err := customerIDFromUser(user.ID)
	if err != nil {
		return SavedCard{}, err
	}
	custody := db.CustodianIDFromContext(ctx)
	if s.CustodianSaleService == nil || custody == uuid.Nil {
		return SavedCard{}, ErrCardCaptureUnavailable
	}
	d := s.CustodianSaleService.DB
	// One collection has one identity, including when another customer submits
	// the same handle. Returning an existing card still checks its owner.
	id := uuid.NewSHA1(uuid.NameSpaceURL, []byte("openrails:platform-card-setup:"+mid.String()+":"+custody.String()+":"+token))
	existing, err := d.Gen(ctx).GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: id})
	if err == nil {
		return platformSetupResult(existing, customer)
	}
	if !db.IsNotFound(err) {
		return SavedCard{}, err
	}
	if (writeposture.View{Config: s.Config, DB: d}).Posture(ctx, mid.UUID()).ReadOnly() {
		return SavedCard{}, ProviderWritesDisabled("card setup is unavailable while provider writes are disabled")
	}
	cfg, proxy, bt, err := s.CustodianSaleService.basisTheory(ctx, req.PSPID.UUID())
	if err != nil {
		return SavedCard{}, err
	}
	if cfg.Custody.Custodian != models.CustodianPlatform {
		return SavedCard{}, ErrOrderAttemptValidation
	}

	// The token's fixed ID recovers a conversion even after BT's idempotency
	// window or the collection intent expires. Its private metadata records the
	// setup that was verified, never caller-supplied authorization facts.
	saved, err := bt.GetToken(ctx, id.String())
	if basistheory.IsNotFound(err) {
		collected, readErr := bt.GetTokenIntent(ctx, token)
		if basistheory.IsNotFound(readErr) {
			return SavedCard{}, ErrPaymentMethodStale
		}
		if readErr != nil {
			return SavedCard{}, readErr
		}
		if err := validatePlatformCollection(cfg.Custody, collected); err != nil {
			return SavedCard{}, err
		}
		reference, verifyErr := proxy.WithSource(nmiproxy.Source{TokenIntentID: token}).VerifyCard(ctx, "pms-"+id.String())
		if verifyErr != nil {
			var refusal *nmi.CustomerVaultError
			if errors.As(verifyErr, &refusal) && !nmi.RequiresVerification(verifyErr) {
				return SavedCard{}, &paymentmethods.PaymentMethodError{Err: verifyErr, Message: "card verification failed", Rail: "nmi", LocalizationID: refusal.LocalizationID}
			}
			return SavedCard{}, verifyErr
		}
		saved, err = bt.ConvertTokenIntent(ctx, token, basistheory.ConvertOpts{ID: id.String(), Container: cfg.Custody.Container, IdempotencyKey: "pmsetup:" + id.String(), Metadata: map[string]string{
			"customer_id": customer.String(), "psp_id": req.PSPID.String(), "account_id": cfg.Scope.AccountID, "verification_id": reference,
		}})
	}
	if err != nil {
		return SavedCard{}, err
	}
	if saved.ID != id.String() || saved.Type != "card" || saved.TenantID != cfg.Custody.AccountID || len(saved.Containers) != 1 || saved.Containers[0] != cfg.Custody.Container || saved.Metadata["customer_id"] != customer.String() || saved.Metadata["psp_id"] != req.PSPID.String() || saved.Metadata["account_id"] != cfg.Scope.AccountID || saved.Metadata["verification_id"] == "" {
		return SavedCard{}, ErrOrderAttemptConflict
	}
	if saved.Card == nil || saved.Card.Last4 == "" || saved.Card.ExpirationMonth < 1 || saved.Card.ExpirationMonth > 12 || saved.Card.ExpirationYear < 2000 {
		return SavedCard{}, errors.New("platform vault returned incomplete card details")
	}
	now := s.now().UTC()
	method := &models.PaymentMethod{ID: id, Status: paymentmethods.StatusActive, CustomerID: customer, Rail: models.RailNMI, Custodian: models.CustodianPlatform, CustodianID: &custody,
		RailMethodRef: saved.ID, Fingerprint: saved.Fingerprint, ChargeVia: nmiproxy.ViaPANProxy, CreatedAt: now, UpdatedAt: now,
		Card: models.ParseCard(saved.Card.Brand, saved.Card.Last4, ""), Metadata: paymentmethods.MergeBillingDetails(nil, req.BillingDetails)}
	method.Card.ExpMonth, method.Card.ExpYear = saved.Card.ExpirationMonth, saved.Card.ExpirationYear
	result := SavedCard{Method: method}
	err = d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		td := d.NewWithPgxTx(tx)
		q := td.Gen(ctx)
		if err := db.EnsureCustomerRow(ctx, td.Qx(ctx), mid.UUID(), customer); err != nil {
			return err
		}
		if _, err := q.LockCustomerForSpend(ctx, gen.LockCustomerForSpendParams{MerchantID: mid.UUID(), ID: customer}); err != nil {
			return err
		}
		handle := paymentmethods.CustodianHandle{Custodian: custody, Method: saved.ID}
		if err := paymentmethods.LockCustodianHandles(ctx, q, mid.UUID(), handle); err != nil {
			return err
		}
		if err := paymentmethods.RequireCustodianHandleAvailable(ctx, q, mid.UUID(), handle); err != nil {
			return err
		}
		existing, err := q.GetPaymentMethodByID(ctx, gen.GetPaymentMethodByIDParams{MerchantID: mid.UUID(), ID: id})
		if err == nil {
			result, err = platformSetupResult(existing, customer)
			return err
		}
		if !db.IsNotFound(err) {
			return err
		}
		if err := paymentmethods.NewPaymentMethodRepo(td).Create(ctx, method); err != nil {
			return err
		}
		_, err = mandates.Create(ctx, q, mandates.Agreement{MerchantID: mid.UUID(), CustomerID: customer, PaymentMethodID: id, PSPID: req.PSPID.UUID(),
			Rail: "nmi", Kind: charge.AgreementCardOnFile, Lineage: &charge.Mandate{InitialTransactionID: saved.Metadata["verification_id"]}, AcceptedAt: now})
		return err
	})
	if err != nil {
		return SavedCard{}, err
	}
	return result, nil
}

func platformSetupResult(row gen.BillingPaymentMethod, customer uuid.UUID) (SavedCard, error) {
	if row.CustomerID != customer {
		return SavedCard{}, ErrOrderAttemptConflict
	}
	if !paymentmethods.Chargeable(row) {
		return SavedCard{}, ErrPaymentMethodStale
	}
	method, err := models.PaymentMethodFromGen(row)
	return SavedCard{Method: method}, err
}
