package openrails

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/asn1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"

	jwt "github.com/golang-jwt/jwt/v5"

	"github.com/open-rails/openrails/billing"
)

// A customer token is minted in two steps (RFC 7523): the merchant's backend
// signs a short assertion naming its customer with the key registered as the
// merchant's remote application, and the server's AuthKit redeems it once for
// an access token bound to that merchant and customer.

const (
	assertionLifetime = 2 * time.Minute
	jwtBearerGrant    = "urn:ietf:params:oauth:grant-type:jwt-bearer"
)

var (
	errInprocessCustomerToken = errors.New("openrails: an in-process host mints customer tokens with its own AuthKit; CustomerToken and CustomerAssertion are a remote handle's")
	errNoAssertionKey         = errors.New("openrails: the handle's credential has no assertion key (RemoteCredential.Assertion)")
)

// CustomerAssertion signs the assertion that names params.CustomerID as the
// merchant's customer, for a frontend to redeem itself at the token endpoint
// (grant_type urn:ietf:params:oauth:grant-type:jwt-bearer, scope
// openrails:self), with its own DPoP proof when it binds the token to a key.
// It is valid for two minutes and redeems once. It makes no request. It fails
// on a handle whose credential has no Assertion and on an in-process handle,
// whose host mints with its own AuthKit.
func (c *Client) CustomerAssertion(ctx context.Context, params billing.CustomerTokenParams) (string, error) {
	key, err := c.assertionKey()
	if err != nil {
		return "", err
	}
	if params.CustomerID.IsZero() {
		return "", invalidErr("customer id is required")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	method, err := assertionMethod(key.Signer)
	if err != nil {
		return "", err
	}
	var jti [16]byte
	if _, err := rand.Read(jti[:]); err != nil {
		return "", fmt.Errorf("openrails: assertion id: %w", err)
	}
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": key.Issuer, "sub": params.CustomerID.String(), "aud": key.TokenURL,
		"iat": now.Unix(), "exp": now.Add(assertionLifetime).Unix(), "jti": hex.EncodeToString(jti[:]),
	}
	if email := strings.TrimSpace(params.Email); email != "" {
		claims["email"], claims["email_verified"] = email, params.EmailVerified
	}
	token := jwt.NewWithClaims(method, claims)
	token.Header["kid"] = key.KeyID
	signed, err := token.SignedString(key.Signer)
	if err != nil {
		return "", fmt.Errorf("openrails: sign customer assertion: %w", err)
	}
	return signed, nil
}

