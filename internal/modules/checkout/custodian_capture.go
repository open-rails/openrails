package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/crypto"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/internal/modules/paymentmethods"
	"github.com/open-rails/openrails/internal/railresolve"
)

var ErrCheckoutCaptureUnavailable = errors.New("custodian capture is unavailable")

// captureSealer seals the SDK authorization at rest under a key derived from
// the custodian's API key; rotating the key strands only open setups.
func captureSealer(custodian merchants.CustodianScope) (*crypto.Sealer, error) {
	sealer, err := crypto.NewSealer(custodian.Secret(custodians.SecretAPIKey), "checkout-capture/v1")
	if err != nil {
		return nil, ErrCheckoutCaptureUnavailable
	}
	return sealer, nil
}
func captureAAD(owner billing.MerchantID, id uuid.UUID, state models.CheckoutCapture) crypto.AAD {
	// Bind both the physical row and accepted authority; copying or editing
	// customer/custodian/session/expiry metadata cannot relocate a credential.
	parts, _ := json.Marshal([]string{id.String(), state.CustomerID.String(), state.PSPID.String(), state.CustodianID.String(), state.AccountID, state.Environment, state.ProfileID, state.PublicAPIKey, state.APIBaseURL, state.SDKURL, state.VendorCustomerID, state.VendorSessionID, state.ExpiresAt.UTC().Format(time.RFC3339Nano)})
	return crypto.SecretAAD(owner, "checkout_capture/v1/"+string(parts))
}
func captureTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func captureScopedID(domain string, owner billing.MerchantID, customer uuid.UUID, key string) uuid.UUID {
	input := []byte(domain + "\x00")
	merchantID := owner.UUID()
	for _, part := range [][]byte{merchantID[:], customer[:], []byte(key)} {
		input = binary.BigEndian.AppendUint64(input, uint64(len(part)))
		input = append(input, part...)
	}
	return uuid.NewHash(sha256.New(), uuid.NameSpaceURL, input, 8)
}
func captureSessionID(owner billing.MerchantID, customer uuid.UUID, key string) uuid.UUID {
	return captureScopedID("openrails/payment-method-setup/v1", owner, customer, key)
}
func captureCustomerReference(owner billing.MerchantID, customer uuid.UUID) string {
	return captureScopedID("openrails/custody-customer/v1", owner, customer, "").String()
}
func (s *CheckoutAttemptService) captureBinding(owner billing.MerchantID, psp merchants.PSPScope, custodian merchants.CustodianScope) (models.CheckoutCapture, error) {
	var empty models.CheckoutCapture
	if s.config == nil || s.config.HyperSwitch == nil || psp.Archived || psp.Rail != "nmi" || psp.CustodianID == nil || *psp.CustodianID != custodian.ID || custodian.Archived || custodian.Kind != models.CustodianHyperSwitch || custodian.Environment != psp.Environment || psp.Environment != config.ExpectedProviderEnvironment(config.IsTestMode(s.config)) {
		return empty, ErrCheckoutCaptureUnavailable
	}
	parsed, err := custodians.ParseSettings(custodian.Kind, custodian.Settings)
	if err != nil {
		return empty, ErrCheckoutCaptureUnavailable
	}
	return models.CheckoutCapture{MerchantID: owner.UUID(), PSPID: psp.ID, CustodianID: custodian.ID, AccountID: custodian.AccountID, Environment: custodian.Environment, ProfileID: parsed.ProfileID, PublicAPIKey: parsed.PublicAPIKey, APIBaseURL: strings.TrimRight(s.config.HyperSwitch.APIBaseURL, "/"), SDKURL: s.config.HyperSwitch.SDKURL}, nil
}

