package checkout

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/cardguard"
	"github.com/open-rails/openrails/internal/crypto"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/integrations/hyperswitch"
	"github.com/open-rails/openrails/internal/merchants"
	"github.com/open-rails/openrails/pkg/merchant"
)

var ErrCheckoutCaptureUnavailable = errors.New("custodian capture is unavailable")

type captureEncryption struct {
	once   sync.Once
	cipher *crypto.Encryptor
	err    error
}

func (s *CheckoutSessionService) SetMerchantSecretStore(store merchants.MerchantSecretReader) {
	s.captureSecrets = store
}
func (s *CheckoutSessionService) captureEncryptor() (*crypto.Encryptor, error) {
	s.captureEncryption.once.Do(func() {
		if s.config == nil || s.config.Encryption == nil || s.db == nil {
			s.captureEncryption.err = ErrCheckoutCaptureUnavailable
			return
		}
		store, err := crypto.NewDBDEKStore(s.db.DataPool())
		if err != nil {
			s.captureEncryption.err = err
			return
		}
		s.captureEncryption.cipher, s.captureEncryption.err = crypto.NewEncryptor(s.config.Encryption.MasterKey, store)
		if s.captureEncryption.err == nil && !s.captureEncryption.cipher.Enabled() {
			s.captureEncryption.err = crypto.ErrEncryptionDisabled
		}
	})
	return s.captureEncryption.cipher, s.captureEncryption.err
}
func captureAAD(owner merchant.ID, id uuid.UUID, state models.CheckoutCapture) crypto.AAD {
	// Bind both the physical row and accepted authority; copying or editing
	// customer/custodian/session/expiry metadata cannot relocate a credential.
	parts, _ := json.Marshal([]string{id.String(), state.CustomerID.String(), state.PSPID.String(), state.CustodianID.String(), state.AccountID, state.Environment, state.ProfileID, state.PublicAPIKey, state.APIBaseURL, state.SDKURL, state.VendorCustomerID, state.VendorSessionID, state.ExpiresAt.UTC().Format(time.RFC3339Nano)})
	return crypto.SecretAAD(owner, "checkout_capture/v1/"+string(parts))
}
func captureTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func captureScopedID(domain string, owner merchant.ID, customer uuid.UUID, key string) uuid.UUID {
	input := []byte(domain + "\x00")
	merchantID := owner.UUID()
	for _, part := range [][]byte{merchantID[:], customer[:], []byte(key)} {
		input = binary.BigEndian.AppendUint64(input, uint64(len(part)))
		input = append(input, part...)
	}
	return uuid.NewHash(sha256.New(), uuid.NameSpaceURL, input, 8)
}
func captureSessionID(owner merchant.ID, customer uuid.UUID, key string) uuid.UUID {
	return captureScopedID("openrails/payment-method-setup/v1", owner, customer, key)
}
func captureCustomerReference(owner merchant.ID, customer uuid.UUID) string {
	return captureScopedID("openrails/custody-customer/v1", owner, customer, "").String()
}
func (s *CheckoutSessionService) captureBinding(owner merchant.ID, psp gen.OpenrailsPsp, custodian gen.OpenrailsCustodian) (models.CheckoutCapture, error) {
	var empty models.CheckoutCapture
	if s.config == nil || s.config.HyperSwitch == nil || psp.MerchantID != owner.UUID() || psp.Archived || psp.Rail != "nmi" || psp.CustodianID == nil || *psp.CustodianID != custodian.ID || custodian.MerchantID != owner.UUID() || custodian.Archived || custodian.Kind != models.CustodianHyperSwitch || custodian.Environment != psp.Environment || psp.Environment != config.ExpectedProviderEnvironment(s.config.IsTestMode()) {
		return empty, ErrCheckoutCaptureUnavailable
	}
	var settings map[string]any
	if json.Unmarshal(custodian.Settings, &settings) != nil {
		return empty, ErrCheckoutCaptureUnavailable
	}
	parsed, err := custodians.ParseSettings(custodian.Kind, settings)
	if err != nil {
		return empty, ErrCheckoutCaptureUnavailable
	}
	return models.CheckoutCapture{MerchantID: owner.UUID(), PSPID: psp.ID, CustodianID: custodian.ID, AccountID: custodian.AccountID, Environment: custodian.Environment, ProfileID: parsed.ProfileID, PublicAPIKey: parsed.PublicAPIKey, APIBaseURL: strings.TrimRight(s.config.HyperSwitch.APIBaseURL, "/"), SDKURL: s.config.HyperSwitch.SDKURL}, nil
}
func (s *CheckoutSessionService) captureFromSession(ctx context.Context, session *models.CheckoutSession) (models.CheckoutCapture, []byte, error) {
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
func (s *CheckoutSessionService) resolveCapture(ctx context.Context, pspID uuid.UUID) (models.CheckoutCapture, *hyperswitch.Client, error) {
	var state models.CheckoutCapture
	owner, err := merchant.Require(ctx)
	if err != nil {
		return state, nil, err
	}
	if s.config == nil || s.config.HyperSwitch == nil || s.captureSecrets == nil {
		return state, nil, ErrCheckoutCaptureUnavailable
	}
	if _, err = s.captureEncryptor(); err != nil {
		return state, nil, ErrCheckoutCaptureUnavailable
	}
	psp, err := s.db.Gen(ctx).GetPSP(ctx, pspID)
	if err != nil || psp.MerchantID != owner.UUID() || psp.Archived || psp.Rail != "nmi" || psp.CustodianID == nil {
		return state, nil, ErrCheckoutCaptureUnavailable
	}
	custodian, err := s.db.Gen(ctx).GetCustodian(ctx, *psp.CustodianID)
	if err != nil || custodian.MerchantID != owner.UUID() || custodian.Archived || custodian.Kind != models.CustodianHyperSwitch || custodian.Environment != psp.Environment {
		return state, nil, ErrCheckoutCaptureUnavailable
	}
	state, err = s.captureBinding(owner, psp, custodian)
	if err != nil {
		return state, nil, err
	}
	var versions map[string]int
	if json.Unmarshal(custodian.CredentialVersions, &versions) != nil || versions[custodians.SecretAPIKey] < 0 {
		return state, nil, ErrCheckoutCaptureUnavailable
	}
	name, err := merchants.CustodianSecretName(custodian.Kind, custodian.Environment, custodian.AccountID, custodians.SecretAPIKey)
	if err != nil {
		return state, nil, err
	}
	secret, err := merchants.ReadSecretRef(ctx, s.captureSecrets, owner, merchants.SecretRef{Name: name, MinVersion: versions[custodians.SecretAPIKey]})
	if err != nil {
		return state, nil, ErrCheckoutCaptureUnavailable
	}
	client, err := hyperswitch.New(hyperswitch.Config{BaseURL: state.APIBaseURL, MerchantID: state.AccountID, ProfileID: state.ProfileID, APIKey: hyperswitch.Secret(secret.Value), ReadOnly: s.config.IsProviderReadOnly()})
	return state, client, err
}
func sameCaptureAccount(a, b models.CheckoutCapture) bool {
	return a.MerchantID == b.MerchantID && a.PSPID == b.PSPID && a.CustodianID == b.CustodianID && a.AccountID == b.AccountID && a.Environment == b.Environment && a.ProfileID == b.ProfileID && a.PublicAPIKey == b.PublicAPIKey && a.APIBaseURL == b.APIBaseURL && a.SDKURL == b.SDKURL
}

func (s *CheckoutSessionService) createPaymentMethodSetup(ctx context.Context, req *CheckoutSessionCreateRequest, user *UserIdentity) (*CheckoutSessionResponse, error) {
	if err := rejectCheckoutSessionPAN(req); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" || cardguard.ContainsPAN(req.IdempotencyKey) || req.Payment.PSPID == uuid.Nil {
		return nil, fmt.Errorf("%w: setup requires PSP id and Idempotency-Key", ErrCheckoutSessionValidation)
	}
	payment := req.Payment
	payment.PSPID = uuid.Nil
	payment.Rail = ""
	if payment != (CheckoutSessionPaymentRequest{}) || req.PriceID != "" || req.SubscriptionID != "" || req.NewPriceID != "" || req.SuccessURL != "" || req.CancelURL != "" || (req.Payment.Rail != "" && req.Payment.Rail != "nmi") {
		return nil, fmt.Errorf("%w: setup accepts no purchase, token or redirect fields", ErrCheckoutSessionValidation)
	}
	if user.Email == nil || strings.TrimSpace(*user.Email) == "" || strings.TrimSpace(user.Username) == "" {
		return nil, fmt.Errorf("%w: capture requires verified customer email and name", ErrCheckoutSessionValidation)
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
	baseFingerprint := checkoutSessionRequestFingerprintForRail(&canonical, user, "nmi")
	body, _ := json.Marshal([]any{baseFingerprint, user.Username, user.Email})
	bodyDigest := sha256.Sum256(body)
	fingerprint := hex.EncodeToString(bodyDigest[:])
	id := captureSessionID(owner, customer, req.IdempotencyKey)
	session, err := s.repo.GetByID(ctx, id)
	if err == nil {
		if session.CustomerID != customer || session.Mode != models.CheckoutSessionModePaymentMethod || session.RailState[checkoutSessionFingerprintKey] != fingerprint {
			return nil, ErrCheckoutSessionConflict
		}
		if session.Status != models.CheckoutSessionStatusCreated {
			return s.renderPaymentMethodSetup(ctx, session)
		}
	} else if !db.IsNotFound(err) {
		return nil, err
	}
	if err = s.requireProviderWrites(); err != nil {
		return nil, err
	}
	pending, client, err := s.resolveCapture(ctx, req.Payment.PSPID)
	if err != nil {
		return nil, err
	}
	if err = client.CheckCaptureContract(ctx); err != nil {
		return nil, ErrCheckoutCaptureUnavailable
	}
	if session == nil {
		now := s.now().UTC().Truncate(time.Microsecond)
		pending.CustomerID = customer
		pending.ExpiresAt = now.Add(defaultCheckoutSessionTTL)
		state := map[string]any{checkoutSessionFingerprintKey: fingerprint, "capture": pending}
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
		if session.CustomerID != customer || session.RailState[checkoutSessionFingerprintKey] != fingerprint {
			return nil, ErrCheckoutSessionConflict
		}
		if session.Status != models.CheckoutSessionStatusCreated {
			return s.renderPaymentMethodSetup(ctx, session)
		}
	}
	accepted, previous, err := s.captureFromSession(ctx, session)
	if err != nil {
		return nil, err
	}
	if !sameCaptureAccount(accepted, pending) {
		return nil, ErrCheckoutSessionConflict
	}
	if !s.now().Before(accepted.ExpiresAt) {
		return s.renderPaymentMethodSetup(ctx, session)
	}
	vendorCustomer, err := client.EnsureCustomer(ctx, captureCustomerReference(owner, customer), user.Username, *user.Email)
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
	cipher, err := s.captureEncryptor()
	if err != nil {
		return nil, err
	}
	accepted.SecretCiphertext, err = cipher.Encrypt(ctx, owner, captureAAD(owner, id, accepted), []byte(vendorSession.SDKAuthorization))
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

func (s *CheckoutSessionService) renderPaymentMethodSetup(ctx context.Context, session *models.CheckoutSession) (*CheckoutSessionResponse, error) {
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
		method := openrails.PaymentMethodID(state.PaymentMethodID)
		response.PaymentMethodID = &method
	}
	if session.Status != models.CheckoutSessionStatusRequiresAction {
		return response, nil
	}
	if s.config == nil || s.config.HyperSwitch == nil || state.APIBaseURL != strings.TrimRight(s.config.HyperSwitch.APIBaseURL, "/") || state.SDKURL != s.config.HyperSwitch.SDKURL {
		return nil, ErrCheckoutCaptureUnavailable
	}
	current, client, err := s.resolveCapture(ctx, session.PspID)
	if err != nil || !sameCaptureAccount(state, current) {
		return nil, ErrCheckoutCaptureUnavailable
	}
	if err = client.CheckCaptureContract(ctx); err != nil {
		return nil, ErrCheckoutCaptureUnavailable
	}
	cipher, err := s.captureEncryptor()
	if err != nil {
		return nil, err
	}
	secret, err := cipher.Decrypt(ctx, owner, captureAAD(owner, session.ID, state), state.SecretCiphertext)
	if err != nil {
		return nil, ErrCheckoutCaptureUnavailable
	}
	if !s.now().Before(state.ExpiresAt) {
		return nil, ErrCheckoutSessionExpired
	}
	response.Capture = &openrails.CustodianCaptureAction{Kind: models.CustodianHyperSwitch, CustodianID: state.CustodianID, SessionID: state.VendorSessionID, CustomerID: state.VendorCustomerID, APIBaseURL: state.APIBaseURL, SDKURL: state.SDKURL, PublicAPIKey: state.PublicAPIKey, SDKAuthorization: string(secret), ExpiresAt: state.ExpiresAt}
	return response, nil
}

func (s *CheckoutSessionService) confirmPaymentMethodSetup(ctx context.Context, session *models.CheckoutSession, req *CheckoutSessionConfirmRequest) (*CheckoutSessionResponse, error) {
	if req == nil || req.Payment.Capture == nil {
		return nil, fmt.Errorf("%w: capture reference required", ErrCheckoutSessionValidation)
	}
	reference := req.Payment.Capture
	state, _, err := s.captureFromSession(ctx, session)
	if err != nil {
		return nil, err
	}
	if reference.CustodianID != state.CustodianID || reference.SessionID != state.VendorSessionID || reference.Token == "" || cardguard.ContainsPAN(reference.Token) || req.Payment.Signature != "" || req.Payment.Wallet != "" || (req.Payment.Rail != "" && req.Payment.Rail != "nmi") {
		return nil, ErrCheckoutSessionConflict
	}
	tokenHash := captureTokenHash(reference.Token)
	if session.Status == models.CheckoutSessionStatusSucceeded {
		if state.AcceptedTokenHash != tokenHash {
			return nil, ErrCheckoutSessionConflict
		}
		return s.renderPaymentMethodSetup(ctx, session)
	}
	if s.isExpired(session) || session.Status != models.CheckoutSessionStatusRequiresAction {
		return nil, ErrCheckoutSessionExpired
	}
	current, client, err := s.resolveCapture(ctx, session.PspID)
	if err != nil {
		return nil, err
	}
	if !sameCaptureAccount(state, current) {
		return nil, ErrCheckoutSessionConflict
	}
	vendorSession, err := client.GetSession(ctx, state.VendorSessionID, state.VendorCustomerID)
	if err != nil || !vendorSession.OwnsToken(reference.Token) || !s.now().Before(vendorSession.ExpiresAt.Time) {
		return nil, ErrCheckoutSessionConflict
	}
	method, err := client.GetMethod(ctx, reference.Token, state.VendorCustomerID)
	if err != nil {
		return nil, ErrCheckoutSessionConflict
	}
	owner, err := merchant.Require(ctx)
	if err != nil {
		return nil, err
	}
	err = s.db.MerchantTx(ctx, func(c context.Context, tx pgx.Tx) error {
		queries := s.db.NewWithPgxTx(tx).Gen(c)
		row, err := queries.GetPaymentMethodSetupSessionForUpdate(c, gen.GetPaymentMethodSetupSessionForUpdateParams{ID: session.ID, MerchantID: owner.UUID()})
		if err != nil {
			return err
		}
		locked, err := models.CheckoutSessionFromGen(row)
		if err != nil {
			return err
		}
		canonical, _, err := s.captureFromSession(c, locked)
		if err != nil {
			return err
		}
		if canonical.VendorSessionID != state.VendorSessionID || !sameCaptureAccount(canonical, state) {
			return ErrCheckoutSessionConflict
		}
		if locked.Status == models.CheckoutSessionStatusSucceeded {
			if canonical.AcceptedTokenHash == tokenHash {
				return nil
			}
			return ErrCheckoutSessionConflict
		}
		if locked.Status != models.CheckoutSessionStatusRequiresAction || !s.now().Before(canonical.ExpiresAt) {
			return ErrCheckoutSessionExpired
		}
		accounts, err := queries.GetCheckoutCaptureAccountsForShare(c, gen.GetCheckoutCaptureAccountsForShareParams{MerchantID: owner.UUID(), PspID: locked.PspID})
		if err != nil {
			if db.IsNotFound(err) {
				return ErrCheckoutSessionConflict
			}
			return err
		}
		current, err := s.captureBinding(owner, accounts.OpenrailsPsp, accounts.OpenrailsCustodian)
		if err != nil || !sameCaptureAccount(canonical, current) {
			return ErrCheckoutSessionConflict
		}
		expiry := method.MaskedExpiry()
		attached, err := queries.AttachCapturedPaymentMethod(c, gen.AttachCapturedPaymentMethodParams{ID: uuid.New(), MerchantID: owner.UUID(), CustomerID: locked.CustomerID, PspID: locked.PspID, CustodianID: new(canonical.CustodianID), VendorCustomerID: canonical.VendorCustomerID, VendorMethodID: method.ID, LastFour: new(method.Data.Card.Last4), CardType: new(method.Data.Card.Brand), ExpiryDate: new(expiry), Now: s.now().UTC()})
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
			return ErrCheckoutSessionConflict
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
