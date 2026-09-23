package nmi

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"

	"github.com/open-rails/openrails/config"
	"github.com/open-rails/openrails/internal/providerposture"
)

type probeContextKey struct{}

// PostureKey binds a posture verdict to this client's exact merchant, PSP
// account, endpoints and security key.
func (c *NMIClient) PostureKey() providerposture.Key {
	return providerposture.Key{
		Rail:       "nmi",
		MerchantID: c.accountMerchantID,
		PSPID:      c.accountPSPID,
		AccountID:  c.providerName,
		Endpoint:   strings.Join([]string{c.endpointDeployment, c.DirectPostURL, c.QueryURL, c.v5BaseURL()}, " "),
		Credential: providerposture.Fingerprint(c.SecurityKey),
	}
}

// VerifyPosture runs the authoritative test-mode check now and records the
// verdict for every client that loads the same credential. It is called when
// credentials are loaded: startup, create and rotation.
func (c *NMIClient) VerifyPosture(ctx context.Context) providerposture.Status {
	if !c.TestMode || c.LoopbackFixture {
		return providerposture.Status{Key: c.PostureKey(), Verdict: providerposture.Simulated}
	}
	return providerposture.Process().Verify(ctx, c.PostureKey(), c.CheckPosture)
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

// requireArmed gates every mutation. Under sandbox posture the credential
// must hold a cached simulated verdict; an unseen credential is verified once.
func (c *NMIClient) requireArmed(ctx context.Context, target string) error {
	if !c.TestMode || ctx.Value(probeContextKey{}) == c {
		return nil
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

// ProxyPostureClient is the posture identity of a credential that a custodian
// proxy forwards to destination. The destination selects which official
// signal applies; it never implies the verdict. A fixture (set only by code
// seams or the loopback provider_sandbox) skips verification: OpenRails never
// dials the destination itself, only the custodian.
func ProxyPostureClient(securityKey, destination string, testMode, loopbackFixture bool) (*NMIClient, error) {
	deployment := ""
	switch destination {
	case DefaultDirectPostURL, GatewayDirectPostURL:
		deployment = config.NMIEndpointGateway
	case SandboxDirectPostURL:
		deployment = config.NMIEndpointSandbox
	default:
		if testMode && !loopbackFixture {
			return nil, fmt.Errorf("%w: unrecognized NMI proxy destination %q", providerposture.ErrDisarmed, destination)
		}
	}
	client, err := NewClient("nmi-proxy", &config.NMIProviderSettings{SecurityKey: securityKey, EndpointDeployment: deployment}, testMode)
	if err != nil {
		return nil, err
	}
	client.DirectPostURL = destination
	client.proxyFixture = loopbackFixture
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
