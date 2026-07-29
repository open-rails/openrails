package checkout

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/basistheory"
	"github.com/open-rails/openrails/internal/integrations/nmi"
	"github.com/open-rails/openrails/internal/intents"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/modules/payments"
	"github.com/open-rails/openrails/internal/modules/payments/charge"
	"github.com/open-rails/openrails/internal/modules/payments/rails/vaultedcard"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Vaulted-card checkout collection (#795 B5): the browser tokenizes into a BT
// token INTENT (Elements; the PAN goes browser->vault, SAQ A); the engine
// accepts ONLY {bt_token_intent_id}. The CIT charges the INTENT (CVC present),
// converts to a durable token in-request on approval, writes the instrument
// row, and persists the stored-credential anchor exactly like the nmidirect
// paths. NT provisioning rides behind the merchant flag and is never
// load-bearing.

// TypeVaultedCardSale is the write-through checkout sale intent (#674) for the
// vaulted_card rail. The NMI order id derives from the intent id, so the
// verify leg answers "did THIS sale charge?" against the GATEWAY (the BT proxy
// has no idempotency — the intents log + NMI dup detection are the safety).
const TypeVaultedCardSale = "vaulted_card_sale"

func VaultedCardSaleIdempotencyKey(checkoutIdempotencyKey string) string {
	return TypeVaultedCardSale + ":" + strings.TrimSpace(checkoutIdempotencyKey)
}

// VaultedCardSalePayload carries everything Execute/Verify need without the
// originating request. NEVER a PAN — the intent id is the only card handle.
type VaultedCardSalePayload struct {
	TokenIntentID string    `json:"bt_token_intent_id"`
	AmountMicros  int64     `json:"amount_micros"`
	Currency      string    `json:"currency"`
	Description   string    `json:"description"`
	UserID        string    `json:"user_id"`
	PriceID       uuid.UUID `json:"price_id"`
	E2ERunID      string    `json:"e2e_run_id,omitempty"`
}

// CheckoutVaultedCardService runs vaulted_card one-time sales.
type CheckoutVaultedCardService struct {
	DB                   *db.DB
	PurchaseService      *CheckoutPurchaseService
	PaymentMethodService vaultedCardInstrumentStore
	IdempotencyStore     checkoutIdempotencyStore
	Rails                railresolve.Source
	Config               *config.Config
	Intents              intentExecutor
	// BTBaseURLOverride points every BT byte at a fake server (test seam).
	BTBaseURLOverride string
	// GatewayDirectPostURLOverride overrides the proxy destination (test seam).
	GatewayDirectPostURLOverride string
	// DisableGatewayVerify skips the NMI query-API verify leg (test seam for
	// sandbox keys that cannot use query.php).
	DisableGatewayVerify bool
}

type vaultedCardInstrumentStore interface {
	Create(ctx context.Context, method *models.PaymentMethod) error
	GetByRailMethodRef(ctx context.Context, provider, methodRef string) (*models.PaymentMethod, error)
}

// resolveConfig arms the ctx merchant's vaulted_card credentials (#788).
func (s *CheckoutVaultedCardService) resolveConfig(ctx context.Context) (*config.VaultedCardRailConfig, error) {
	if s.Rails == nil {
		return nil, errors.New("vaulted_card rail resolution not configured")
	}
	rc, err := s.Rails.RailConfig(ctx, string(models.RailVaultedCard), "")
	if err != nil {
		return nil, err
	}
	if rc.VaultedCard == nil {
		return nil, errors.New("vaulted_card account resolved without credentials")
	}
	return rc.VaultedCard, nil
}

func (s *CheckoutVaultedCardService) btClient(cfg *config.VaultedCardRailConfig) (*basistheory.Client, error) {
	baseURL := s.BTBaseURLOverride
	if baseURL == "" {
		baseURL = cfg.APIBaseURL
	}
	return basistheory.New(basistheory.Config{
		APIKey:        cfg.APIKey,
		BaseURL:       baseURL,
		WebhookKeyURL: cfg.WebhookKeyURL,
		ReadOnly:      s.Config != nil && s.Config.IsProviderReadOnly(),
	})
}

func (s *CheckoutVaultedCardService) charger(cfg *config.VaultedCardRailConfig) (*vaultedcard.Charger, error) {
	bt, err := s.btClient(cfg)
	if err != nil {
		return nil, err
	}
	gw := vaultedcard.GatewayConfig{
		SecurityKey:   cfg.GatewaySecurityKey,
		DirectPostURL: cfg.GatewayDirectPostURL,
	}
	if s.GatewayDirectPostURLOverride != "" {
		gw.DirectPostURL = s.GatewayDirectPostURLOverride
	}
	return vaultedcard.New(bt, gw), nil
}

// Process runs the vaulted_card one-time sale as a write-through intent,
// mirroring CheckoutNMISaleService.Process.
func (s *CheckoutVaultedCardService) Process(ctx context.Context, req *CheckoutRequest, user *UserIdentity, price *models.Price, product *models.Product, idempotencyKey string) (*CheckoutResponse, error) {
	const idempOp = "vaulted_card_sale"
	if s == nil || s.Intents == nil {
		return nil, errors.New("vaulted_card checkout not wired")
	}
	// PAN firewall (SAQ A): the engine accepts ONLY the token-intent handle.
	if err := RejectPANShapedFields(req); err != nil {
		return nil, err
	}
	intentID := strings.TrimSpace(req.BTTokenIntentID)
	if intentID == "" {
		return nil, errors.New("bt_token_intent_id is required for vaulted_card checkout (collect via BT Elements)")
	}
	if _, err := s.resolveConfig(ctx); err != nil {
		return nil, fmt.Errorf("vaulted_card rail is not configured: %w", err)
	}
	if _, err := customerIDFromUser(user.ID); err != nil {
		return nil, err
	}

	idempRec, alreadyExists, err := s.IdempotencyStore.Begin(ctx, idempOp, idempotencyKey)
	if err != nil {
		return nil, fmt.Errorf("idempotency check failed: %w", err)
	}
	if alreadyExists {
		switch idempRec.Status {
		case IdempotencyStatusSuccess:
			var cached checkoutSaleIdempotencyResult
			if err := json.Unmarshal(idempRec.Result, &cached); err == nil {
				return saleResponse(cached, "Purchase already completed"), nil
			}
			return &CheckoutResponse{Status: "success", Action: "new", Message: "Purchase already completed"}, nil
		case IdempotencyStatusPending:
			return nil, errors.New("checkout already in progress, please wait")
		case IdempotencyStatusFailed:
			// The durable intent below is the source of truth.
		}
	}

	tid, err := merchant.Require(ctx)
	if err != nil {
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, err
	}
	intent, err := s.Intents.EnqueueAndExecute(ctx, intents.EnqueueParams{
		MerchantID: tid.UUID(),
		Provider:   string(models.RailVaultedCard),
		IntentType: TypeVaultedCardSale,
		PriceID:    &price.ID,
		Payload: VaultedCardSalePayload{
			TokenIntentID: intentID,
			AmountMicros:  price.Amount,
			Currency:      price.Currency,
			Description:   fmt.Sprintf("Purchase: %s", product.DisplayName),
			UserID:        user.ID,
			PriceID:       price.ID,
			E2ERunID:      strings.TrimSpace(req.Metadata["e2e_run_id"]),
		},
		IdempotencyKey: VaultedCardSaleIdempotencyKey(idempotencyKey),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         intents.OriginUser,
		OriginReason:   "checkout one-time sale (vaulted_card)",
	})
	if err != nil {
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("post vaulted_card sale intent: %w", err)
	}

	switch intent.Status {
	case intents.StatusSucceeded:
		cached, derr := saleResultFromIntent(intent)
		if derr != nil {
			return nil, fmt.Errorf("sale succeeded but evidence unreadable: %w", derr)
		}
		payload, _ := json.Marshal(cached)
		_ = s.IdempotencyStore.Complete(ctx, idempOp, idempotencyKey, payload)
		return saleResponse(cached, "Purchase completed successfully"), nil
	case intents.StatusFailedTerminal:
		reason := "payment failed"
		if intent.LastFailureReason != nil && *intent.LastFailureReason != "" {
			reason = "payment failed: " + *intent.LastFailureReason
		}
		failErr := errors.New(reason)
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, failErr)
		return nil, failErr
	default:
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, ErrCheckoutProcessing)
		return nil, ErrCheckoutProcessing
	}
}