// captureAccounts is the PSP and the custodian holding its cards, as the
// merchant's configuration has them.
func (s *CheckoutAttemptService) captureAccounts(ctx context.Context, owner billing.MerchantID, pspID uuid.UUID) (merchants.PSPScope, merchants.CustodianScope, error) {
	svc := merchants.Of(s.db)
	if svc == nil {
		return merchants.PSPScope{}, merchants.CustodianScope{}, ErrCheckoutCaptureUnavailable
	}
	psp, ok, err := svc.PSPScopeByID(ctx, owner, pspID)
	if err != nil || !ok || psp.CustodianID == nil {
		return merchants.PSPScope{}, merchants.CustodianScope{}, ErrCheckoutCaptureUnavailable
	}
	custodian, ok, err := svc.CustodianScopeByID(ctx, owner, *psp.CustodianID)
	if err != nil || !ok {
		return merchants.PSPScope{}, merchants.CustodianScope{}, ErrCheckoutCaptureUnavailable
	}
	return psp, custodian, nil
}
func (s *CheckoutAttemptService) captureFromSession(ctx context.Context, session *models.CheckoutAttempt) (models.CheckoutCapture, []byte, error) {
	owner, err := merchant.Require(ctx)
	if err != nil {
		return models.CheckoutCapture{}, nil, err
	}
	raw, err := json.Marshal(session.RailState["capture"])
	if err != nil {
		return models.CheckoutCapture{}, nil, err
	}
	state, err := models.DecodeCheckoutCapture(raw, owner.UUID(), session.CustomerID, session.PspID, session.Status, session.ExpiresAt)
	return state, raw, err
}
func (s *CheckoutAttemptService) resolveCapture(ctx context.Context, pspID uuid.UUID) (models.CheckoutCapture, *hyperswitch.Client, *crypto.Sealer, error) {
	var state models.CheckoutCapture
	owner, err := merchant.Require(ctx)
	if err != nil {
		return state, nil, nil, err
	}
	if s.config == nil || s.config.HyperSwitch == nil {
		return state, nil, nil, ErrCheckoutCaptureUnavailable
	}
	psp, custodian, err := s.captureAccounts(ctx, owner, pspID)
	if err != nil {
		return state, nil, nil, err
	}
	if state, err = s.captureBinding(owner, psp, custodian); err != nil {
		return state, nil, nil, err
	}
	sealer, err := captureSealer(custodian)
	if err != nil {
		return state, nil, nil, err
	}
	client, err := railresolve.HyperSwitchClient(s.config, owner, custodian)
	return state, client, sealer, err
}
func sameCaptureAccount(a, b models.CheckoutCapture) bool {
	return a.MerchantID == b.MerchantID && a.PSPID == b.PSPID && a.CustodianID == b.CustodianID && a.AccountID == b.AccountID && a.Environment == b.Environment && a.ProfileID == b.ProfileID && a.PublicAPIKey == b.PublicAPIKey && a.APIBaseURL == b.APIBaseURL && a.SDKURL == b.SDKURL
}

// Contact data satisfies the vendor's customer schema. It never selects an
// account: only authenticated merchant/payer ids derive the exact reference.
func captureContact(req *CheckoutAttemptCreateRequest, user *UserIdentity) (string, string, error) {
	name := strings.TrimSpace(user.Username)
	if name == "" {
		name = strings.TrimSpace(req.Payment.NameOnCard)
	}
	email := strings.TrimSpace(req.Payment.Email)
	if user.Email != nil && strings.TrimSpace(*user.Email) != "" {
		email = strings.TrimSpace(*user.Email)
	}
	address, err := mail.ParseAddress(email)
	if name == "" || len(name) > 200 || strings.IndexFunc(name, unicode.IsControl) >= 0 || len(email) > 320 || err != nil || address.Name != "" || address.Address != email || cardguard.ContainsPAN(name) || cardguard.ContainsPAN(email) {
		return "", "", fmt.Errorf("%w: capture requires a valid contact email and name", ErrCheckoutAttemptValidation)
	}
	return name, email, nil
}

