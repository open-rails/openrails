package payment

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

var ErrCardCaptureUnavailable = errors.New("custodian capture is unavailable")

// captureSealer seals the SDK authorization at rest under a key derived from
// the custodian's API key; rotating the key strands only open setups.
func captureSealer(custodian merchants.CustodianScope) (*crypto.Sealer, error) {
	sealer, err := crypto.NewSealer(custodian.Secret(custodians.SecretAPIKey), "purchase-capture/v1")
	if err != nil {
		return nil, ErrCardCaptureUnavailable
	}
	return sealer, nil
}
func captureAAD(owner billing.MerchantID, id uuid.UUID, state models.CardCapture) crypto.AAD {
	// Bind both the physical row and accepted authority; copying or editing
	// customer/custodian/session/expiry metadata cannot relocate a credential.
	parts, _ := json.Marshal([]string{id.String(), state.CustomerID.String(), state.PSPID.String(), state.CustodianID.String(), state.AccountID, state.Environment, state.ProfileID, state.PublicAPIKey, state.APIBaseURL, state.SDKURL, state.VendorCustomerID, state.VendorSessionID, state.ExpiresAt.UTC().Format(time.RFC3339Nano)})
	return crypto.SecretAAD(owner, "card_capture/v1/"+string(parts))
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
func captureMethodID(owner billing.MerchantID, customer uuid.UUID, key string) uuid.UUID {
	return captureScopedID("openrails/payment-method-setup/v1", owner, customer, key)
}
func captureCustomerReference(owner billing.MerchantID, customer uuid.UUID) string {
	return captureScopedID("openrails/custody-customer/v1", owner, customer, "").String()
}
func (s *OrderAttemptService) captureBinding(owner billing.MerchantID, psp merchants.PSPScope, custodian merchants.CustodianScope) (models.CardCapture, error) {
	var empty models.CardCapture
	if s.config == nil || s.config.HyperSwitch == nil || psp.Archived || psp.Rail != "nmi" || psp.CustodianID == nil || *psp.CustodianID != custodian.ID || custodian.Archived || custodian.Kind != models.CustodianHyperSwitch || custodian.Environment != config.EnvironmentClass(psp.Environment) {
		return empty, ErrCardCaptureUnavailable
	}
	parsed, err := custodians.ParseSettings(custodian.Kind, custodian.Settings)
	if err != nil {
		return empty, ErrCardCaptureUnavailable
	}
	return models.CardCapture{MerchantID: owner.UUID(), PSPID: psp.ID, CustodianID: custodian.ID, AccountID: custodian.AccountID, Environment: custodian.Environment, ProfileID: parsed.ProfileID, PublicAPIKey: parsed.PublicAPIKey, APIBaseURL: strings.TrimRight(s.config.HyperSwitch.APIBaseURL, "/"), SDKURL: s.config.HyperSwitch.SDKURL}, nil
}

// captureAccounts is the PSP and the custodian holding its cards, as the
// merchant's configuration has them.
func (s *OrderAttemptService) captureAccounts(ctx context.Context, owner billing.MerchantID, pspID uuid.UUID) (merchants.PSPScope, merchants.CustodianScope, error) {
	svc := merchants.Of(s.db)
	if svc == nil {
		return merchants.PSPScope{}, merchants.CustodianScope{}, ErrCardCaptureUnavailable
	}
	psp, ok, err := svc.PSPScopeByID(ctx, owner, pspID)
	if err != nil || !ok || psp.CustodianID == nil {
		return merchants.PSPScope{}, merchants.CustodianScope{}, ErrCardCaptureUnavailable
	}
	custodian, ok, err := svc.CustodianScopeByID(ctx, owner, *psp.CustodianID)
	if err != nil || !ok {
		return merchants.PSPScope{}, merchants.CustodianScope{}, ErrCardCaptureUnavailable
	}
	return psp, custodian, nil
}
func (s *OrderAttemptService) resolveCapture(ctx context.Context, pspID uuid.UUID) (models.CardCapture, *hyperswitch.Client, *crypto.Sealer, error) {
	var state models.CardCapture
	owner, err := merchant.Require(ctx)
	if err != nil {
		return state, nil, nil, err
	}
	if s.config == nil || s.config.HyperSwitch == nil {
		return state, nil, nil, ErrCardCaptureUnavailable
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
func sameCaptureAccount(a, b models.CardCapture) bool {
	return a.MerchantID == b.MerchantID && a.PSPID == b.PSPID && a.CustodianID == b.CustodianID && a.AccountID == b.AccountID && a.Environment == b.Environment && a.ProfileID == b.ProfileID && a.PublicAPIKey == b.PublicAPIKey && a.APIBaseURL == b.APIBaseURL && a.SDKURL == b.SDKURL
}

// captureContact uses authenticated contact facts when available; the request
// supplies billing contact details only, never the vendor customer identity.
func captureContact(details *billing.BillingDetails, user *UserIdentity) (string, string, error) {
	name, email := strings.TrimSpace(user.Username), ""
	if details != nil {
		if name == "" && details.Name != nil {
			name = strings.TrimSpace(*details.Name)
		}
		if details.Email != nil {
			email = strings.TrimSpace(*details.Email)
		}
	}
	if user.Email != nil && strings.TrimSpace(*user.Email) != "" {
		email = strings.TrimSpace(*user.Email)
	}
	address, err := mail.ParseAddress(email)
	if name == "" || len(name) > 200 || strings.IndexFunc(name, unicode.IsControl) >= 0 || len(email) > 320 || err != nil || address.Name != "" || address.Address != email || cardguard.ContainsPAN(name) || cardguard.ContainsPAN(email) {
		return "", "", fmt.Errorf("%w: capture requires a valid contact email and name", ErrOrderAttemptValidation)
	}
	return name, email, nil
}

func captureFromMethod(owner billing.MerchantID, method *models.PaymentMethod) (models.CardCapture, error) {
	var state models.CardCapture
	if err := json.Unmarshal(method.Capture, &state); err != nil {
		return state, err
	}
	status := models.OrderAttemptStatusRequiresAction
	switch {
	case state.PaymentMethodID != uuid.Nil:
		status = models.OrderAttemptStatusSucceeded
	case method.Status == "removed":
		status = models.OrderAttemptStatusExpired
	}
	decoded, err := models.DecodeCardCapture(method.Capture, owner.UUID(), method.CustomerID, state.PSPID, status, &state.ExpiresAt)
	if err != nil {
		return state, err
	}
	if method.Status == "requires_action" && (method.Custodian != models.CustodianHyperSwitch || method.CustodianID == nil || *method.CustodianID != state.CustodianID || method.PspID != nil || method.SetupRef != state.VendorSessionID) {
		return state, models.ErrCardCaptureBinding
	}
	return decoded, nil
}

// SaveCustodianCard opens hosted card fields for this customer's pending
// payment method. Only the first accepted vendor session is exposed. Competing
// vendor preparations may expire unused; none can charge a card.
func (s *OrderAttemptService) SaveCustodianCard(ctx context.Context, req billing.CreatePaymentMethodParams, key string, user *UserIdentity, metadata map[string]any) (SavedCard, error) {
	if strings.TrimSpace(key) == "" || cardguard.ContainsPAN(key) || req.Token != "" || req.Card != nil || req.PSPID.IsZero() {
		return SavedCard{}, fmt.Errorf("%w: custodian capture requires PSP id and Idempotency-Key, without a token or card", ErrOrderAttemptValidation)
	}
	name, email, err := captureContact(req.BillingDetails, user)
	if err != nil {
		return SavedCard{}, err
	}
	customer, err := customerIDFromUser(user.ID)
	if err != nil {
		return SavedCard{}, err
	}
	owner, err := merchant.Require(ctx)
	if err != nil {
		return SavedCard{}, err
	}
	request, _ := json.Marshal([]any{req.PSPID, name, email, req.BillingDetails})
	fingerprint := captureTokenHash(string(request))
	id := captureMethodID(owner, customer, key)
	repo := paymentmethods.NewPaymentMethodRepo(s.db)
	if method, err := repo.GetByID(ctx, id); err == nil {
		state, err := captureFromMethod(owner, method)
		if err != nil {
			return SavedCard{}, err
		}
		if method.CustomerID != customer || state.RequestFingerprint != fingerprint {
			return SavedCard{}, ErrOrderAttemptConflict
		}
		return s.renderCapturedCard(ctx, method)
	} else if !errors.Is(err, paymentmethods.ErrPaymentMethodNotFound) {
		return SavedCard{}, err
	}
	if err = s.requireProviderWrites(ctx); err != nil {
		return SavedCard{}, err
	}
	state, client, sealer, err := s.resolveCapture(ctx, req.PSPID.UUID())
	if err != nil {
		return SavedCard{}, err
	}
	if err = client.CheckCaptureContract(ctx); err != nil {
		return SavedCard{}, ErrCardCaptureUnavailable
	}
	vendorCustomer, err := client.EnsureCustomer(ctx, captureCustomerReference(owner, customer), name, email)
	if err != nil {
		return SavedCard{}, ErrCardCaptureUnavailable
	}
	vendorSession, err := client.CreateSession(ctx, vendorCustomer.ID)
	if err != nil {
		return SavedCard{}, ErrCardCaptureUnavailable
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	state.CustomerID, state.RequestFingerprint = customer, fingerprint
	state.ExpiresAt = now.Add(defaultOrderAttemptTTL)
	if vendorSession.ExpiresAt.Before(state.ExpiresAt) {
		state.ExpiresAt = vendorSession.ExpiresAt.UTC().Truncate(time.Microsecond)
	}
	state.VendorCustomerID, state.VendorSessionID = vendorCustomer.ID, vendorSession.ID
	state.SecretCiphertext, err = sealer.Seal(captureAAD(owner, id, state), []byte(vendorSession.SDKAuthorization))
	if err != nil {
		return SavedCard{}, err
	}
	raw, _ := json.Marshal(state)
	details, _ := json.Marshal(metadata)
	if err = db.EnsureCustomerRow(ctx, s.db.Qx(ctx), owner.UUID(), customer); err != nil {
		return SavedCard{}, err
	}
	_, err = s.db.Gen(ctx).CreateCapturedPaymentMethod(ctx, gen.CreateCapturedPaymentMethodParams{ID: id, MerchantID: owner.UUID(), CustomerID: customer, CustodianID: state.CustodianID, SetupRef: state.VendorSessionID, Capture: raw, Metadata: details, Now: now})
	if err != nil {
		return SavedCard{}, err
	}
	method, err := repo.GetByID(ctx, id)
	if err != nil {
		return SavedCard{}, err
	}
	accepted, err := captureFromMethod(owner, method)
	if err != nil {
		return SavedCard{}, err
	}
	if method.CustomerID != customer || accepted.RequestFingerprint != fingerprint {
		return SavedCard{}, ErrOrderAttemptConflict
	}
	return s.renderCapturedCard(ctx, method)
}

func (s *OrderAttemptService) renderCapturedCard(ctx context.Context, method *models.PaymentMethod) (SavedCard, error) {
	owner, err := merchant.Require(ctx)
	if err != nil {
		return SavedCard{}, err
	}
	state, err := captureFromMethod(owner, method)
	if err != nil {
		return SavedCard{}, err
	}
	if state.PaymentMethodID != uuid.Nil {
		if state.PaymentMethodID != method.ID {
			method, err = paymentmethods.NewPaymentMethodRepo(s.db).GetByID(ctx, state.PaymentMethodID)
			if err != nil {
				return SavedCard{}, err
			}
		}
		return SavedCard{Method: method}, nil
	}
	if method.Status != "requires_action" || !s.now().Before(state.ExpiresAt) {
		_, err = s.db.Gen(ctx).ExpireCapturedPaymentMethod(ctx, gen.ExpireCapturedPaymentMethodParams{MerchantID: owner.UUID(), ID: method.ID, Now: s.now()})
		if err != nil {
			return SavedCard{}, err
		}
		return SavedCard{}, ErrOrderAttemptExpired
	}
	current, client, sealer, err := s.resolveCapture(ctx, state.PSPID)
	if err != nil || !sameCaptureAccount(state, current) {
		return SavedCard{}, ErrCardCaptureUnavailable
	}
	if err = client.CheckCaptureContract(ctx); err != nil {
		return SavedCard{}, ErrCardCaptureUnavailable
	}
	secret, err := sealer.Open(captureAAD(owner, method.ID, state), state.SecretCiphertext)
	if err != nil {
		return SavedCard{}, ErrCardCaptureUnavailable
	}
	psp := billing.PSPID(state.PSPID)
	return SavedCard{Method: method, NextAction: &billing.NextAction{Type: "capture", PSPID: &psp, Transactions: []string{}, Payload: map[string]string{
		"kind": models.CustodianHyperSwitch, "custodian_id": state.CustodianID.String(), "session_id": state.VendorSessionID, "customer_id": state.VendorCustomerID,
		"api_base_url": state.APIBaseURL, "sdk_url": state.SDKURL, "public_api_key": state.PublicAPIKey, "sdk_authorization": string(secret), "expires_at": state.ExpiresAt.Format(time.RFC3339Nano),
	}}}, nil
}

// ConfirmCustodianCard reads the token back from its exact vendor session,
// then attaches the instrument under the customer and custodian handle locks.
// It never charges or creates a card-on-file mandate.
func (s *OrderAttemptService) ConfirmCustodianCard(ctx context.Context, id uuid.UUID, user *UserIdentity, reference *billing.CustodianCaptureReference) (SavedCard, error) {
	if reference == nil {
		return SavedCard{}, fmt.Errorf("%w: capture reference required", ErrOrderAttemptValidation)
	}
	owner, err := merchant.Require(ctx)
	if err != nil {
		return SavedCard{}, err
	}
	repo := paymentmethods.NewPaymentMethodRepo(s.db)
	method, err := repo.GetByID(ctx, id)
	if err != nil {
		return SavedCard{}, err
	}
	if method.CustomerID.String() != user.ID {
		return SavedCard{}, paymentmethods.ErrPaymentMethodNotFound
	}
	state, err := captureFromMethod(owner, method)
	if err != nil {
		return SavedCard{}, err
	}
	if reference.CustodianID != state.CustodianID.String() || reference.SessionID != state.VendorSessionID || reference.Token == "" || cardguard.ContainsPAN(reference.Token) {
		return SavedCard{}, ErrOrderAttemptConflict
	}
	tokenHash := captureTokenHash(reference.Token)
	if state.PaymentMethodID != uuid.Nil {
		if state.AcceptedTokenHash != tokenHash {
			return SavedCard{}, ErrOrderAttemptConflict
		}
		return s.renderCapturedCard(ctx, method)
	}
	if method.Status != "requires_action" || !s.now().Before(state.ExpiresAt) {
		return s.renderCapturedCard(ctx, method)
	}
	current, client, _, err := s.resolveCapture(ctx, state.PSPID)
	if err != nil {
		return SavedCard{}, err
	}
	if !sameCaptureAccount(state, current) {
		return SavedCard{}, ErrOrderAttemptConflict
	}
	if err = client.CheckCaptureContract(ctx); err != nil {
		return SavedCard{}, ErrCardCaptureUnavailable
	}
	vendorSession, err := client.GetSession(ctx, state.VendorSessionID, state.VendorCustomerID)
	if err != nil || !vendorSession.OwnsToken(reference.Token) || !s.now().Before(vendorSession.ExpiresAt.Time) {
		return SavedCard{}, ErrOrderAttemptConflict
	}
	instrument, err := client.GetMethod(ctx, reference.Token, state.VendorCustomerID)
	if err != nil {
		return SavedCard{}, ErrOrderAttemptConflict
	}
	err = s.db.MerchantTx(ctx, func(c context.Context, tx pgx.Tx) error {
		q := s.db.NewWithPgxTx(tx).Gen(c)
		if _, err := q.LockCustomerForSpend(c, gen.LockCustomerForSpendParams{MerchantID: owner.UUID(), ID: method.CustomerID}); err != nil {
			return err
		}
		handle := paymentmethods.CustodianHandle{Custodian: state.CustodianID, Method: instrument.ID}
		if err := paymentmethods.LockCustodianHandles(c, q, owner.UUID(), handle); err != nil {
			return err
		}
		if err := paymentmethods.RequireCustodianHandleAvailable(c, q, owner.UUID(), handle); err != nil {
			if errors.Is(err, paymentmethods.ErrPaymentMethodDeleteProcessing) || errors.Is(err, paymentmethods.ErrPaymentMethodDeleteUnsafe) {
				return ErrOrderAttemptConflict
			}
			return err
		}
		row, err := q.GetCapturedPaymentMethodForUpdate(c, gen.GetCapturedPaymentMethodForUpdateParams{MerchantID: owner.UUID(), ID: id})
		if err != nil {
			return err
		}
		locked, err := models.PaymentMethodFromGen(row)
		if err != nil {
			return err
		}
		canonical, err := captureFromMethod(owner, locked)
		if err != nil {
			return err
		}
		if canonical.VendorSessionID != state.VendorSessionID || !sameCaptureAccount(canonical, state) {
			return ErrOrderAttemptConflict
		}
		if canonical.PaymentMethodID != uuid.Nil {
			if canonical.AcceptedTokenHash == tokenHash {
				return nil
			}
			return ErrOrderAttemptConflict
		}
		if locked.Status != "requires_action" || !s.now().Before(canonical.ExpiresAt) {
			return ErrOrderAttemptExpired
		}
		if _, err = q.GetCardCaptureAccountsForShare(c, gen.GetCardCaptureAccountsForShareParams{MerchantID: owner.UUID(), PspID: canonical.PSPID, CustodianID: canonical.CustodianID}); err != nil {
			return err
		}
		psp, custodian, err := s.captureAccounts(c, owner, canonical.PSPID)
		if err != nil {
			return ErrOrderAttemptConflict
		}
		current, err := s.captureBinding(owner, psp, custodian)
		if err != nil || !sameCaptureAccount(canonical, current) {
			return ErrOrderAttemptConflict
		}
		canonical.PaymentMethodID = id
		var replacement *uuid.UUID
		existing, err := q.GetCapturedInstrument(c, gen.GetCapturedInstrumentParams{MerchantID: owner.UUID(), CustodianID: canonical.CustodianID, VendorCustomerID: canonical.VendorCustomerID, VendorMethodID: instrument.ID})
		if err == nil {
			if existing.CustomerID != method.CustomerID {
				return ErrOrderAttemptConflict
			}
			if existing.Status != "active" || existing.ParkReason != nil {
				return ErrPaymentMethodStale
			}
			canonical.PaymentMethodID = existing.ID
			replacement = &existing.ID
		} else if !db.IsNotFound(err) {
			return err
		}
		canonical.VendorMethodID, canonical.AcceptedTokenHash, canonical.SecretCiphertext = instrument.ID, tokenHash, ""
		raw, _ := json.Marshal(canonical)
		brand, last4, month, year := models.ParseCard(instrument.Data.Card.Brand, instrument.Data.Card.Last4, instrument.MaskedExpiry()).Columns()
		changed, err := q.CompleteCapturedPaymentMethod(c, gen.CompleteCapturedPaymentMethodParams{ID: id, MerchantID: owner.UUID(), Replacement: replacement, Capture: raw, VendorCustomerID: canonical.VendorCustomerID, VendorMethodID: instrument.ID, CardBrand: brand, CardLast4: last4, CardExpMonth: month, CardExpYear: year, Now: s.now().UTC()})
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrOrderAttemptConflict
		}
		return nil
	})
	if err != nil {
		return SavedCard{}, err
	}
	method, err = repo.GetByID(ctx, id)
	if err != nil {
		return SavedCard{}, err
	}
	return s.renderCapturedCard(ctx, method)
}
