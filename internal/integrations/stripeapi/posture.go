package stripeapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/providerposture"
)

// APIBase is the Stripe REST root every production call site targets.
const APIBase = "https://api.stripe.com"

// PostureKey binds a verdict to the merchant's PSP, the account it declares
// (acct_...) and its secret key: a change of any is a new key, checked anew.
// A live key arms on Live, a test key on Simulated, and the verdict lasts: a
// key's account and mode never change.
func PostureKey(merchantID, pspID uuid.UUID, declared, secretKey string) providerposture.Key {
	key := providerposture.Key{Rail: "stripe", MerchantID: merchantID, PSPID: pspID, AccountID: declared, Endpoint: APIBase, Credential: providerposture.Fingerprint(secretKey), Lasting: true}
	if liveSecretKey(secretKey) {
		key.Expect = providerposture.Live
	}
	return key
}

// PostureCheck proves a key at the fixed Stripe API root: GET /v1/account
// must name the declared account (another is Mismatched). A live key's mode
// is its prefix; a test key also reads GET /v1/balance, whose livemode must be
// false. A key Stripe refuses (401), or one lacking the permission a read
// needs (403), is Refused with Stripe's answer, which names that permission.
func PostureCheck(transport http.RoundTripper, secretKey, declared string) providerposture.Check {
	return func(ctx context.Context) (providerposture.Verdict, error) {
		live := liveSecretKey(secretKey)
		switch {
		case !live && !strings.HasPrefix(secretKey, "sk_test_") && !strings.HasPrefix(secretKey, "rk_test_"):
			return providerposture.Refused, errors.New("stripe key has no live or test-mode prefix")
		case declared == "":
			return providerposture.Refused, errors.New("stripe PSP declares no account to prove its key against")
		}
		client := &http.Client{Transport: transport, Timeout: DefaultTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		read := func(path string, target any) (providerposture.Verdict, error) {
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, APIBase+path, nil)
			if err != nil {
				return providerposture.Unknown, err
			}
			req.Header.Set("Authorization", "Bearer "+secretKey)
			req.Header.Set(VersionHeader, APIVersion)
			resp, err := client.Do(req)
			if err != nil {
				return providerposture.Unknown, fmt.Errorf("stripe posture read %s: %w", path, err)
			}
			defer resp.Body.Close()
			switch {
			case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
				return providerposture.Refused, fmt.Errorf("stripe refused GET %s (%d): %s", path, resp.StatusCode, refusalReason(resp.Body))
			case resp.StatusCode != http.StatusOK:
				return providerposture.Unknown, fmt.Errorf("stripe posture read %s: http %d", path, resp.StatusCode)
			}
			if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(target); err != nil {
				return providerposture.Unknown, fmt.Errorf("stripe posture read %s: %w", path, err)
			}
			return providerposture.Unknown, nil
		}
		var account struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		}
		if verdict, err := read("/v1/account", &account); err != nil {
			return verdict, err
		}
		if account.Object != "account" || account.ID != declared {
			return providerposture.Mismatched, errors.New("stripe key belongs to a different account than declared")
		}
		if live {
			return providerposture.Live, nil
		}
		var balance struct {
			Object   string `json:"object"`
			Livemode *bool  `json:"livemode"`
		}
		if verdict, err := read("/v1/balance", &balance); err != nil {
			return verdict, err
		}
		if balance.Object != "balance" || balance.Livemode == nil {
			return providerposture.Unknown, errors.New("stripe livemode read: unexpected response")
		}
		if *balance.Livemode {
			return providerposture.Live, errors.New("stripe account reports livemode=true for a test key")
		}
		return providerposture.Simulated, nil
	}
}

// keyText is a Stripe key as a refusal may quote it.
var keyText = regexp.MustCompile(`\b[a-z]{2,4}_(live|test)_[0-9A-Za-z*]+`)

// refusalReason is Stripe's message for a refused read, which names the
// permission a restricted key lacks, with any key it quotes removed.
func refusalReason(body io.Reader) string {
	var answer struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(body, 64<<10)).Decode(&answer) != nil || strings.TrimSpace(answer.Error.Message) == "" {
		return "no reason given"
	}
	reason := keyText.ReplaceAllString(strings.TrimSpace(answer.Error.Message), "[key]")
	if len(reason) > 300 {
		reason = reason[:300]
	}
	return reason
}

