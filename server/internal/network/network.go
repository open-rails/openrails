// Package network is the customer network of openrails.dev: an account of
// the server's own AuthKit linked to merchants' customers. A link is made
// only when the merchant's side (the customer's credential there) and the
// account's side (a fresh network sign-in whose ID token answers a
// challenge) are proven in one exchange; never by an email match.
package network

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/iam"
	log "github.com/sirupsen/logrus"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/abusestate"
	"github.com/open-rails/openrails/internal/billingauth"
	"github.com/open-rails/openrails/internal/customernetwork"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/merchant"
	"github.com/open-rails/openrails/internal/shared/apperr"
)

const (
	// TermsAgreement is the AuthKit agreement an account accepts to join the
	// network; a server whose AuthKit declares it runs the network.
	TermsAgreement = "network-terms"
	// ScopeLink is the consent a merchant's client needs to link the
	// merchant's own customer to the account (the connect path).
	ScopeLink = "openrails:link"
	// CheckoutClientID is the checkout page's first-party client, which the
	// server declares: its ID tokens prove the network side of a checkout
	// link.
	CheckoutClientID = "openrails-checkout"
	// ChallengeTTL is how long a link challenge can be redeemed.
	ChallengeTTL = 5 * time.Minute
)

// Enabled reports whether an AuthKit configuration runs the network.
func Enabled(cfg authkit.Config) bool {
	return slices.ContainsFunc(cfg.Agreements, func(a authkit.AgreementConfig) bool { return a.Key == TermsAgreement })
}

// Service is the customer network.
type Service struct {
	db    *db.DB
	ak    *authkit.Client
	store *abusestate.Store
	now   func() time.Time
}

var _ customernetwork.Network = (*Service)(nil)