// --- intent handler ----------------------------------------------------------

// VaultedCardSaleIntentHandler: money-mover semantics mirror the NMI sale
// handler — re-executions verify at the GATEWAY first (the BT proxy has no
// idempotency), transport-ambiguous outcomes park as unknown_needs_verify,
// parsed declines are terminal with the verbatim code as evidence.
type VaultedCardSaleIntentHandler struct {
	Sale   *CheckoutVaultedCardService
	Policy intents.BackoffPolicy
}

func NewVaultedCardSaleIntentHandler(sale *CheckoutVaultedCardService) *VaultedCardSaleIntentHandler {
	return &VaultedCardSaleIntentHandler{Sale: sale, Policy: intents.DefaultBackoff}
}

func (h *VaultedCardSaleIntentHandler) Type() string { return TypeVaultedCardSale }
func (h *VaultedCardSaleIntentHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}
func (h *VaultedCardSaleIntentHandler) PrunePolicy() (keepPayload, keepEvidence bool) {
	return false, true
}

func (h *VaultedCardSaleIntentHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}

func decodeVaultedCardSalePayload(intent gen.OpenrailsRailIntent) (VaultedCardSalePayload, error) {
	var p VaultedCardSalePayload
	if len(intent.Payload) == 0 {
		return p, errors.New("vaulted_card sale intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode vaulted_card sale payload: %w", err)
	}
	if p.TokenIntentID == "" || p.AmountMicros <= 0 || p.Currency == "" || p.UserID == "" || p.PriceID == uuid.Nil {
		return p, errors.New("vaulted_card sale payload is incomplete")
	}
	return p, nil
}

