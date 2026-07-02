package nmi

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/open-rails/openrails/internal/shared/moneyutil"
	log "github.com/sirupsen/logrus"
)

// Test-mode account detection (#348).
//
// NMI sandbox/test accounts are indistinguishable by configuration: they hit
// the SAME production gateway URL and the security key carries no marker
// (unlike Stripe's sk_test_/sk_live_ prefixes). What IS distinguishable is
// behavior: the canonical test card (4111...) is a non-issued PAN that no
// production rail can ever approve, while a test-mode account simulates
// approval for it without touching a rail
// (docs.nmi.com/reference/testing-methods). One authorization-only probe is
// therefore conclusive:
//
//	auth on the test card APPROVED -> the account is simulating (safe)
//	auth on the test card DECLINED -> the account is LIVE
//	transport/credential errors (response=3) -> indeterminate
//
// The probe is harmless on a live account — an auth on a non-issued PAN is
// declined and no money can move (it does cost one declined-attempt gateway
// fee, typically cents). An approved simulated auth is voided (best-effort)
// for tidiness.
//
// The amount and order_id are randomized per attempt: NMI's duplicate-
// transaction check rejects a repeat of the same card+amount+order_id within
// its window (response=3 "Duplicate transaction"), and the rail refuses
// dup_seconds=0, so a fixed probe would turn back-to-back boots (#362) into
// ProbeIndeterminate. The verdict is amount-independent — no production
// rail approves a non-issued PAN at any amount.

const (
	probeTestCard      = "4111111111111111"
	probeTestExpiry    = "1029" // the documented test-card expiry (10/29)
	probeOrderIDPrefix = "openrails-testmode-probe-"
)

// probeAmount returns a randomized auth amount in [$1.01, $1.99] so repeated
// probes never trip duplicate-transaction detection. crypto/rand is used (over
// math/rand) only to satisfy gosec G404 — randomness here is for uniqueness,
// not security; a read failure just degrades to the lower bound.
func probeAmount() string {
	var b [1]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("1.%02d", 1+int(b[0])%99)
}

// TestModeProbeResult classifies what the probe learned about the account.
type TestModeProbeResult int

const (
	// ProbeIndeterminate: transport failure or a gateway-level error
	// (e.g. invalid credentials) — nothing can be concluded.
	ProbeIndeterminate TestModeProbeResult = iota
	// ProbeSimulated: the account approved the non-issued test card —
	// only a simulator can do that; transactions cannot move real money.
	ProbeSimulated
	// ProbeLive: the account declined the test card — it forwarded to a real
	// rail, so it is a live account.
	ProbeLive
)

// ProbeTestMode determines whether this account is currently simulating
// transactions (NMI Test Mode / sandbox). Call only when the operating mode
// expects sandbox money; on a live account the probe costs one declined auth.
func (c *NMIClient) ProbeTestMode() (TestModeProbeResult, error) {
	if err := c.checkConfiguration(); err != nil {
		return ProbeIndeterminate, err
	}

	approved, txnID, gatewayErr, err := c.probeAuth(probeAmount())
	if err != nil {
		return ProbeIndeterminate, err
	}
	if gatewayErr != "" {
		// response=3: request-level error (bad credentials, validation) —
		// says nothing about live vs simulated.
		return ProbeIndeterminate, fmt.Errorf("nmi test-mode probe inconclusive: %s", gatewayErr)
	}
	if !approved {
		return ProbeLive, nil
	}
	c.voidProbe(txnID)
	return ProbeSimulated, nil
}

// probeOrderIDSuffix returns 8 hex chars of randomness for the per-probe
// order_id. Same crypto/rand-for-gosec note as probeAmount.
func probeOrderIDSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%08x", binary.BigEndian.Uint32(b[:]))
}

// probeAuth submits an authorization-only request with the documented test
// card via POST /v5/payments/auth. Returns the approval verdict, the
// transaction id (for voiding), the gateway error text when the gateway
// rejected the REQUEST itself (v5 error envelope or response=3), and any
// transport error.
func (c *NMIClient) probeAuth(amount string) (approved bool, txnID string, gatewayErr string, err error) {
	amountCents, err := v5AmountToCents(amount)
	if err != nil {
		return false, "", "", fmt.Errorf("nmi test-mode probe amount %q: %w", amount, err)
	}
	req := v5PaymentRequest{
		Amount:         centsJSONAmount(moneyutil.Cents(amountCents)),
		PaymentDetails: &v5PaymentDetails{CardNumber: probeTestCard, CardExp: probeTestExpiry},
		OrderDetails:   &v5OrderDetails{ID: probeOrderIDPrefix + probeOrderIDSuffix()},
	}
	var txn v5Transaction
	if v5Err := c.sendV5Request(http.MethodPost, "/payments/auth", req, &txn); v5Err != nil {
		if errors.Is(v5Err, ErrProviderReadOnly) {
			return false, "", "", v5Err
		}
		// A 4xx envelope (bad credentials, validation) is the v5 shape of the
		// classic response=3: request-level rejection, verdict indeterminate.
		return false, "", v5Err.Error(), nil
	}
	if strings.TrimSpace(txn.Response) == "3" {
		return false, "", strings.TrimSpace(txn.ResponseText), nil
	}
	return txn.approved(), txn.ID, "", nil
}

// voidProbe best-effort voids an approved probe authorization. On a
// simulating account the void is itself simulated; failures only get logged —
// an unvoided test-card auth holds no real funds.
func (c *NMIClient) voidProbe(txnID string) {
	if strings.TrimSpace(txnID) == "" {
		return
	}
	if err := c.Void(txnID); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"provider":       c.providerName,
			"transaction_id": txnID,
		}).Warn("nmi test-mode probe: failed to void probe authorization")
	}
}
