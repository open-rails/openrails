package ccbill

import (
	"context"
	"fmt"
	"strings"
)

// probeSubscriptionID is a well-formed subscription id no account holds.
const probeSubscriptionID = "0100000000000000001"

// probeRefusals are the subscription-management answers that refuse the
// login (CCBill API Guide, "Success and Error Codes").
var probeRefusals = map[string]string{
	"-1":  "CCBill refused the login",
	"-8":  "this deployment's address is outside the DataLink user's allowed range",
	"-9":  "the account may not use DataLink for this action",
	"-10": "DataLink is not set up for this account at this level",
	"-12": "three failed logins in the last hour: CCBill accepts the next in an hour",
}

// ProbeCredentials proves the DataLink login by a status read of a
// subscription no account holds. CCBill's API Guide answers -3 (no record)
// or -4 (another account's) only to an authenticated login, -1 to one it
// refuses. Subscription management is no DataLink export, so the probe takes
// no hourly slot, whoever names the account, and it changes nothing.
func (c *DataLinkClient) ProbeCredentials(ctx context.Context) error {
	if err := c.ValidateConfig(); err != nil {
		return err
	}
	body, err := c.postSubscriptionManagement(ctx, actionViewSubscriptionStatus, probeSubscriptionID)
	if err != nil {
		return fmt.Errorf("ccbill credential probe: %w", err)
	}
	fields, code, err := parseSubscriptionManagementBody(body)
	if err != nil {
		return fmt.Errorf("ccbill credential probe: %w", err)
	}
	if _, found := fields["subscriptionStatus"]; found {
		return nil // a record: the login authenticated
	}
	if code == "" {
		code = fields["results"]
	}
	code = strings.TrimSpace(strings.Trim(code, `"`))
	switch {
	case code == "-3" || code == "-4":
		return nil
	case probeRefusals[code] != "":
		return fmt.Errorf("ccbill credential probe: %w: results=%s: %s", ErrDataLinkAuth, code, probeRefusals[code])
	case code == "-7":
		return fmt.Errorf("ccbill credential probe: %w: results=-7: an internal error, or a refused login", ErrDataLinkIndeterminate)
	default:
		return fmt.Errorf("ccbill credential probe: subscription management answered results=%q", code)
	}
}