// gatewayQueryClient builds the NMI query-leg client for verify-by-orderid.
func (h *VaultedCardSaleIntentHandler) gatewayQueryClient(cfg *config.VaultedCardRailConfig) (*nmi.NMIClient, error) {
	testMode := h.Sale.Config != nil && h.Sale.Config.IsTestMode()
	return nmi.NewClient(string(models.RailVaultedCard), &config.NMIProviderSettings{SecurityKey: cfg.GatewaySecurityKey}, testMode)
}

func (h *VaultedCardSaleIntentHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	if h.Sale == nil || h.Sale.PurchaseService == nil {
		return intents.Parked("vaulted_card checkout service not wired")
	}
	p, err := decodeVaultedCardSalePayload(intent)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	cfg, err := h.Sale.resolveConfig(ctx)
	if err != nil {
		return intents.Parked(fmt.Sprintf("vaulted_card rail not armed: %v", err))
	}
	orderID := nmiSaleIntentOrderID(intent.ID, p.E2ERunID)

	// Money mover: re-executions verify at the gateway before sending again.
	if intent.Attempts > 1 && !h.Sale.DisableGatewayVerify {
		client, cerr := h.gatewayQueryClient(cfg)
		if cerr != nil {
			return intents.Ambiguous("gateway verify client unavailable: " + cerr.Error())
		}
		txnID, found, verr := client.FindSuccessfulSaleByOrderID(ctx, orderID)
		if verr != nil {
			return intents.Ambiguous("pre-send verification read failed: " + verr.Error())
		}
		if found {
			return h.finalize(ctx, intent.MerchantID, cfg, p, orderID, txnID, true)
		}
	}

	amountCents, err := moneyutil.NativeToRailMinorExact(p.Currency, p.AmountMicros)
	if err != nil {
		return intents.Terminal("sale amount must be representable in whole cents: " + err.Error())
	}
	charger, err := h.Sale.charger(cfg)
	if err != nil {
		return intents.Parked("vaulted_card client build failed: " + err.Error())
	}

	// Prior anchor: an instrument with the intent's fingerprint may already be
	// anchored — reuse its unscheduled sequence. Fingerprint comes from the
	// intent read (loud not-found = expired intent, #651).
	bt, err := h.Sale.btClient(cfg)
	if err != nil {
		return intents.Parked("vaulted_card client build failed: " + err.Error())
	}
	tokenIntent, err := bt.GetTokenIntent(ctx, p.TokenIntentID)
	if err != nil {
		if basistheory.IsNotFound(err) {
			return intents.Terminal("bt token intent not found (intents expire after 24h; re-collect the card)")
		}
		return intents.Retryable("bt token intent read failed: " + err.Error())
	}
	priorRef, _ := h.priorAnchor(ctx, intent.MerchantID, tokenIntent.Fingerprint)

	citContext := charge.InitialOneTime()
	if priorRef != "" {
		citContext = charge.OneTimeReuse(priorRef)
	}
	res, err := charger.WithSource(vaultedcard.Source{TokenIntentID: p.TokenIntentID}).Charge(ctx, charge.Request{
		Instrument:  charge.Instrument{Rail: string(models.RailVaultedCard)},
		AmountMinor: moneyutil.Cents(amountCents),
		Currency:    p.Currency,
		Description: p.Description,
		OrderRef:    orderID,
		Context:     citContext,
	})
	if err != nil {
		switch {
		case errors.Is(err, basistheory.ErrProviderReadOnly):
			return intents.Parked("basistheory provider writes blocked (mode=readonly)")
		case basistheory.IsTransportAmbiguous(err) || nmi.IsTransportAmbiguous(err):
			return intents.Ambiguous("sale outcome unknown: " + err.Error())
		}
		if pe, ok := basistheory.IsBTProxyError(err); ok {
			if pe.Status == 429 {
				return intents.Retryable("bt proxy rate limited: " + err.Error())
			}
			// Pre-forward BT failure (auth/expression/config): operator-shaped,
			// NEVER a decline.
			return intents.Parked("bt proxy failed pre-forward: " + err.Error())
		}
		var vaultErr *nmi.CustomerVaultError
		if errors.As(err, &vaultErr) {
			// Transient gateway condition (420/421/430) — retry via the ledger.
			return intents.Retryable("gateway transient: " + err.Error())
		}
		return intents.Terminal("sale request rejected: " + err.Error())
	}
	if res.Declined {
		code := ""
		if res.FailureCode != nil {
			code = *res.FailureCode
		}
		// #796: the decline is a charge attempt — durable failed payments row.
		recordDeclinedAttempt(ctx, h.Sale.PurchaseService.PaymentService, DeclinedAttempt{
			UserID:                 p.UserID,
			PriceID:                p.PriceID,
			Rail:                   string(models.RailVaultedCard),
			SyntheticTransactionID: "vaulted_card_sale_declined:" + intent.ID.String(),
			AmountMicros:           p.AmountMicros,
			Currency:               p.Currency,
			FailureCode:            code,
			AttemptKind:            payments.AttemptInitial,
			TokenType:              res.TokenType,
		})
		msg := "declined"
		if res.FailureMessage != nil {
			msg = *res.FailureMessage
		}
		return intents.TerminalWithEvidence(msg, map[string]any{
			"declined":     true,
			"failure_code": code,
		})
	}
	return h.finalizeApproved(ctx, intent.MerchantID, cfg, p, orderID, res, tokenIntent)
}

