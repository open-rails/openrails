package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
)

type recorder struct{ got []config.Email }

func (r *recorder) Send(_ context.Context, m config.Email) error {
	r.got = append(r.got, m)
	return nil
}
func (*recorder) CheckHealth(context.Context) error { return nil }

// The control plane's AuthKit messages reach the one sender rendered, from
// the deployment, with the message attached.
func TestAuthKitMessagesRender(t *testing.T) {
	rec := &recorder{}
	a := AuthKitSender{Sender: rec}
	for _, msg := range []iam.EmailMessage{
		{Kind: iam.MessageVerification, To: "u@e.test", Code: "123456", Link: "https://x.test/v"},
		{Kind: iam.MessageLoginCode, To: "u@e.test", Code: "654321"},
		{Kind: iam.MessagePasswordReset, To: "u@e.test", Link: "https://x.test/r"},
		{Kind: iam.MessageInvite, To: "u@e.test", Link: "https://x.test/i"},
		{Kind: iam.MessageWelcome, To: "u@e.test"},
		{Kind: iam.MessageContactChanged, To: "u@e.test", ContactChange: &iam.ContactChange{Field: iam.ContactEmail, NewValue: "n@e.test"}},
		{Kind: iam.MessageDeviceKeyEnrolled, To: "u@e.test", DeviceKey: &iam.DeviceKeyNotice{Label: "Laptop", CreatedAt: time.Now()}},
		{Kind: iam.MessageMFAReset, To: "u@e.test"},
	} {
		require.NoError(t, a.Send(context.Background(), msg), msg.Kind)
	}
	require.Len(t, rec.got, 8)
	for _, m := range rec.got {
		require.Equal(t, "u@e.test", m.To)
		require.Empty(t, m.From, "the deployment's own address")
		require.NotEmpty(t, m.Subject)
		require.NotEmpty(t, m.Text)
	}
	require.Contains(t, rec.got[0].Text, "123456")
	require.Contains(t, rec.got[1].Text, "654321")
	require.True(t, strings.Contains(rec.got[2].HTML, "https://x.test/r"))
	require.Error(t, a.Send(context.Background(), iam.EmailMessage{Kind: "unknown", To: "u@e.test"}))
	require.Error(t, a.Send(context.Background(), iam.EmailMessage{Kind: iam.MessageContactChanged, To: "u@e.test"}))
}
