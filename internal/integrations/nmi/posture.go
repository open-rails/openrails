package nmi

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/config"
	"github.com/open-rails/openrails/internal/providerposture"
)

type probeContextKey struct{}

// PostureKey binds a posture verdict to this client's exact merchant, PSP
// account, environment, endpoints and security key.
func (c *NMIClient) PostureKey() providerposture.Key {
	key := providerposture.Key{
		Rail:       "nmi",
		MerchantID: c.accountMerchantID,
		PSPID:      c.accountPSPID,
		AccountID:  c.providerName,
		Endpoint:   strings.Join([]string{c.Environment, c.DirectPostURL, c.QueryURL, c.v5BaseURL()}, " "),
		Credential: providerposture.Fingerprint(c.SecurityKey),
	}
	if c.Live() {
		key.Expect = providerposture.Live
	}
	return key
}

// VerifyPosture runs NMI's authoritative check of the declared environment
// and records the verdict for every client that loads the same credential. It
// runs when credentials load (startup, create, rotation).
func (c *NMIClient) VerifyPosture(ctx context.Context) providerposture.Status {
	if c.LoopbackFixture && !c.Live() {
		return providerposture.Status{Key: c.PostureKey(), Verdict: providerposture.Simulated}
	}
	return providerposture.Process().Verify(ctx, c.PostureKey(), c.PostureCheck())
}

// PostureCheck is the check that proves the declared environment: a live or
// test account answers the regular gateway's read-only test_mode_status, a
// sandbox account the sandbox's qualification probe.
func (c *NMIClient) PostureCheck() providerposture.Check {
	return c.CheckPosture
}

// CheckPosture asks NMI whether the account moves real money and refuses an
// answer that disagrees with the declared environment. A test-mode account
// approves without moving money, so it must never grant access as live.
func (c *NMIClient) CheckPosture(ctx context.Context) (providerposture.Verdict, error) {
	ctx = context.WithValue(ctx, probeContextKey{}, c)
	var result TestModeProbeResult
	var err error
	if c.Environment == config.EnvironmentSandbox {
		result, err = c.ProbeTestMode(ctx)
	} else {
		result, err = c.readGatewayTestMode(ctx)
	}
	switch {
	case errors.Is(err, errUnexpectedTestMode):
		// The gateway answered, just not with the documented answer: asking
		// again gets the same, so it never arms until its credentials reload.
		return providerposture.Unsupported, err
	case err != nil:
		return providerposture.Unknown, err
	case result == ProbeLive && c.Live():
		return providerposture.Live, nil
	case result == ProbeLive:
		return providerposture.Live, fmt.Errorf("%w: it moves real money; the PSP declares %s", ErrEnvironmentMismatch, c.Environment)
	case result == ProbeSimulated && !c.Live():
		return providerposture.Simulated, nil
	case result == ProbeSimulated:
		return providerposture.Simulated, fmt.Errorf("%w: it is in test mode; the PSP declares live (disable test mode in the NMI gateway)", ErrEnvironmentMismatch)
	default:
		return providerposture.Unknown, errors.New("NMI test-mode verification was indeterminate")
	}
}

// requireArmed gates every mutation: the credential must hold a cached verdict
// matching its declared environment (live, or simulated for test and
// sandbox); an unseen credential is verified once, inline.
func (c *NMIClient) requireArmed(ctx context.Context, target string) error {
	if ctx.Value(probeContextKey{}) == c {
		return nil
	}
	if c.Live() {
		// A live credential proves it is not in test mode before its first
		// mutation: verified on load, or inline (bounded, once) when this
		// process has not seen it.
		if c.LoopbackFixture {
			return nil
		}
		return providerposture.Process().Require(ctx, c.PostureKey(), c.CheckPosture)
	}
	if c.LoopbackFixture {
		if err := config.ValidateLoopbackGatewayURL(target); err != nil {
			return fmt.Errorf("%w: loopback fixture: %w", providerposture.ErrDisarmed, err)
		}
		return nil
	}
	return providerposture.Process().Require(ctx, c.PostureKey(), c.CheckPosture)
}

// readGatewayTestMode uses the regular gateway's read-only Query contract.
// Never fall back from a failed read to a financial probe or another endpoint.
func (c *NMIClient) readGatewayTestMode(ctx context.Context) (TestModeProbeResult, error) {
	if err := c.checkConfiguration(); err != nil {
		return ProbeIndeterminate, err
	}
	raw, err := c.sendQueryRequest(ctx, url.Values{"security_key": {c.SecurityKey}, "report_type": {"test_mode_status"}})
	if err != nil {
		return ProbeIndeterminate, err
	}
	// The gateway's error answer is a failed read, not an answer of an
	// unknown shape: rejected credentials or a retryable error.
	var refused struct {
		XMLName       xml.Name `xml:"nm_response"`
		ErrorResponse string   `xml:"error_response"`
	}
	if xml.Unmarshal([]byte(raw), &refused) == nil && strings.TrimSpace(refused.ErrorResponse) != "" {
		return ProbeIndeterminate, queryError("test_mode_status", strings.TrimSpace(refused.ErrorResponse))
	}
	return parseGatewayTestMode(raw)
}

