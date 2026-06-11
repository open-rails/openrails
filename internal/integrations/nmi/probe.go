package nmi

import (
	"fmt"
	"net/url"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Test-mode account detection (#347).
//
// NMI sandbox/test accounts are indistinguishable by configuration: they hit
// the SAME production gateway URL and the security key carries no marker
// (unlike Stripe's sk_test_/sk_live_ prefixes). What IS distinguishable is the
// gateway's documented simulation behavior when an account is in Test Mode
// (docs.nmi.com/reference/testing-methods):
//
//   - any valid test card with amount >= 1.00 is APPROVED without touching a
//     processor;
//   - any amount < 1.00 is deterministically DECLINED by the simulator.
//
// A LIVE account cannot reproduce that signature: the canonical test PAN
// (4111...) is not an issued card, so a real processor DECLINES the $1.00
// auth. The probe therefore runs two authorization-only requests:
//
//	auth $1.00 approved + auth $0.50 declined  -> account is simulating (safe)
//	auth $1.00 DECLINED                        -> account is LIVE
//	auth approves both amounts                 -> not the simulator signature;
//	                                              treated as live (unsafe)
//	transport/credential errors (response=3)   -> indeterminate
//
// The probe is harmless on a live account — an auth on a non-issued PAN is
// declined and no money can move. An approved simulated auth is voided
// (best-effort) for tidiness.

const (
	probeTestCard   = "4111111111111111"
	probeTestExpiry = "1029" // the documented test-card expiry (10/29)
)

// TestModeProbeResult classifies what the probe learned about the account.
type TestModeProbeResult int

const (
	// ProbeIndeterminate: transport failure or a gateway-level error
	// (e.g. invalid credentials) — nothing can be concluded.
	ProbeIndeterminate TestModeProbeResult = iota
	// ProbeSimulated: the account exhibits the documented Test Mode
	// simulation signature — transactions cannot move real money.
	ProbeSimulated
	// ProbeLive: the account did NOT simulate — it forwarded to a real
	// processor (or otherwise broke the simulation signature).
	ProbeLive
)

// ProbeTestMode determines whether this account is currently simulating
// transactions (NMI Test Mode / sandbox) by fingerprinting the documented
// simulation behavior. Call only when the operating mode expects sandbox
// money; on a live account the probe costs one declined auth.
func (c *NMIClient) ProbeTestMode() (TestModeProbeResult, error) {
	if err := c.checkConfiguration(); err != nil {
		return ProbeIndeterminate, err
	}

	// Probe 1: auth $1.00 on the documented test card. Simulator: approved.
	// Live processor: declined (non-issued PAN).
	approved, txnID, gatewayErr, err := c.probeAuth("1.00")
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

	// Probe 2: auth $0.50. The simulator deterministically DECLINES amounts
	// below 1.00; an account that approves both amounts is not exhibiting the
	// simulation signature and must be treated as live.
	approved, txnID, gatewayErr, err = c.probeAuth("0.50")
	if err != nil || gatewayErr != "" {
		if err == nil {
			err = fmt.Errorf("nmi test-mode probe inconclusive on sub-dollar check: %s", gatewayErr)
		}
		return ProbeIndeterminate, err
	}
	if approved {
		c.voidProbe(txnID)
		return ProbeLive, nil
	}
	return ProbeSimulated, nil
}

// probeAuth submits an authorization-only request with the documented test
// card. Returns the approval verdict, the transaction id (for voiding), the
// gateway error text when the gateway rejected the REQUEST itself
// (response=3), and any transport error.
func (c *NMIClient) probeAuth(amount string) (approved bool, txnID string, gatewayErr string, err error) {
	values := url.Values{
		"type":         {"auth"},
		"security_key": {c.SecurityKey},
		"ccnumber":     {probeTestCard},
		"ccexp":        {probeTestExpiry},
		"amount":       {amount},
		"order_id":     {"openrails-testmode-probe"},
	}
	response, err := c.sendDirectRequest(values)
	if err != nil {
		return false, "", "", fmt.Errorf("nmi test-mode probe request failed: %w", err)
	}
	output, err := parseDirectResponse(response)
	if err != nil {
		return false, "", "", fmt.Errorf("nmi test-mode probe response unparseable: %w", err)
	}
	if output.Get("response") == "3" {
		return false, "", responseText(output, response), nil
	}
	return isDirectResponseApproved(output), output.Get("transactionid"), "", nil
}

// voidProbe best-effort voids an approved probe authorization. On a
// simulating account the void is itself simulated; failures only get logged —
// an unvoided $1.00 test-card auth holds no real funds.
func (c *NMIClient) voidProbe(txnID string) {
	if strings.TrimSpace(txnID) == "" {
		return
	}
	values := url.Values{
		"type":          {"void"},
		"security_key":  {c.SecurityKey},
		"transactionid": {txnID},
	}
	if _, err := c.sendDirectRequest(values); err != nil {
		log.WithError(err).WithFields(log.Fields{
			"provider":       c.providerName,
			"transaction_id": txnID,
		}).Warn("nmi test-mode probe: failed to void probe authorization")
	}
}
