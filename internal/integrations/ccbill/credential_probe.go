package ccbill

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// probeSubscriptionID is a well-formed subscription id no account holds.
const probeSubscriptionID = "0100000000000000001"

// CCBill locks a DataLink login out for an hour after three failed ones
// (-12). A login, the client account and the username, is probed at most
// ProbeFailures times an hour that CCBill refuses, whichever merchant or
// money class declares it, so a probe never makes the third; a stranger's
// refused probes hold only the username they named.
const (
	ProbeInterval = time.Hour
	ProbeFailures = 2
	// ProbeTimeout bounds one probe, and the lease its in-flight slot holds:
	// a probe killed in flight frees it this soon.
	ProbeTimeout = 15 * time.Second
)

// ProbeLimitedError is a probe not sent: the login's refused probes within
// ProbeInterval reached ProbeFailures, or another probe of it is in flight.
// Until is when it may be probed, Wait how long that was from the refusal.
type ProbeLimitedError struct {
	Until time.Time
	Wait  time.Duration
}

func (e *ProbeLimitedError) Error() string {
	return fmt.Sprintf("ccbill credential probe: CCBill refused this login within the hour, or it is being probed; probe again after %s", e.Until.UTC().Format(time.RFC3339))
}

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
// no hourly slot, and it changes nothing. UNCONFIRMED: the guide does not say
// that -3 or -4 binds the login to clientAccnum; a login of another account
// might answer them. The cross-merchant ownership check (one current PSP
// holds an account) bounds what such a probe can claim.
//
// The login's probe is leased for ProbeTimeout while in flight (Pacing), one
// at a time; a definite refusal (ErrDataLinkAuth: -1, -8, -9, -10, -12, an
// HTTP 401 or 403) is counted for ProbeInterval. No answer, a 5xx, a -7 or a
// canceled probe counts nothing.
func (c *DataLinkClient) ProbeCredentials(ctx context.Context) error {
	if err := c.ValidateConfig(); err != nil {
		return err
	}
	slots, now, login := c.slots(), c.now(), c.probeLogin()
	refused := make([]string, ProbeFailures)
	held, first := 0, time.Time{}
	for i := range refused {
		refused[i] = fmt.Sprintf("%s:refused:%d", login, i)
		if until, ok := slots.Opens(ctx, refused[i], now); ok {
			if held++; first.IsZero() || until.Before(first) {
				first = until
			}
		}
	}
	if held == ProbeFailures {
		return &ProbeLimitedError{Until: first, Wait: first.Sub(now)}
	}
	lease := login + ":lease"
	if until, open := slots.Claim(ctx, lease, now, now.Add(ProbeTimeout)); !open {
		return &ProbeLimitedError{Until: until, Wait: until.Sub(now)}
	}
	defer slots.Reopen(context.WithoutCancel(ctx), lease)
	probeCtx, cancel := context.WithTimeout(ctx, ProbeTimeout)
	defer cancel()
	err := c.authenticate(probeCtx)
	if errors.Is(err, ErrDataLinkAuth) {
		at, record := c.now(), context.WithoutCancel(ctx)
		for _, key := range refused {
			if _, taken := slots.Claim(record, key, at, at.Add(ProbeInterval)); taken {
				break
			}
		}
	}
	return err
}

// probeLogin is the slot key of the client's login: its account and a digest
// of its username, which is a credential.
func (c *DataLinkClient) probeLogin() string {
	sum := sha256.Sum256([]byte(c.Username))
	return "ccbill:probe:" + c.ClientAccNum + ":" + hex.EncodeToString(sum[:8])
}

func (c *DataLinkClient) authenticate(ctx context.Context) error {
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