// CustomerToken mints an access token for params.CustomerID as the merchant's
// customer: it signs a CustomerAssertion and redeems it at the credential's
// Assertion.TokenURL. The backend hands the token to its frontend, which sends
// it to the customer routes as its Bearer. It fails on a handle whose
// credential has no Assertion and on an in-process handle, whose host mints
// with its own AuthKit. A refusal is a *billing.StatusError carrying the token
// endpoint's status and its OAuth error as the message, with no code.
func (c *Client) CustomerToken(ctx context.Context, params billing.CustomerTokenParams) (*billing.CustomerToken, error) {
	assertion, err := c.CustomerAssertion(ctx, params)
	if err != nil {
		return nil, err
	}
	if c.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.timeout)
		defer cancel()
	}
	form := url.Values{"grant_type": {jwtBearerGrant}, "assertion": {assertion}, "scope": {billing.ScopeSelf}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.assertion.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("openrails: build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	requested := time.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", billing.ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if err != nil {
		return nil, fmt.Errorf("%w: read token response: %w", billing.ErrUnreachable, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, tokenEndpointError(resp.StatusCode, raw)
	}
	var granted struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(raw, &granted); err != nil || granted.AccessToken == "" || granted.ExpiresIn <= 0 {
		return nil, fmt.Errorf("%w: the token endpoint answered no access token", billing.ErrUnreachable)
	}
	return &billing.CustomerToken{
		AccessToken: granted.AccessToken, TokenType: granted.TokenType,
		ExpiresAt: requested.Add(time.Duration(granted.ExpiresIn) * time.Second),
	}, nil
}

func (c *Client) assertionKey() (*AssertionKey, error) {
	switch {
	case c == nil || c.inprocess:
		return nil, errInprocessCustomerToken
	case c.assertion == nil:
		return nil, errNoAssertionKey
	}
	return c.assertion, nil
}

// tokenEndpointError is the token endpoint's refusal (RFC 6749 §5.2). It is
// not an OpenRails route, so the error carries no OpenRails code.
func tokenEndpointError(status int, raw []byte) error {
	var refusal struct {
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	message := excerptErrorBody(raw)
	if json.Unmarshal(raw, &refusal) == nil && refusal.Error != "" {
		message = excerptErrorBody([]byte(strings.TrimSuffix(refusal.Error+": "+refusal.Description, ": ")))
	}
	return &billing.StatusError{Status: status, ErrorDetails: billing.ErrorDetails{Message: "token endpoint: " + message}}
}

// validate refuses an assertion key that could sign nothing redeemable.
func (k *AssertionKey) validate() error {
	if k == nil {
		return nil
	}
	switch {
	case strings.TrimSpace(k.Issuer) == "":
		return errors.New("Assertion.Issuer is required")
	case strings.TrimSpace(k.KeyID) == "":
		return errors.New("Assertion.KeyID is required")
	case k.Signer == nil:
		return errors.New("Assertion.Signer is required")
	}
	if _, err := assertionMethod(k.Signer); err != nil {
		return err
	}
	u, err := url.Parse(k.TokenURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("Assertion.TokenURL %q must be the token endpoint's absolute http or https URL", k.TokenURL)
	}
	return nil
}

// signerMethod signs a JWT with a crypto.Signer, so the key may live in a
// KMS or an HSM.
type signerMethod struct {
	alg string
	// hash digests the signing input; zero when the signer takes the input
	// itself (Ed25519).
	hash crypto.Hash
	// size is an ECDSA coordinate's bytes; zero for the other families.
	size int
}

// assertionMethod is the JWS algorithm of signer's key.
func assertionMethod(signer crypto.Signer) (signerMethod, error) {
	switch pub := signer.Public().(type) {
	case *rsa.PublicKey:
		return signerMethod{alg: "RS256", hash: crypto.SHA256}, nil
	case ed25519.PublicKey:
		return signerMethod{alg: "EdDSA"}, nil
	case *ecdsa.PublicKey:
		switch pub.Curve {
		case elliptic.P256():
			return signerMethod{alg: "ES256", hash: crypto.SHA256, size: 32}, nil
		case elliptic.P384():
			return signerMethod{alg: "ES384", hash: crypto.SHA384, size: 48}, nil
		case elliptic.P521():
			return signerMethod{alg: "ES512", hash: crypto.SHA512, size: 66}, nil
		}
	}
	return signerMethod{}, errors.New("Assertion.Signer must hold an RSA, ECDSA (P-256, P-384, P-521) or Ed25519 key")
}

func (m signerMethod) Alg() string { return m.alg }

// Verify is unused: a handle only signs.
func (signerMethod) Verify(string, []byte, any) error { return errors.ErrUnsupported }

func (m signerMethod) Sign(signingString string, key any) ([]byte, error) {
	signer, ok := key.(crypto.Signer)
	if !ok {
		return nil, jwt.ErrInvalidKeyType
	}
	if m.hash == 0 {
		return signer.Sign(rand.Reader, []byte(signingString), crypto.Hash(0))
	}
	digest := m.hash.New()
	digest.Write([]byte(signingString))
	signature, err := signer.Sign(rand.Reader, digest.Sum(nil), m.hash)
	if err != nil || m.size == 0 {
		return signature, err
	}
	// A crypto.Signer answers ECDSA in ASN.1 DER; JWS wants R and S, fixed width.
	var point struct{ R, S *big.Int }
	if rest, err := asn1.Unmarshal(signature, &point); err != nil || len(rest) != 0 || point.R == nil || point.S == nil || point.R.Sign() < 0 || point.S.Sign() < 0 ||
		len(point.R.Bytes()) > m.size || len(point.S.Bytes()) > m.size {
		return nil, errors.New("openrails: the signer answered a malformed ECDSA signature")
	}
	raw := make([]byte, 2*m.size)
	point.R.FillBytes(raw[:m.size])
	point.S.FillBytes(raw[m.size:])
	return raw, nil
}