// RegisterPosture names the check of a PSP's key when its merchant's
// configuration loads, and starts it unless the key has a verdict: a write
// with the key waits for that first check (requirePosture).
func (f *Factory) RegisterPosture(merchantID, pspID uuid.UUID, declared, secretKey string) {
	providerposture.Process().Register(PostureKey(merchantID, pspID, declared, secretKey), f.PostureCheckFor(secretKey, declared))
}

// VerifyPosture verifies a PSP's key now and records the verdict for the
// transport gate: the key's mode, and that it belongs to the declared account.
func (f *Factory) VerifyPosture(ctx context.Context, merchantID, pspID uuid.UUID, declared, secretKey string) providerposture.Status {
	return providerposture.Process().Verify(ctx, PostureKey(merchantID, pspID, declared, secretKey), f.PostureCheckFor(secretKey, declared))
}

// PostureCheckFor returns the check of a declared account's key.
func (f *Factory) PostureCheckFor(secretKey, declared string) providerposture.Check {
	switch {
	case f != nil && f.wired:
		return PostureCheck(f.base, secretKey, declared)
	case f != nil && f.base != nil:
		return func(context.Context) (providerposture.Verdict, error) {
			if liveSecretKey(secretKey) {
				// An injected transport is a fake provider: it moves no money.
				return providerposture.Simulated, errors.New("stripe live key with an injected transport")
			}
			return providerposture.Simulated, nil
		}
	}
	return PostureCheck(http.DefaultTransport, secretKey, declared)
}

// WriteDisarmed is the refusal a write with secretKey would meet at the
// transport, from what is known without asking Stripe: a live key to a fake
// provider, a key no loaded PSP declares, or one whose recorded verdict
// refuses writes. A key still being checked is not refused here; its write's
// gate waits for it.
func (f *Factory) WriteDisarmed(secretKey string) error {
	fixture := f != nil && f.base != nil && !f.wired
	switch {
	case fixture && liveSecretKey(secretKey):
		return fmt.Errorf("%w: stripe live key to a fake provider", providerposture.ErrDisarmed)
	case fixture:
		return nil
	}
	return providerposture.Process().CredentialRefusal("stripe", APIBase, providerposture.Fingerprint(secretKey))
}

// requirePosture gates a mutation on its key's verdict, from the check its
// PSP registered when its merchant's configuration loaded (RegisterPosture):
// a live key's account, a test key's account and test mode. A key no loaded
// PSP declares is refused at once (ErrPending, an ErrDisarmed that parks an
// intent); one whose first check is running waits for it. A write for a
// connected account (Stripe-Account) is never proven. A live key never
// reaches a fake provider (an injected transport or a literal loopback IP); a
// test key's mutation there is a fixture's.
func (t *guardTransport) requirePosture(req *http.Request) error {
	secretKey := requestSecret(req)
	switch {
	case t.fixture || loopbackDestination(req):
		if liveSecretKey(secretKey) {
			return fmt.Errorf("%w: stripe live key to a fake provider", providerposture.ErrDisarmed)
		}
		return nil
	case req.Header.Get("Stripe-Account") != "":
		return fmt.Errorf("%w: stripe writes for a connected account are not proven", providerposture.ErrDisarmed)
	}
	return providerposture.Process().RequireCredential(req.Context(), "stripe", req.URL.Scheme+"://"+req.URL.Host, providerposture.Fingerprint(secretKey))
}

// loopbackDestination reports a request to a literal loopback IP: a fixture.
func loopbackDestination(req *http.Request) bool {
	ip := net.ParseIP(req.URL.Hostname())
	return ip != nil && ip.IsLoopback()
}

func requestSecret(req *http.Request) string {
	if user, _, ok := req.BasicAuth(); ok {
		return user
	}
	return strings.TrimSpace(strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
}

// liveSecretKey reports a live-mode Stripe secret or restricted key.
func liveSecretKey(secretKey string) bool {
	return strings.HasPrefix(secretKey, "sk_live_") || strings.HasPrefix(secretKey, "rk_live_")
}
