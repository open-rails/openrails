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
	"github.com/open-rails/openrails/internal/modules/payments/rails/nmiproxy"
	"github.com/open-rails/openrails/internal/railresolve"
	"github.com/open-rails/openrails/internal/shared/moneyutil"
	"github.com/open-rails/openrails/internal/shared/uuidutil"
	"github.com/open-rails/openrails/pkg/merchant"
)

// Custodian-held card checkout collection (#795 B5): the browser tokenizes
// into a Basis Theory token INTENT (Elements; the PAN goes browser->custodian,
// SAQ A); the engine accepts ONLY {bt_token_intent_id}. The CIT charges the
// INTENT (CVC present), converts to a durable token in-request on approval,
// writes the instrument row, and persists the stored-credential anchor exactly
// like the nmidirect paths. NT provisioning rides behind the merchant flag and
// is never load-bearing.
//
// The RAIL here is NMI (or#879): the custodian holds the card, the PSP's own
// gateway charges it.

// TypeCustodianSale is the write-through checkout sale intent (#674) for a
// custodian-held card. The NMI order id derives from the intent id, so the
// verify leg answers "did THIS sale charge?" against the GATEWAY (the proxy
// has no idempotency — the intents log + NMI dup detection are the safety).
const TypeCustodianSale = "custodian_sale"

func CustodianSaleIdempotencyKey(checkoutIdempotencyKey string) string {
	return TypeCustodianSale + ":" + strings.TrimSpace(checkoutIdempotencyKey)
}

// CustodianSalePayload carries everything Execute/Verify need without the
// originating request. NEVER a PAN — the intent id is the only card handle.
type CustodianSalePayload struct {
	TokenIntentID string    `json:"bt_token_intent_id"`
	AmountMicros  int64     `json:"amount_micros"`
	Currency      string    `json:"currency"`
	Description   string    `json:"description"`
	UserID        string    `json:"user_id"`
	PriceID       uuid.UUID `json:"price_id"`
	E2ERunID      string    `json:"e2e_run_id,omitempty"`
}

