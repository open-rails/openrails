package nmi

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/open-rails/openrails/config"
)

// ErrTestModeNotQualified proves the customer request was not dispatched.
// It is distinct from an ambiguous provider mutation outcome.
var ErrTestModeNotQualified = errors.New("NMI test-mode dispatch qualification refused")

type dispatchQualificationKey struct{}
type probeQualificationKey struct{}
type dispatchBinding struct {
	client                                              *NMIClient
	merchant, psp                                       uuid.UUID
	securityKey, directURL, queryURL, v5URL, deployment string
}
type dispatchQualification struct {
	binding  dispatchBinding
	consumed atomic.Bool
}

func (c *NMIClient) dispatchBinding() dispatchBinding {
	return dispatchBinding{c, c.accountMerchantID, c.accountPSPID, c.SecurityKey, c.DirectPostURL, c.QueryURL, c.V5BaseURL, c.endpointDeployment}
}

// QualifyDispatch runs immediately before a durable submission fence. Its
// private capability authorizes exactly the next mutation on this captured
// account/credential/deployment, in this call only. It is never persisted.
func (c *NMIClient) QualifyDispatch(ctx context.Context) (context.Context, error) {
	if !c.TestMode || !c.endpointDeploymentExplicit || c.accountMerchantID == uuid.Nil || c.accountPSPID == uuid.Nil {
		return ctx, nil
	}
	if c.ReadOnly {
		return ctx, ErrProviderReadOnly
	}
	binding := c.dispatchBinding()
	if err := CheckTestModeArm(ctx, c); err != nil {
		return ctx, fmt.Errorf("%w: %w", ErrTestModeNotQualified, err)
	}
	if binding != c.dispatchBinding() {
		return ctx, fmt.Errorf("%w: account configuration changed", ErrTestModeNotQualified)
	}
	return context.WithValue(ctx, dispatchQualificationKey{}, &dispatchQualification{binding: binding}), nil
}

func (c *NMIClient) qualifyMutation(ctx context.Context) error {
	if !c.TestMode || !c.endpointDeploymentExplicit || c.accountMerchantID == uuid.Nil || c.accountPSPID == uuid.Nil || ctx.Value(probeQualificationKey{}) == c {
		return nil
	}
	if permit, ok := ctx.Value(dispatchQualificationKey{}).(*dispatchQualification); ok {
		if permit.binding != c.dispatchBinding() {
			return fmt.Errorf("%w: account configuration changed", ErrTestModeNotQualified)
		}
		if permit.consumed.CompareAndSwap(false, true) {
			return nil
		}
	}
	_, err := c.QualifyDispatch(ctx)
	return err
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
	if c.endpointDeploymentExplicit && c.endpointDeployment == config.NMIEndpointGateway {
		return c.readGatewayTestMode(ctx)
	}
	return c.ProbeTestMode(ctx)
}