// Verify resolves an ambiguous sale via the gateway query leg.
func (h *VaultedCardSaleIntentHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	p, err := decodeVaultedCardSalePayload(intent)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	cfg, err := h.Sale.resolveConfig(ctx)
	if err != nil {
		return intents.Ambiguous("vaulted_card rail not armed; cannot verify")
	}
	if h.Sale.DisableGatewayVerify {
		return intents.Ambiguous("gateway verify disabled; manual resolution required")
	}
	client, err := h.gatewayQueryClient(cfg)
	if err != nil {
		return intents.Ambiguous("gateway verify client unavailable: " + err.Error())
	}
	orderID := nmiSaleIntentOrderID(intent.ID, p.E2ERunID)
	txnID, found, err := client.FindSuccessfulSaleByOrderID(ctx, orderID)
	if err != nil {
		return intents.Ambiguous("provider read failed: " + err.Error())
	}
	if !found {
		return intents.Retryable("no successful sale found for order id; charge verified not executed")
	}
	return h.finalize(ctx, intent.MerchantID, cfg, p, orderID, txnID, true)
}

// priorAnchor finds an existing instrument by vault fingerprint and returns
// its unscheduled stored-credential anchor (+ the instrument row).
func (h *VaultedCardSaleIntentHandler) priorAnchor(ctx context.Context, merchantID uuid.UUID, fingerprint string) (string, *gen.OpenrailsPaymentMethod) {
	if h.Sale.DB == nil || strings.TrimSpace(fingerprint) == "" {
		return "", nil
	}
	row, err := h.Sale.DB.Gen(ctx).GetPaymentMethodByVaultFingerprint(ctx, gen.GetPaymentMethodByVaultFingerprintParams{
		MerchantID:       merchantID,
		Rail:             string(models.RailVaultedCard),
		VaultFingerprint: fingerprint,
	})
	if err != nil {
		if !db.IsNotFound(err) {
			log.WithContext(ctx).WithError(err).Warn("vaulted_card: fingerprint dedup lookup failed")
		}
		return "", nil
	}
	return strings.TrimSpace(row.StoredCredentialUnscheduledRef), &row
}

