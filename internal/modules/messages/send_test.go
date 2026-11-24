package messages

import (
	"context"
	"errors"
	"fmt"
	"net/textproto"
	"testing"
)

// TestOnlyARecipientRefusalIsFinal reads errors as helpers/smtp words them.
func TestOnlyARecipientRefusalIsFinal(t *testing.T) {
	reply := func(code int) error { return &textproto.Error{Code: code, Msg: "refused"} }
	for _, c := range []struct {
		err  error
		want refusal
	}{
		{fmt.Errorf("smtp: RCPT TO: %w", reply(550)), recipientRefused},
		{fmt.Errorf("deliver: %w", fmt.Errorf("smtp: RCPT TO: %w", reply(553))), recipientRefused},
		{fmt.Errorf("smtp: RCPT TO: %w", reply(450)), retryable},
		{fmt.Errorf("smtp: MAIL FROM: %w", reply(550)), senderRefused},
		{fmt.Errorf("smtp: AUTH: %w", reply(535)), senderRefused},
		{fmt.Errorf("smtp: AUTH: %w", reply(454)), retryable},
		{fmt.Errorf("smtp: greeting: %w", reply(554)), retryable},
		{fmt.Errorf("smtp: DATA: %w", reply(552)), retryable},
		{fmt.Errorf("smtp: EHLO: %w", reply(500)), retryable},
		{fmt.Errorf("smtp: %w", context.DeadlineExceeded), retryable},
		{errors.New("a host's sender failed"), retryable},
	} {
		if got, _ := refusalOf(c.err); got != c.want {
			t.Errorf("%v: %v, want %v", c.err, got, c.want)
		}
	}
}
