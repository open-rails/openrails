package email

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

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