func (s *CheckoutAttemptService) createPaymentMethodSetup(ctx context.Context, req *CheckoutAttemptCreateRequest, user *UserIdentity) (*CheckoutAttemptResponse, error) {
	if err := rejectCheckoutAttemptPAN(req); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" || cardguard.ContainsPAN(req.IdempotencyKey) || req.Payment.PSPID == uuid.Nil {
		return nil, fmt.Errorf("%w: setup requires PSP id and Idempotency-Key", ErrCheckoutAttemptValidation)
	}
	payment := req.Payment
	payment.PSPID = uuid.Nil
	payment.Rail = ""
	payment.Email = ""
	payment.NameOnCard = ""
	if payment != (CheckoutAttemptPaymentRequest{}) || req.PriceID != "" || req.SuccessURL != "" || req.CancelURL != "" || (req.Payment.Rail != "" && req.Payment.Rail != "nmi") {
		return nil, fmt.Errorf("%w: setup accepts no purchase, token or redirect fields", ErrCheckoutAttemptValidation)
	}
	name, email, err := captureContact(req, user)
	if err != nil {
		return nil, err
	}
	customer, err := customerIDFromUser(user.ID)
	if err != nil {
		return nil, err
	}
	owner, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	canonical := *req
	canonical.Payment.Rail = "nmi"
	canonical.Payment.Email = email
	canonical.Payment.NameOnCard = name
	fingerprint := checkoutAttemptRequestFingerprintForRail(&canonical, user, "nmi")
	id := captureSessionID(owner, customer, req.IdempotencyKey)
	session, err := s.repo.GetByID(ctx, id)
	if err == nil {
		if session.CustomerID != customer || session.Mode != models.CheckoutAttemptModePaymentMethod || session.RailState[checkoutAttemptFingerprintKey] != fingerprint {
			return nil, ErrCheckoutAttemptConflict
		}
		if session.Status != models.CheckoutAttemptStatusCreated {
			return s.renderPaymentMethodSetup(ctx, session)
		}
	} else if !db.IsNotFound(err) {
		return nil, err
	}
	if err = s.requireProviderWrites(ctx); err != nil {
		return nil, err
	}
	pending, client, sealer, err := s.resolveCapture(ctx, req.Payment.PSPID)
	if err != nil {
		return nil, err
	}
	if err = client.CheckCaptureContract(ctx); err != nil {
		return nil, ErrCheckoutCaptureUnavailable
	}
	if session == nil {
		now := s.now().UTC().Truncate(time.Microsecond)
		pending.CustomerID = customer
		pending.ExpiresAt = now.Add(defaultCheckoutAttemptTTL)
		state := map[string]any{checkoutAttemptFingerprintKey: fingerprint, "capture": pending}
		raw, _ := json.Marshal(state)
		metadata, _ := json.Marshal(normalizeMetadata(req.Metadata))
		if err = db.EnsureCustomerRow(ctx, s.db.Qx(ctx), uuid.Nil, customer); err != nil {
			return nil, err
		}
		_, err = s.db.Gen(ctx).CreatePaymentMethodSetupSession(ctx, gen.CreatePaymentMethodSetupSessionParams{ID: id, MerchantID: owner.UUID(), CustomerID: customer, PspID: req.Payment.PSPID, ExpiresAt: new(pending.ExpiresAt), RailState: raw, Metadata: metadata, Now: now})
		if err != nil {
			return nil, err
		}
		session, err = s.repo.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		if session.CustomerID != customer || session.RailState[checkoutAttemptFingerprintKey] != fingerprint {
			return nil, ErrCheckoutAttemptConflict
		}
		if session.Status != models.CheckoutAttemptStatusCreated {
			return s.renderPaymentMethodSetup(ctx, session)
		}
	}
	accepted, previous, err := s.captureFromSession(ctx, session)
	if err != nil {
		return nil, err
	}
	if !sameCaptureAccount(accepted, pending) {
		return nil, ErrCheckoutAttemptConflict
	}
	if !s.now().Before(accepted.ExpiresAt) {
		return s.renderPaymentMethodSetup(ctx, session)
	}
	vendorCustomer, err := client.EnsureCustomer(ctx, captureCustomerReference(owner, customer), name, email)
	if err != nil {
		return nil, ErrCheckoutCaptureUnavailable
	}
	vendorSession, err := client.CreateSession(ctx, vendorCustomer.ID)
	if err != nil {
		return nil, ErrCheckoutCaptureUnavailable
	}
	accepted.VendorCustomerID = vendorCustomer.ID
	accepted.VendorSessionID = vendorSession.ID
	if vendorSession.ExpiresAt.Before(accepted.ExpiresAt) {
		accepted.ExpiresAt = vendorSession.ExpiresAt.UTC().Truncate(time.Microsecond)
	}
	accepted.SecretCiphertext, err = sealer.Seal(captureAAD(owner, id, accepted), []byte(vendorSession.SDKAuthorization))
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(accepted)
	_, err = s.db.Gen(ctx).AcceptPaymentMethodSetupSession(ctx, gen.AcceptPaymentMethodSetupSessionParams{ID: id, MerchantID: owner.UUID(), Capture: encoded, Previous: previous, ExpiresAt: new(accepted.ExpiresAt), Now: s.now().UTC()})
	if err != nil {
		return nil, err
	}
	// A competing preparation may have won; expose only the accepted row.
	session, err = s.repo.GetByID(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.renderPaymentMethodSetup(ctx, session)
}