// errUnexpectedTestMode is the gateway's own response, a lone bare
// <nm_response>, that does not hold the documented answer.
var errUnexpectedTestMode = errors.New("unexpected test_mode_status answer")

// errNotGatewayResponse is a body that is not a lone bare <nm_response>: a
// maintenance or WAF page in front of the gateway, which says nothing about it.
var errNotGatewayResponse = errors.New("test_mode_status response is not the gateway's")

func parseGatewayTestMode(raw string) (TestModeProbeResult, error) {
	// Deliberately accept only the documented bounded response, not a substring
	// match, truthy conversion, nested duplicate, or an error alongside true.
	if len(raw) > 4096 {
		return ProbeIndeterminate, errors.New("oversized test-mode response")
	}
	decoder := xml.NewDecoder(strings.NewReader(raw))
	depth, count, roots := 0, 0, 0
	value := ""
	// Held until the whole body proves to be the gateway's response.
	var unexpected error
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ProbeIndeterminate, errors.New("invalid test-mode XML")
		}
		switch element := token.(type) {
		case xml.StartElement:
			depth++
			bare := element.Name.Space == "" && len(element.Attr) == 0
			switch {
			case depth == 1:
				roots++
				if roots > 1 || !bare || element.Name.Local != "nm_response" {
					return ProbeIndeterminate, errNotGatewayResponse
				}
			case depth == 2 && bare && element.Name.Local == "test_mode_enabled":
				count++
			case unexpected == nil:
				unexpected = fmt.Errorf("%w: <%s> (namespace %q, %d attributes) at depth %d", errUnexpectedTestMode, element.Name.Local, element.Name.Space, len(element.Attr), depth)
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			text := strings.TrimSpace(string(element))
			switch {
			case text == "":
			case depth == 0:
				return ProbeIndeterminate, errNotGatewayResponse
			case depth == 2:
				value += text
			case unexpected == nil:
				unexpected = fmt.Errorf("%w: text outside <test_mode_enabled>", errUnexpectedTestMode)
			}
		case xml.Directive:
			return ProbeIndeterminate, errNotGatewayResponse
		}
	}
	if depth != 0 || roots != 1 {
		return ProbeIndeterminate, errors.New("incomplete test-mode response")
	}
	if unexpected != nil {
		return ProbeIndeterminate, unexpected
	}
	if count != 1 {
		return ProbeIndeterminate, fmt.Errorf("%w: %d <test_mode_enabled> results", errUnexpectedTestMode, count)
	}
	switch value {
	case "true":
		return ProbeSimulated, nil
	case "false":
		return ProbeLive, nil
	default:
		return ProbeIndeterminate, fmt.Errorf("%w: <test_mode_enabled> is %q", errUnexpectedTestMode, value[:min(len(value), 32)])
	}
}

// ProxyPostureClient is the posture identity of a PSP credential that a
// custodian proxy forwards. The declared environment selects the destination
// (DirectPostURL) and the official signal; it is never inferred from a URL.
// fixtureDestination (code seams and the loopback provider_sandbox only)
// replaces the destination and skips verification: OpenRails never dials it
// itself, only the custodian does.
func ProxyPostureClient(merchantID, pspID uuid.UUID, accountID string, cfg *config.NMIProviderSettings, fixtureDestination string) (*NMIClient, error) {
	client, err := NewAccountClient(merchantID, pspID, accountID, cfg)
	if err != nil {
		return nil, err
	}
	if fixtureDestination != "" {
		client.DirectPostURL = fixtureDestination
		client.proxyFixture = true
	}
	return client, nil
}

// RequireArmedFor gates a mutation that carries securityKey to destination
// through another transport (a custodian proxy).
func (c *NMIClient) RequireArmedFor(ctx context.Context, destination, securityKey string) error {
	if c == nil {
		return fmt.Errorf("%w: NMI proxy has no posture identity", providerposture.ErrDisarmed)
	}
	if !c.Live() && (destination != c.DirectPostURL || securityKey != c.SecurityKey) {
		return fmt.Errorf("%w: proxy credential or destination is not the verified NMI account", providerposture.ErrDisarmed)
	}
	if c.proxyFixture {
		return nil
	}
	return c.requireArmed(ctx, destination)
}