// finalize is the verified-existing leg (no fresh charge result): the charge
// landed at the gateway; conversion may still be pending.
func (h *VaultedCardSaleIntentHandler) finalize(ctx context.Context, merchantID uuid.UUID, cfg *config.VaultedCardRailConfig, p VaultedCardSalePayload, orderID, transactionID string, _ bool) intents.Outcome {
	res := charge.Result{
		TransactionID: transactionID,
		TokenType:     charge.TokenTypePANViaVault,
		CapturedRef:   transactionID,
	}
	bt, err := h.Sale.btClient(cfg)
	if err != nil {
		return intents.Parked("vaulted_card client build failed: " + err.Error())
	}
	tokenIntent, err := bt.GetTokenIntent(ctx, p.TokenIntentID)
	if err != nil && !basistheory.IsNotFound(err) {
		return intents.Ambiguous("bt token intent read failed during finalize: " + err.Error())
	}
	return h.finalizeApproved(ctx, merchantID, cfg, p, orderID, res, tokenIntent)
}

// finalizeApproved converts the intent to a durable token, writes/reuses the
// instrument row, persists the stored-credential anchor write-once, provisions
// an NT when armed (never load-bearing), and registers the purchase.
func (h *VaultedCardSaleIntentHandler) finalizeApproved(ctx context.Context, merchantID uuid.UUID, cfg *config.VaultedCardRailConfig, p VaultedCardSalePayload, orderID string, res charge.Result, tokenIntent *basistheory.TokenIntent) intents.Outcome {
	bt, err := h.Sale.btClient(cfg)
	if err != nil {
		return intents.Parked("vaulted_card client build failed: " + err.Error())
	}

	var token *basistheory.CardToken
	if tokenIntent != nil {
		token, err = bt.ConvertTokenIntent(ctx, p.TokenIntentID, basistheory.ConvertOpts{
			IdempotencyKey: "btconv:" + orderID,
			Deduplicate:    true,
		})
		if err != nil {
			if basistheory.IsNotFound(err) {
				// Intent expired inside the crash window. The charge HAPPENED —
				// register the money; the instrument needs re-collection (#657).
				log.WithContext(ctx).WithFields(log.Fields{
					"bt_token_intent_id": p.TokenIntentID, "order_id": orderID,
				}).Error("vaulted_card: token intent expired before conversion; purchase recorded WITHOUT a stored instrument (re-collect card)")
				token = nil
			} else if errors.Is(err, basistheory.ErrProviderReadOnly) {
				return intents.Parked("basistheory provider writes blocked (mode=readonly)")
			} else {
				// The charge happened; keep resolving until conversion lands.
				return intents.Ambiguous("sale charged, but token conversion failed: " + err.Error())
			}
		}
	}

	var instrumentID *uuid.UUID
	if token != nil {
		id, ierr := h.ensureInstrument(ctx, merchantID, p, token)
		if ierr != nil {
			return intents.Ambiguous("sale charged, but instrument write failed: " + ierr.Error())
		}
		instrumentID = id
		// Anchor the unscheduled sequence write-once (identical to nmidirect).
		if ref := strings.TrimSpace(res.CapturedRef); ref != "" && h.Sale.DB != nil {
			if _, cerr := h.Sale.DB.Gen(ctx).CaptureStoredCredentialRef(ctx, gen.CaptureStoredCredentialRefParams{
				MerchantID: merchantID,
				ID:         *id,
				Agreement:  string(charge.AgreementUnscheduled),
				Ref:        ref,
			}); cerr != nil {
				log.WithContext(ctx).WithError(cerr).Warn("vaulted_card: failed to persist stored-credential anchor (#297); next charge re-captures")
			}
		}
		// NT provisioning (B8): armed by config, idempotent per PAN, and never
		// load-bearing — any failure warns and the instrument stays pan_proxy.
		if cfg.NetworkTokens {
			h.provisionNetworkToken(ctx, bt, merchantID, *id, token.ID, orderID)
		}
	}

	metadata := map[string]any{"order_id": orderID, "bt_token_intent_id": p.TokenIntentID}
	if token != nil {
		metadata["bt_token_id"] = token.ID
	}
	if p.E2ERunID != "" {
		metadata["e2e_run_id"] = p.E2ERunID
	}
	result, err := h.Sale.PurchaseService.RegisterPurchase(ctx, &payments.RegisterPurchaseRequest{
		UserID:        p.UserID,
		PriceID:       p.PriceID,
		Rail:          string(models.RailVaultedCard),
		TransactionID: res.TransactionID,
		Amount:        p.AmountMicros,
		Currency:      p.Currency,
		Metadata:      metadata,
		AttemptKind:   payments.AttemptInitial,
		TokenType:     res.TokenType,
	})
	if err != nil {
		return intents.Ambiguous("sale charged, but purchase registration failed: " + err.Error())
	}
	evidence := map[string]any{
		nmiSaleEvidenceTransactionID: res.TransactionID,
		nmiSaleEvidencePaymentID:     result.PaymentID.String(),
	}
	if instrumentID != nil {
		evidence["payment_method_id"] = instrumentID.String()
	}
	if result.DelayedStart != nil {
		evidence[nmiSaleEvidenceDelayedStart] = result.DelayedStart.UTC().Format(time.RFC3339)
	}
	return intents.Succeeded(evidence)
}