func (s *CheckoutAttemptService) renderPaymentMethodSetup(ctx context.Context, session *models.CheckoutAttempt) (*CheckoutAttemptResponse, error) {
	owner, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	if s.isExpired(session) && !s.isTerminal(session.Status) {
		if err = s.MarkExpired(ctx, session.ID, "capture session expired"); err != nil {
			return nil, err
		}
		session, err = s.repo.GetByID(ctx, session.ID)
		if err != nil {
			return nil, err
		}
	}
	state, _, err := s.captureFromSession(ctx, session)
	if err != nil {
		return nil, err
	}
	response := s.sessionToResponse(session)
	if state.PaymentMethodID != uuid.Nil {
		method := billing.PaymentMethodID(state.PaymentMethodID)
		response.PaymentMethodID = &method
	}
	if session.Status != models.CheckoutAttemptStatusRequiresAction {
		return response, nil
	}
	if s.config == nil || s.config.HyperSwitch == nil || state.APIBaseURL != strings.TrimRight(s.config.HyperSwitch.APIBaseURL, "/") || state.SDKURL != s.config.HyperSwitch.SDKURL {
		return nil, ErrCheckoutCaptureUnavailable
	}
	current, client, sealer, err := s.resolveCapture(ctx, session.PspID)
	if err != nil || !sameCaptureAccount(state, current) {
		return nil, ErrCheckoutCaptureUnavailable
	}
	if err = client.CheckCaptureContract(ctx); err != nil {
		return nil, ErrCheckoutCaptureUnavailable
	}
	secret, err := sealer.Open(captureAAD(owner, session.ID, state), state.SecretCiphertext)
	if err != nil {
		return nil, ErrCheckoutCaptureUnavailable
	}
	if !s.now().Before(state.ExpiresAt) {
		return nil, ErrCheckoutAttemptExpired
	}
	response.Capture = &CustodianCaptureAction{Kind: models.CustodianHyperSwitch, CustodianID: state.CustodianID, SessionID: state.VendorSessionID, CustomerID: state.VendorCustomerID, APIBaseURL: state.APIBaseURL, SDKURL: state.SDKURL, PublicAPIKey: state.PublicAPIKey, SDKAuthorization: string(secret), ExpiresAt: state.ExpiresAt}
	return response, nil
}