// New is the network over the engine's database, the server's AuthKit and
// the short-lived store challenges live in.
func New(database *db.DB, ak *authkit.Client, store *abusestate.Store, now func() time.Time) (*Service, error) {
	if database == nil || ak == nil || store == nil {
		return nil, errors.New("network: database, AuthKit and the abuse store are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{db: database, ak: ak, store: store, now: now}, nil
}

var (
	errChallengeInvalid = apperr.New(http.StatusBadRequest, billing.CodeNetworkLinkChallengeInvalid, "the link challenge is unknown, expired or spent, or the ID token does not answer it")
	errLinkConflict     = apperr.New(http.StatusConflict, billing.CodeNetworkLinkConflict, "the customer is linked to another openrails.dev account")
	errAccountLinked    = apperr.New(http.StatusConflict, billing.CodeNetworkAccountLinked, "the openrails.dev account is linked to another customer of this merchant")
	errTermsRequired    = apperr.New(http.StatusForbidden, billing.CodeNetworkTermsRequired, "the openrails.dev account has not accepted the network terms")
	errLinkInUse        = apperr.New(http.StatusConflict, billing.CodeNetworkLinkInUse, "the customer's subscriptions at this merchant still charge its cards")
	errLinkNotFound     = apperr.New(http.StatusNotFound, billing.CodeResourceNotFound, "no such active link")
)

// challenge is what a challenge id holds in the store.
type challenge struct {
	Merchant   billing.MerchantID         `json:"m"`
	Customer   billing.CustomerID         `json:"c"`
	Credential customernetwork.Credential `json:"k"`
	Order      billing.OrderID            `json:"o"`
	Nonce      string                     `json:"n"`
	IssuedAt   time.Time                  `json:"t"`
}

func challengeKey(id string) string { return "network-link:" + id }

func random(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// Challenge issues a challenge bound to the merchant side.
func (s *Service) Challenge(ctx context.Context, proof customernetwork.Proof) (billing.NetworkLinkChallenge, error) {
	id, err := random(16)
	if err != nil {
		return billing.NetworkLinkChallenge{}, err
	}
	nonce, err := random(32)
	if err != nil {
		return billing.NetworkLinkChallenge{}, err
	}
	now := s.now().UTC()
	raw, err := json.Marshal(challenge{Merchant: proof.Merchant, Customer: proof.Customer, Credential: proof.Credential, Order: proof.Order, Nonce: nonce, IssuedAt: now})
	if err != nil {
		return billing.NetworkLinkChallenge{}, err
	}
	id = "lnc_" + id
	s.store.Put(ctx, challengeKey(id), raw, ChallengeTTL)
	return billing.NetworkLinkChallenge{ID: id, Nonce: nonce, ExpiresAt: now.Add(ChallengeTTL)}, nil
}

// Link redeems a challenge: the same merchant side, and an ID token of a
// network sign-in after the challenge, carrying its nonce, for the client
// the merchant side allows.
func (s *Service) Link(ctx context.Context, proof customernetwork.Proof, params billing.CreateNetworkLinkParams) (billing.CustomerLink, bool, error) {
	raw, ok := s.store.Take(ctx, challengeKey(params.ChallengeID))
	var c challenge
	if !ok || json.Unmarshal(raw, &c) != nil || c.Merchant != proof.Merchant || c.Customer != proof.Customer ||
		c.Credential != proof.Credential || c.Order != proof.Order {
		return billing.CustomerLink{}, false, errChallengeInvalid
	}
	tok, err := s.ak.VerifyIDToken(ctx, params.IDToken)
	switch {
	case errors.Is(err, iam.ErrInvalidIDToken):
		return billing.CustomerLink{}, false, errChallengeInvalid
	case err != nil:
		return billing.CustomerLink{}, false, fmt.Errorf("network: verify ID token: %w", err)
	}
	account, err := billing.ParseNetworkAccountID(tok.Subject)
	if err != nil || account.IsZero() || subtle.ConstantTimeCompare([]byte(tok.Nonce), []byte(c.Nonce)) != 1 ||
		tok.AuthTime.Before(c.IssuedAt.Truncate(time.Second)) {
		return billing.CustomerLink{}, false, errChallengeInvalid
	}
	method := billing.CustomerLinkConnect
	switch proof.Credential {
	case customernetwork.CheckoutURL:
		method = billing.CustomerLinkCheckout
		if tok.ClientID != CheckoutClientID || tok.GroupID != "" {
			return billing.CustomerLink{}, false, errChallengeInvalid
		}
	case customernetwork.CustomerToken:
		if tok.GroupID != proof.Merchant.String() {
			return billing.CustomerLink{}, false, errChallengeInvalid
		}
		if ok, err := s.consented(ctx, tok.Subject, tok.ClientID, ScopeLink); err != nil {
			return billing.CustomerLink{}, false, err
		} else if !ok {
			return billing.CustomerLink{}, false, errChallengeInvalid
		}
	default:
		return billing.CustomerLink{}, false, errChallengeInvalid
	}
	version, err := s.termsVersion(ctx, tok.Subject)
	if err != nil {
		return billing.CustomerLink{}, false, err
	}
	if version == "" {
		return billing.CustomerLink{}, false, errTermsRequired
	}
	// A customer that is a network account itself is linked to no other.
	native, err := s.db.Gen(ctx).NativeCustomerExists(ctx, gen.NativeCustomerExistsParams{MerchantID: proof.Merchant.UUID(), ID: proof.Customer.UUID()})
	if err != nil {
		return billing.CustomerLink{}, false, err
	}
	if native && account.UUID() != proof.Customer.UUID() {
		return billing.CustomerLink{}, false, errLinkConflict
	}
	credential := string(proof.Credential)
	insert := gen.InsertCustomerLinkParams{
		MerchantID: proof.Merchant.UUID(), CustomerID: proof.Customer.UUID(), AccountID: account.UUID(), Method: string(method),
		Credential: &credential, AuthTime: &tok.AuthTime, Amr: append([]string{}, tok.AMR...), TermsVersion: version, LinkedAt: s.now().UTC(),
	}
	if method == billing.CustomerLinkCheckout {
		order := proof.Order.UUID()
		insert.OrderID = &order
	}
	return s.insert(ctx, insert)
}

// insert makes a link unless one stands: the customer's own to the same
// account is answered as it is.
func (s *Service) insert(ctx context.Context, p gen.InsertCustomerLinkParams) (billing.CustomerLink, bool, error) {
	var out billing.CustomerLink
	created := false
	err := s.db.MerchantTx(merchant.WithID(ctx, billing.MerchantID(p.MerchantID)), func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		existing, err := q.GetActiveCustomerLinkByCustomer(ctx, gen.GetActiveCustomerLinkByCustomerParams{MerchantID: p.MerchantID, CustomerID: p.CustomerID})
		switch {
		case err == nil && existing.AccountID == p.AccountID:
			out = linkOf(existing)
			return nil
		case err == nil:
			return errLinkConflict
		case !db.IsNotFound(err):
			return err
		}
		if _, err := q.GetActiveCustomerLinkByAccount(ctx, gen.GetActiveCustomerLinkByAccountParams{AccountID: p.AccountID, MerchantID: p.MerchantID}); err == nil {
			return errAccountLinked
		} else if !db.IsNotFound(err) {
			return err
		}
		row, err := q.InsertCustomerLink(ctx, p)
		if err != nil {
			return err
		}
		out, created = linkOf(row), true
		method := out.Method
		return enqueue(ctx, q, row, billing.HostEventCustomerNetworkLinked, billing.CustomerNetworkHostEvent{AccountID: out.AccountID, Method: &method}, row.LinkedAt)
	})
	if db.IsUniqueViolation(err) {
		// Another request linked one side meanwhile.
		return billing.CustomerLink{}, false, errAccountLinked
	}
	return out, created, err
}

func enqueue(ctx context.Context, q *gen.Queries, row gen.BillingCustomerLink, kind billing.HostEventType, payload billing.CustomerNetworkHostEvent, at time.Time) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return q.EnqueueCustomerNetworkEvent(ctx, gen.EnqueueCustomerNetworkEventParams{
		MerchantID: row.MerchantID, EventType: string(kind), CustomerID: row.CustomerID, OccurredAt: at, Data: data, LinkID: row.ID.String(),
	})
}

func linkOf(row gen.BillingCustomerLink) billing.CustomerLink {
	out := billing.CustomerLink{
		ID: billing.CustomerLinkID(row.ID), MerchantID: billing.MerchantID(row.MerchantID), CustomerID: billing.CustomerID(row.CustomerID),
		AccountID: billing.NetworkAccountID(row.AccountID), Method: billing.CustomerLinkMethod(row.Method), Status: billing.CustomerLinkStatus(row.Status),
		LinkedAt: row.LinkedAt.UTC(),
	}
	if row.RevokedAt != nil {
		at := row.RevokedAt.UTC()
		out.RevokedAt = &at
	}
	if row.RevokedBy != nil {
		by := billing.CustomerLinkRevokedBy(*row.RevokedBy)
		out.RevokedBy = &by
	}
	return out
}

// consented reports whether account's consent to clientID holds scope.
func (s *Service) consented(ctx context.Context, account, clientID, scope string) (bool, error) {
	consents, err := s.ak.OAuthConsents(ctx, account)
	if err != nil {
		return false, fmt.Errorf("network: read consents: %w", err)
	}
	return slices.ContainsFunc(consents, func(c iam.OAuthConsent) bool {
		return c.ClientID == clientID && slices.Contains(c.Scopes, scope)
	}), nil
}

// termsVersion is the network-terms version account accepted; "" while they
// are due.
func (s *Service) termsVersion(ctx context.Context, account string) (string, error) {
	due, err := s.ak.AgreementsDue(ctx, account)
	if err != nil {
		return "", fmt.Errorf("network: read agreements due: %w", err)
	}
	if slices.ContainsFunc(due, func(a iam.Agreement) bool { return a.Key == TermsAgreement }) {
		return "", nil
	}
	accepted, err := s.ak.UserAgreements(ctx, account)
	if err != nil {
		return "", fmt.Errorf("network: read agreements: %w", err)
	}
	var latest *iam.AgreementAcceptance
	for i, a := range accepted {
		if a.Key == TermsAgreement && (latest == nil || a.AcceptedAt.After(latest.AcceptedAt)) {
			latest = &accepted[i]
		}
	}
	if latest == nil {
		return "", nil
	}
	return latest.Version, nil
}

// Networks are the merchant's customers' active links.
func (s *Service) Networks(ctx context.Context, mid billing.MerchantID, customers []uuid.UUID) (map[uuid.UUID]billing.CustomerNetwork, error) {
	rows, err := s.db.Gen(ctx).ListActiveCustomerLinksByCustomers(ctx, gen.ListActiveCustomerLinksByCustomersParams{MerchantID: mid.UUID(), CustomerIds: customers})
	if err != nil {
		return nil, err
	}
	out := make(map[uuid.UUID]billing.CustomerNetwork, len(rows))
	for _, row := range rows {
		out[row.CustomerID] = billing.CustomerNetwork{AccountID: billing.NetworkAccountID(row.AccountID), LinkedAt: row.LinkedAt.UTC(), Method: billing.CustomerLinkMethod(row.Method)}
	}
	return out, nil
}

// CustomerOf is the merchant's customer linked to account.
func (s *Service) CustomerOf(ctx context.Context, mid billing.MerchantID, account billing.NetworkAccountID) (billing.CustomerID, bool, error) {
	row, err := s.db.Gen(ctx).GetActiveCustomerLinkByAccount(ctx, gen.GetActiveCustomerLinkByAccountParams{AccountID: account.UUID(), MerchantID: mid.UUID()})
	switch {
	case db.IsNotFound(err):
		return billing.CustomerID{}, false, nil
	case err != nil:
		return billing.CustomerID{}, false, err
	}
	return billing.CustomerID(row.CustomerID), true, nil
}

// AdmitNative admits the account as the merchant's own customer: refused
// while it is linked to another customer there (one person is one customer
// per merchant), linked by sign-in when it accepted the network terms.
func (s *Service) AdmitNative(ctx context.Context, mid billing.MerchantID, subject billing.CustomerID, admit func(context.Context) error) error {
	row, err := s.db.Gen(ctx).GetActiveCustomerLinkByAccount(ctx, gen.GetActiveCustomerLinkByAccountParams{AccountID: subject.UUID(), MerchantID: mid.UUID()})
	linked := err == nil
	switch {
	case linked && row.CustomerID != subject.UUID():
		return billingauth.Refusal(billing.CodeNetworkAccountLinked)
	case !linked && !db.IsNotFound(err):
		return err
	}
	if err := admit(ctx); err != nil || linked {
		return err
	}
	return s.linkNative(ctx, mid, subject)
}

// linkNative links the merchant's native customer subject to itself, once
// the account accepted the network terms.
func (s *Service) linkNative(ctx context.Context, mid billing.MerchantID, subject billing.CustomerID) error {
	version, err := s.termsVersion(ctx, subject.String())
	if err != nil || version == "" {
		return err
	}
	_, _, err = s.insert(ctx, gen.InsertCustomerLinkParams{
		MerchantID: mid.UUID(), CustomerID: subject.UUID(), AccountID: subject.UUID(), Method: string(billing.CustomerLinkSignIn),
		TermsVersion: version, LinkedAt: s.now().UTC(),
	})
	var refusal *apperr.Error
	if errors.As(err, &refusal) {
		// Linked otherwise meanwhile: the next admission decides.
		log.WithContext(ctx).WithFields(log.Fields{"merchant_id": mid, "code": refusal.Code}).Info("network: native customer not linked")
		return nil
	}
	return err
}
