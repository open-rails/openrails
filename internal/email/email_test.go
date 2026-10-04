package email

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/open-rails/authkit/iam"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/internal/config"
)

func sendgridAt(t *testing.T, h http.HandlerFunc) *SendGrid {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s, err := NewSendGrid(config.SendGridConfig{APIKey: " SG.test ", From: config.EmailAddress{Name: "Deployment", Address: "noreply@deploy.test"}})
	require.NoError(t, err)
	s.base = srv.URL
	return s
}

type sent struct {
	From             struct{ Email, Name string } `json:"from"`
	Personalizations []struct {
		To []struct{ Email string } `json:"to"`
	} `json:"personalizations"`
	Subject string                         `json:"subject"`
	Content []struct{ Type, Value string } `json:"content"`
}

// Concurrent sends carry their own bodies over the SendGrid wire; an empty
// From is the deployment's, a merchant's address is its own.
func TestSendGridSend(t *testing.T) {
	var mu sync.Mutex
	var bodies []sent
	s := sendgridAt(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer SG.test" || r.URL.Path != "/v3/mail/send" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var body sent
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	var wg sync.WaitGroup
	for _, from := range []config.EmailAddress{{}, {Name: "Store", Address: "billing@store.test"}} {
		wg.Go(func() {
			require.NoError(t, s.Send(context.Background(), config.Email{From: from, To: "reader@example.test", Subject: "to " + from.Address, Text: "plain", HTML: "<p>html</p>"}))
		})
	}
	wg.Wait()
	require.Len(t, bodies, 2)
	for _, b := range bodies {
		require.Equal(t, "to "+map[bool]string{true: "billing@store.test", false: ""}[b.From.Email == "billing@store.test"], b.Subject)
		require.Equal(t, "reader@example.test", b.Personalizations[0].To[0].Email)
		require.Len(t, b.Content, 2)
		if b.From.Email != "billing@store.test" {
			require.Equal(t, "noreply@deploy.test", b.From.Email)
			require.Equal(t, "Deployment", b.From.Name)
		}
	}

	refused := sendgridAt(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"errors":[{"message":"sender not verified"}]}`)
	})
	require.ErrorContains(t, refused.Send(context.Background(), config.Email{To: "a@b.test", Subject: "x", Text: "x"}), "sender not verified")
	none, err := NewSendGrid(config.SendGridConfig{APIKey: "SG.x"})
	require.NoError(t, err)
	require.ErrorContains(t, none.Send(context.Background(), config.Email{To: "a@b.test"}), "no from address")
	_, err = NewSendGrid(config.SendGridConfig{})
	require.ErrorContains(t, err, "api_key is required")
}

// A hung SendGrid never stalls the caller past its deadline.
func TestSendGridHangIsBounded(t *testing.T) {
	release := make(chan struct{})
	s := sendgridAt(t, func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	require.Equal(t, 10*time.Second, s.client.Timeout)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	require.Error(t, s.Send(ctx, config.Email{To: "a@b.test", Subject: "hung"}))
	require.Less(t, time.Since(start), 2*time.Second)
}

// Health is whether the key may send mail.
func TestSendGridHealth(t *testing.T) {
	for scopes, ok := range map[string]bool{`{"scopes":["mail.send","stats.read"]}`: true, `{"scopes":["stats.read"]}`: false} {
		s := sendgridAt(t, func(w http.ResponseWriter, r *http.Request) {
			require.Equal(t, "/v3/scopes", r.URL.Path)
			_, _ = io.WriteString(w, scopes)
		})
		require.Equal(t, ok, s.CheckHealth(context.Background()) == nil, scopes)
	}
}

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
		require.NotNil(t, m.Auth)
	}
	require.Contains(t, rec.got[0].Text, "123456")
	require.Contains(t, rec.got[1].Text, "654321")
	require.True(t, strings.Contains(rec.got[2].HTML, "https://x.test/r"))
	require.Error(t, a.Send(context.Background(), iam.EmailMessage{Kind: "unknown", To: "u@e.test"}))
	require.Error(t, a.Send(context.Background(), iam.EmailMessage{Kind: iam.MessageContactChanged, To: "u@e.test"}))
}