func (s *CheckoutAttemptService) confirmPaymentMethodSetup(ctx context.Context, session *models.CheckoutAttempt, req *CheckoutAttemptConfirmRequest) (*CheckoutAttemptResponse, error) {
	if req == nil || req.Payment.Capture == nil {
		return nil, fmt.Errorf("%w: capture reference required", ErrCheckoutAttemptValidation)
	}
	reference := req.Payment.Capture
	state, _, err := s.captureFromSession(ctx, session)
	if err != nil {
		return nil, err
	}
	if reference.CustodianID != state.CustodianID || reference.SessionID != state.VendorSessionID || reference.Token == "" || cardguard.ContainsPAN(reference.Token) || req.Payment.Signature != "" || req.Payment.Wallet != "" || (req.Payment.Rail != "" && req.Payment.Rail != "nmi") {
		return nil, ErrCheckoutAttemptConflict
	}
	tokenHash := captureTokenHash(reference.Token)
	if session.Status == models.CheckoutAttemptStatusSucceeded {
		if state.AcceptedTokenHash != tokenHash {
			return nil, ErrCheckoutAttemptConflict
		}
		return s.renderPaymentMethodSetup(ctx, session)
	}
	if s.isExpired(session) || session.Status != models.CheckoutAttemptStatusRequiresAction {
		return nil, ErrCheckoutAttemptExpired
	}
	current, client, _, err := s.resolveCapture(ctx, session.PspID)
	if err != nil {
		return nil, err
	}
	if !sameCaptureAccount(state, current) {
		return nil, ErrCheckoutAttemptConflict
	}
	if err := client.CheckCaptureContract(ctx); err != nil {
		return nil, ErrCheckoutCaptureUnavailable
	}
	vendorSession, err := client.GetSession(ctx, state.VendorSessionID, state.VendorCustomerID)
	if err != nil || !vendorSession.OwnsToken(reference.Token) || !s.now().Before(vendorSession.ExpiresAt.Time) {
		return nil, ErrCheckoutAttemptConflict
	}
	method, err := client.GetMethod(ctx, reference.Token, state.VendorCustomerID)
	if err != nil {
		return nil, ErrCheckoutAttemptConflict
	}
	owner, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	err = s.db.MerchantTx(ctx, func(c context.Context, tx pgx.Tx) error {
		queries := s.db.NewWithPgxTx(tx).Gen(c)
		if _, err := queries.LockCustomerForSpend(c, gen.LockCustomerForSpendParams{MerchantID: owner.UUID(), ID: session.CustomerID}); err != nil {
			return err
		}
		handle := paymentmethods.CustodianHandle{Custodian: state.CustodianID, Method: method.ID}
		if err := paymentmethods.LockCustodianHandles(c, queries, owner.UUID(), handle); err != nil {
			return err
		}
		if err := paymentmethods.RequireCustodianHandleAvailable(c, queries, owner.UUID(), handle); err != nil {
			if errors.Is(err, paymentmethods.ErrPaymentMethodDeleteProcessing) || errors.Is(err, paymentmethods.ErrPaymentMethodDeleteUnsafe) {
				return ErrCheckoutAttemptConflict
			}
			return err
		}
		row, err := queries.GetPaymentMethodSetupSessionForUpdate(c, gen.GetPaymentMethodSetupSessionForUpdateParams{ID: session.ID, MerchantID: owner.UUID()})
		if err != nil {
			return err
		}
		locked, err := models.CheckoutAttemptFromGen(row)
		if err != nil {
			return err
		}
		canonical, _, err := s.captureFromSession(c, locked)
		if err != nil {
			return err
		}
		if canonical.VendorSessionID != state.VendorSessionID || !sameCaptureAccount(canonical, state) {
			return ErrCheckoutAttemptConflict
		}
		if locked.Status == models.CheckoutAttemptStatusSucceeded {
			if canonical.AcceptedTokenHash == tokenHash {
				return nil
			}
			return ErrCheckoutAttemptConflict
		}
		if locked.Status != models.CheckoutAttemptStatusRequiresAction || !s.now().Before(canonical.ExpiresAt) {
			return ErrCheckoutAttemptExpired
		}
		if _, err := queries.GetCheckoutCaptureAccountsForShare(c, gen.GetCheckoutCaptureAccountsForShareParams{MerchantID: owner.UUID(), PspID: locked.PspID, CustodianID: canonical.CustodianID}); err != nil {
			if db.IsNotFound(err) {
				return ErrCheckoutAttemptConflict
			}
			return err
		}
		psp, custodian, err := s.captureAccounts(c, owner, locked.PspID)
		if err != nil {
			return ErrCheckoutAttemptConflict
		}
		current, err := s.captureBinding(owner, psp, custodian)
		if err != nil || !sameCaptureAccount(canonical, current) {
			return ErrCheckoutAttemptConflict
		}
		brand, last4, month, year := models.ParseCard(method.Data.Card.Brand, method.Data.Card.Last4, method.MaskedExpiry()).Columns()
		attached, err := queries.AttachCapturedPaymentMethod(c, gen.AttachCapturedPaymentMethodParams{ID: uuid.New(), MerchantID: owner.UUID(), CustomerID: locked.CustomerID, CustodianID: new(canonical.CustodianID), VendorCustomerID: canonical.VendorCustomerID, VendorMethodID: method.ID, CardBrand: brand, CardLast4: last4, CardExpMonth: month, CardExpYear: year, Now: s.now().UTC()})
		if err != nil {
			return err
		}
		canonical.PaymentMethodID = attached.ID
		canonical.VendorMethodID = method.ID
		canonical.AcceptedTokenHash = tokenHash
		canonical.SecretCiphertext = ""
		raw, _ := json.Marshal(canonical)
		affected, err := queries.CompletePaymentMethodSetupSession(c, gen.CompletePaymentMethodSetupSessionParams{ID: locked.ID, MerchantID: owner.UUID(), Capture: raw, Now: s.now().UTC()})
		if err != nil {
			return err
		}
		if affected != 1 {
			return ErrCheckoutAttemptConflict
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	session, err = s.repo.GetByID(ctx, session.ID)
	if err != nil {
		return nil, err
	}
	return s.renderPaymentMethodSetup(ctx, session)
}