// CheckoutCustodianSaleService runs one-time sales on custodian-held cards.
type CheckoutCustodianSaleService struct {
	DB                   *db.DB
	PurchaseService      *CheckoutPurchaseService
	PaymentMethodService custodianInstrumentStore
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

type custodianInstrumentStore interface {
	Create(ctx context.Context, method *models.PaymentMethod) error
	GetByRailMethodRef(ctx context.Context, provider, methodRef string) (*models.PaymentMethod, error)
}

// custodialPSP is the resolved arrangement one custodian sale charges through:
// the PSP's own gateway credentials plus the custodian that holds the card.
type custodialPSP struct {
	Custody            *config.CustodianConfig
	GatewaySecurityKey string
	// GatewayDirectPostURL: "" = the NMI client default.
	GatewayDirectPostURL string
}

// resolveConfig arms the ctx merchant's custodian-held-card credentials (#788):
// the NMI PSP that charges, and the custodian it references (or#879/or#880).
func (s *CheckoutCustodianSaleService) resolveConfig(ctx context.Context) (*custodialPSP, error) {
	if s.Rails == nil {
		return nil, errors.New("rail resolution not configured")
	}
	rc, err := s.Rails.RailConfig(ctx, string(models.RailNMI), "")
	if err != nil {
		return nil, err
	}
	if rc.Custody == nil || rc.Custody.Custodian == "" || rc.Custody.Custodian == models.CustodianPSP {
		return nil, errors.New("nmi psp declares no third-party custodian; custodian checkout is not armed")
	}
	if rc.NMI == nil || strings.TrimSpace(rc.NMI.SecurityKey) == "" {
		return nil, errors.New("nmi psp resolved without a gateway security key")
	}
	return &custodialPSP{Custody: rc.Custody, GatewaySecurityKey: rc.NMI.SecurityKey}, nil
}

func (s *CheckoutCustodianSaleService) btClient(cfg *custodialPSP) (*basistheory.Client, error) {
	baseURL := s.BTBaseURLOverride
	if baseURL == "" {
		baseURL = cfg.Custody.APIBaseURL
	}
	return basistheory.New(basistheory.Config{
		APIKey:        cfg.Custody.APIKey,
		BaseURL:       baseURL,
		WebhookKeyURL: cfg.Custody.WebhookKeyURL,
		ReadOnly:      s.Config != nil && s.Config.IsProviderReadOnly(),
	})
}

func (s *CheckoutCustodianSaleService) charger(cfg *custodialPSP) (*nmiproxy.Charger, error) {
	bt, err := s.btClient(cfg)
	if err != nil {
		return nil, err
	}
	gw := nmiproxy.GatewayConfig{
		SecurityKey:   cfg.GatewaySecurityKey,
		DirectPostURL: cfg.GatewayDirectPostURL,
	}
	if s.GatewayDirectPostURLOverride != "" {
		gw.DirectPostURL = s.GatewayDirectPostURLOverride
	}
	return nmiproxy.New(bt, gw), nil
}

// Process runs the custodian-held-card one-time sale as a write-through intent,
// mirroring CheckoutNMISaleService.Process.
func (s *CheckoutCustodianSaleService) Process(ctx context.Context, req *CheckoutRequest, user *UserIdentity, price *models.Price, product *models.Product, idempotencyKey string) (*CheckoutResponse, error) {
	const idempOp = "custodian_sale"
	if s == nil || s.Intents == nil {
		return nil, errors.New("custodian checkout not wired")
	}
	// PAN firewall (SAQ A): the engine accepts ONLY the token-intent handle.
	if err := RejectPANShapedFields(req); err != nil {
		return nil, err
	}
	intentID := strings.TrimSpace(req.BTTokenIntentID)
	if intentID == "" {
		return nil, errors.New("bt_token_intent_id is required for custodian-held card checkout (collect via BT Elements)")
	}
	if _, err := s.resolveConfig(ctx); err != nil {
		return nil, fmt.Errorf("custodian checkout is not configured: %w", err)
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
		Provider:   string(models.RailNMI),
		IntentType: TypeCustodianSale,
		PriceID:    &price.ID,
		Payload: CustodianSalePayload{
			TokenIntentID: intentID,
			AmountMicros:  price.Amount,
			Currency:      price.Currency,
			Description:   fmt.Sprintf("Purchase: %s", product.DisplayName),
			UserID:        user.ID,
			PriceID:       price.ID,
			E2ERunID:      strings.TrimSpace(req.Metadata["e2e_run_id"]),
		},
		IdempotencyKey: CustodianSaleIdempotencyKey(idempotencyKey),
		NextAttemptAt:  time.Now().UTC(),
		Origin:         intents.OriginUser,
		OriginReason:   "checkout one-time sale (custodian-held card)",
	})
	if err != nil {
		_ = s.IdempotencyStore.Fail(ctx, idempOp, idempotencyKey, err)
		return nil, fmt.Errorf("post custodian sale intent: %w", err)
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

// CustodianSaleIntentHandler: money-mover semantics mirror the NMI sale
// handler — re-executions verify at the GATEWAY first (the BT proxy has no
// idempotency), transport-ambiguous outcomes park as unknown_needs_verify,
// parsed declines are terminal with the verbatim code as evidence.
type CustodianSaleIntentHandler struct {
	Sale   *CheckoutCustodianSaleService
	Policy intents.BackoffPolicy
}

func NewCustodianSaleIntentHandler(sale *CheckoutCustodianSaleService) *CustodianSaleIntentHandler {
	return &CustodianSaleIntentHandler{Sale: sale, Policy: intents.DefaultBackoff}
}

func (h *CustodianSaleIntentHandler) Type() string { return TypeCustodianSale }
func (h *CustodianSaleIntentHandler) Backoff(attempts int32) time.Duration {
	return h.Policy.Delay(attempts)
}
func (h *CustodianSaleIntentHandler) PrunePolicy() (keepPayload, keepEvidence bool) {
	return false, true
}

func (h *CustodianSaleIntentHandler) CheckRelevance(context.Context, gen.OpenrailsRailIntent) (intents.Relevance, error) {
	return intents.StillRelevant(), nil
}

func decodeCustodianSalePayload(intent gen.OpenrailsRailIntent) (CustodianSalePayload, error) {
	var p CustodianSalePayload
	if len(intent.Payload) == 0 {
		return p, errors.New("custodian sale intent has no payload")
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return p, fmt.Errorf("decode custodian sale payload: %w", err)
	}
	if p.TokenIntentID == "" || p.AmountMicros <= 0 || p.Currency == "" || p.UserID == "" || p.PriceID == uuid.Nil {
		return p, errors.New("custodian sale payload is incomplete")
	}
	return p, nil
}

// gatewayQueryClient builds the NMI query-leg client for verify-by-orderid.
func (h *CustodianSaleIntentHandler) gatewayQueryClient(cfg *custodialPSP) (*nmi.NMIClient, error) {
	testMode := h.Sale.Config != nil && h.Sale.Config.IsTestMode()
	return nmi.NewClient(string(models.RailNMI), &config.NMIProviderSettings{SecurityKey: cfg.GatewaySecurityKey}, testMode)
}

func (h *CustodianSaleIntentHandler) Execute(ctx context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	if h.Sale == nil || h.Sale.PurchaseService == nil {
		return intents.Parked("custodian checkout service not wired")
	}
	p, err := decodeCustodianSalePayload(intent)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	cfg, err := h.Sale.resolveConfig(ctx)
	if err != nil {
		return intents.Parked(fmt.Sprintf("custodian checkout not armed: %v", err))
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
		return intents.Parked("custodian client build failed: " + err.Error())
	}

	// Prior anchor: an instrument with the intent's fingerprint may already be
	// anchored — reuse its unscheduled sequence. Fingerprint comes from the
	// intent read (loud not-found = expired intent, #651).
	bt, err := h.Sale.btClient(cfg)
	if err != nil {
		return intents.Parked("custodian client build failed: " + err.Error())
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
	res, err := charger.WithSource(nmiproxy.Source{TokenIntentID: p.TokenIntentID}).Charge(ctx, charge.Request{
		Instrument:  charge.Instrument{Rail: nmiproxy.Rail},
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
			Rail:                   nmiproxy.Rail,
			SyntheticTransactionID: "custodian_sale_declined:" + intent.ID.String(),
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
func (h *CustodianSaleIntentHandler) Verify(ctx context.Context, intent gen.OpenrailsRailIntent) intents.Outcome {
	p, err := decodeCustodianSalePayload(intent)
	if err != nil {
		return intents.Terminal(err.Error())
	}
	cfg, err := h.Sale.resolveConfig(ctx)
	if err != nil {
		return intents.Ambiguous("custodian checkout not armed; cannot verify")
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

// priorAnchor finds an existing instrument by the custodian's PAN fingerprint
// and returns its unscheduled stored-credential anchor (+ the instrument row).
func (h *CustodianSaleIntentHandler) priorAnchor(ctx context.Context, merchantID uuid.UUID, fingerprint string) (string, *gen.OpenrailsPaymentMethod) {
	if h.Sale.DB == nil || strings.TrimSpace(fingerprint) == "" {
		return "", nil
	}
	row, err := h.Sale.DB.Gen(ctx).GetPaymentMethodByFingerprint(ctx, gen.GetPaymentMethodByFingerprintParams{
		MerchantID:  merchantID,
		Custodian:   nmiproxy.Custodian,
		Fingerprint: fingerprint,
	})
	if err != nil {
		if !db.IsNotFound(err) {
			log.WithContext(ctx).WithError(err).Warn("custodian sale: fingerprint dedup lookup failed")
		}
		return "", nil
	}
	return strings.TrimSpace(row.StoredCredentialUnscheduledRef), &row
}

// finalize is the verified-existing leg (no fresh charge result): the charge
// landed at the gateway; conversion may still be pending.
func (h *CustodianSaleIntentHandler) finalize(ctx context.Context, merchantID uuid.UUID, cfg *custodialPSP, p CustodianSalePayload, orderID, transactionID string, _ bool) intents.Outcome {
	res := charge.Result{
		TransactionID: transactionID,
		TokenType:     charge.TokenTypePANViaProxy,
		CapturedRef:   transactionID,
	}
	bt, err := h.Sale.btClient(cfg)
	if err != nil {
		return intents.Parked("custodian client build failed: " + err.Error())
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
func (h *CustodianSaleIntentHandler) finalizeApproved(ctx context.Context, merchantID uuid.UUID, cfg *custodialPSP, p CustodianSalePayload, orderID string, res charge.Result, tokenIntent *basistheory.TokenIntent) intents.Outcome {
	bt, err := h.Sale.btClient(cfg)
	if err != nil {
		return intents.Parked("custodian client build failed: " + err.Error())
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
				}).Error("custodian sale: token intent expired before conversion; purchase recorded WITHOUT a stored instrument (re-collect card)")
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
				log.WithContext(ctx).WithError(cerr).Warn("custodian sale: failed to persist stored-credential anchor (#297); next charge re-captures")
			}
		}
		// NT provisioning (B8): armed by config, idempotent per PAN, and never
		// load-bearing — any failure warns and the instrument stays pan_proxy.
		if cfg.Custody.NetworkTokens {
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
		Rail:          nmiproxy.Rail,
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
func (h *CustodianSaleIntentHandler) ensureInstrument(ctx context.Context, merchantID uuid.UUID, p CustodianSalePayload, token *basistheory.CardToken) (*uuid.UUID, error) {
	if h.Sale.PaymentMethodService == nil {
		return nil, errors.New("payment method service not wired")
	}
	if existing, err := h.Sale.PaymentMethodService.GetByRailMethodRef(ctx, nmiproxy.Rail, token.ID); err == nil && existing != nil {
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
		ID:            uuidutil.NewV7(),
		CustomerID:    customerID,
		Rail:          models.RailNMI,
		RailMethodRef: token.ID,
		RebillDriver:  models.RebillDriverOpenRails,
		Custodian:     nmiproxy.Custodian,
		Fingerprint:   token.Fingerprint,
		ChargeVia:     nmiproxy.ViaPANProxy,
		CreatedAt:     now,
		UpdatedAt:     now,
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
		if existing, gerr := h.Sale.PaymentMethodService.GetByRailMethodRef(ctx, nmiproxy.Rail, token.ID); gerr == nil && existing != nil {
			return &existing.ID, nil
		}
		return nil, err
	}
	return &method.ID, nil
}

// provisionNetworkToken creates (or dedups onto) the card's network token and
// persists id/status/PAR. Failures warn — NT is never load-bearing on NMI.
func (h *CustodianSaleIntentHandler) provisionNetworkToken(ctx context.Context, bt *basistheory.Client, merchantID, instrumentID uuid.UUID, tokenID, orderID string) {
	nt, err := bt.CreateNetworkToken(ctx, basistheory.NetworkTokenRequest{
		TokenID:        tokenID,
		IdempotencyKey: "btnt:" + orderID,
	})
	if err != nil {
		log.WithContext(ctx).WithError(err).WithField("bt_token_id", tokenID).
			Warn("custodian sale: network token provisioning failed; instrument stays pan_proxy (never load-bearing)")
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
		log.WithContext(ctx).WithError(err).Warn("custodian sale: failed to persist network token on instrument")
	}
}

func stringPtrIfSet(v string) *string {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	return &v
}
