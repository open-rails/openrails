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

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/providerposture"
)

type probeContextKey struct{}

// PostureKey binds a posture verdict to this client's exact merchant, PSP
// account, endpoints and security key.
func (c *NMIClient) PostureKey() providerposture.Key {
	key := providerposture.Key{
		Rail:       "nmi",
		MerchantID: c.accountMerchantID,
		PSPID:      c.accountPSPID,
		AccountID:  c.providerName,
		Endpoint:   strings.Join([]string{c.endpointDeployment, c.DirectPostURL, c.QueryURL, c.v5BaseURL()}, " "),
		Credential: providerposture.Fingerprint(c.SecurityKey),
	}
	if !c.TestMode {
		key.Expect = providerposture.Live
	}
	return key
}

// VerifyPosture runs the authoritative test-mode check now and records the
// verdict for every client that loads the same credential. It is called when
// credentials are loaded: startup, create and rotation. Under live posture
// the account must be proven live (SEC-33).
func (c *NMIClient) VerifyPosture(ctx context.Context) providerposture.Status {
	if !c.TestMode {
		return providerposture.Process().Verify(ctx, c.PostureKey(), c.CheckLivePosture)
	}
	if c.LoopbackFixture {
		return providerposture.Status{Key: c.PostureKey(), Verdict: providerposture.Simulated}
	}
	return providerposture.Process().Verify(ctx, c.PostureKey(), c.CheckPosture)
}

// PostureCheck is the check that arms this client under its posture.
func (c *NMIClient) PostureCheck() providerposture.Check {
	if !c.TestMode {
		return c.CheckLivePosture
	}
	return c.CheckPosture
}

// CheckLivePosture proves a live deployment's NMI account is not in test mode
// through the read-only test_mode_status query. A test-mode account approves
// without moving money, so it must never grant access under live posture.
func (c *NMIClient) CheckLivePosture(ctx context.Context) (providerposture.Verdict, error) {
	if c.endpointDeployment == config.NMIEndpointSandbox {
		return providerposture.Simulated, ErrSandboxEndpointUnderLive
	}
	result, err := c.readGatewayTestMode(context.WithValue(ctx, probeContextKey{}, c))
	switch {
	case err != nil:
		return providerposture.Unknown, err
	case result == ProbeLive:
		return providerposture.Live, nil
	case result == ProbeSimulated:
		return providerposture.Simulated, ErrTestModeUnderLivePosture
	default:
		return providerposture.Unknown, errors.New("NMI test-mode verification was indeterminate")
	}
}

// CheckPosture is the NMI authoritative signal: the regular gateway's
// read-only test_mode_status, or the dedicated sandbox's qualification probe.
func (c *NMIClient) CheckPosture(ctx context.Context) (providerposture.Verdict, error) {
	result, err := c.qualifyAccount(context.WithValue(ctx, probeContextKey{}, c))
	switch {
	case err != nil:
		return providerposture.Unknown, err
	case result == ProbeSimulated:
		return providerposture.Simulated, nil
	case result == ProbeLive:
		return providerposture.Live, ErrLiveCredentialsUnderTestMode
	default:
		return providerposture.Unknown, errors.New("NMI test-mode verification was indeterminate")
	}
}

// requireArmed gates every mutation: the credential must hold a cached verdict
// matching the posture (simulated under sandbox, live under live); an unseen
// credential is verified once, inline.
func (c *NMIClient) requireArmed(ctx context.Context, target string) error {
	if ctx.Value(probeContextKey{}) == c {
		return nil
	}
	if !c.TestMode {
		// SEC-33: a live credential proves it is not in test mode before its
		// first mutation — verified when loaded, or inline (bounded, once) when
		// this process has not seen it yet.
		if c.LoopbackFixture {
			return nil
		}
		return providerposture.Process().Require(ctx, c.PostureKey(), c.CheckLivePosture)
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
	return parseGatewayTestMode(raw)
}

func parseGatewayTestMode(raw string) (TestModeProbeResult, error) {
	// Deliberately accept only the documented bounded response, not a substring
	// match, truthy conversion, nested duplicate, or an error alongside true.
	if len(raw) > 4096 {
		return ProbeIndeterminate, errors.New("oversized test-mode response")
	}
	decoder := xml.NewDecoder(strings.NewReader(raw))
	depth, count, roots := 0, 0, 0
	value := ""
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
			if depth == 1 {
				roots++
				if roots > 1 {
					return ProbeIndeterminate, errors.New("multiple test-mode roots")
				}
			}
			if element.Name.Space != "" || len(element.Attr) != 0 || depth == 1 && element.Name.Local != "nm_response" || depth == 2 && element.Name.Local != "test_mode_enabled" || depth > 2 {
				return ProbeIndeterminate, errors.New("unexpected test-mode response")
			}
			if depth == 2 {
				count++
			}
		case xml.EndElement:
			depth--
		case xml.CharData:
			text := strings.TrimSpace(string(element))
			if depth == 2 {
				value += text
			} else if text != "" {
				return ProbeIndeterminate, errors.New("unexpected test-mode text")
			}
		case xml.Directive:
			return ProbeIndeterminate, errors.New("unsupported test-mode XML directive")
		}
	}
	if depth != 0 || count != 1 || roots != 1 {
		return ProbeIndeterminate, errors.New("missing or repeated test-mode result")
	}
	switch value {
	case "true":
		return ProbeSimulated, nil
	case "false":
		return ProbeLive, nil
	default:
		return ProbeIndeterminate, errors.New("unknown test-mode result")
	}
}

func (c *NMIClient) qualifyAccount(ctx context.Context) (TestModeProbeResult, error) {
	if c.endpointDeployment == config.NMIEndpointGateway {
		return c.readGatewayTestMode(ctx)
	}
	return c.ProbeTestMode(ctx)
}

// ProxyPostureClient is the posture identity of a PSP credential that a
// custodian proxy forwards. The declared endpoint deployment selects the
// destination (DirectPostURL) and the official signal; it is never inferred
// from a URL. fixtureDestination (code seams and the loopback
// provider_sandbox only) replaces the destination and skips verification:
// OpenRails never dials it itself, only the custodian does.
func ProxyPostureClient(merchantID, pspID uuid.UUID, accountID string, cfg *config.NMIProviderSettings, fixtureDestination string, testMode bool) (*NMIClient, error) {
	client, err := NewAccountClient(merchantID, pspID, accountID, cfg, testMode)
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
	if c.TestMode && (destination != c.DirectPostURL || securityKey != c.SecurityKey) {
		return fmt.Errorf("%w: proxy credential or destination is not the verified NMI account", providerposture.ErrDisarmed)
	}
	if c.proxyFixture {
		return nil
	}
	return c.requireArmed(ctx, destination)
}