// ensureInstrument writes (or reuses) the payment_methods row for a converted
// durable token.
func (h *VaultedCardSaleIntentHandler) ensureInstrument(ctx context.Context, merchantID uuid.UUID, p VaultedCardSalePayload, token *basistheory.CardToken) (*uuid.UUID, error) {
	if h.Sale.PaymentMethodService == nil {
		return nil, errors.New("payment method service not wired")
	}
	if existing, err := h.Sale.PaymentMethodService.GetByRailMethodRef(ctx, string(models.RailVaultedCard), token.ID); err == nil && existing != nil {
		return &existing.ID, nil
	} else if err != nil && !db.IsNotFound(err) && !errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) {
		return nil, err
	}
	customerID, err := customerIDFromUser(p.UserID)
	if err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	method := &models.PaymentMethod{
		ID:               uuidutil.NewV7(),
		CustomerID:       customerID,
		Rail:             models.RailVaultedCard,
		RailMethodRef:    token.ID,
		RebillDriver:     models.RebillDriverOpenRails,
		Custodian:        vaultedcard.Custodian,
		VaultFingerprint: token.Fingerprint,
		ChargeVia:        vaultedcard.ViaPANProxy,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	if token.Card != nil {
		method.LastFour = stringPtrIfSet(token.Card.Last4)
		method.CardType = stringPtrIfSet(token.Card.Brand)
		if token.Card.ExpirationMonth > 0 && token.Card.ExpirationYear > 0 {
			exp := fmt.Sprintf("%02d/%02d", token.Card.ExpirationMonth, token.Card.ExpirationYear%100)
			method.ExpiryDate = &exp
		}
	}
	if err := h.Sale.PaymentMethodService.Create(ctx, method); err != nil {
		// Concurrent write of the same token: reuse the surviving row.
		if existing, gerr := h.Sale.PaymentMethodService.GetByRailMethodRef(ctx, string(models.RailVaultedCard), token.ID); gerr == nil && existing != nil {
			return &existing.ID, nil
		}
		return nil, err
	}
	return &method.ID, nil
}

// provisionNetworkToken creates (or dedups onto) the card's network token and
// persists id/status/PAR. Failures warn — NT is never load-bearing on NMI.
func (h *VaultedCardSaleIntentHandler) provisionNetworkToken(ctx context.Context, bt *basistheory.Client, merchantID, instrumentID uuid.UUID, tokenID, orderID string) {
	nt, err := bt.CreateNetworkToken(ctx, basistheory.NetworkTokenRequest{
		TokenID:        tokenID,
		IdempotencyKey: "btnt:" + orderID,
	})
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("bt_token_id", tokenID).
			Warn("vaulted_card: network token provisioning failed; instrument stays pan_proxy (never load-bearing)")
		return
	}
	if h.Sale.DB == nil {
		return
	}
	if _, err := h.Sale.DB.Gen(ctx).SetPaymentMethodNetworkToken(ctx, gen.SetPaymentMethodNetworkTokenParams{
		MerchantID:         merchantID,
		ID:                 instrumentID,
		NetworkTokenID:     nt.ID,
		NetworkTokenStatus: nt.Status,
		NetworkTokenPar:    nt.PAR,
	}); err != nil {
		log.WithContext(ctx).WithError(err).Warn("vaulted_card: failed to persist network token on instrument")
	}
}

func stringPtrIfSet(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}
