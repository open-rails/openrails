package subscriptions

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sendgrid/sendgrid-go/helpers/mail"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails/config"
)

func sendgridAt(t *testing.T, h http.HandlerFunc) *EmailService {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	s, err := NewEmailService(&config.SendGridConfig{APIKey: "SG.test"}, nil)
	require.NoError(t, err)
	s.request.BaseURL = srv.URL + "/v3/mail/send"
	return s
}

func message(subject string) *mail.SGMailV3 {
	return mail.NewSingleEmail(mail.NewEmail("Store", "store@example.test"), subject, mail.NewEmail("", "reader@example.test"), "plain", "<p>html</p>")
}

// Concurrent sends each carry their own body over the real SendGrid wire.
func TestSendGridSendsIndependentBodies(t *testing.T) {
	var mu sync.Mutex
	var subjects []string
	s := sendgridAt(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Authorization") != "Bearer SG.test" || r.URL.Path != "/v3/mail/send" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		mu.Lock()
		subjects = append(subjects, string(body))
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
	})
	var wg sync.WaitGroup
	for _, subject := range []string{"first", "second"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.send(context.Background(), message(subject)); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	require.Len(t, subjects, 2)
	require.NotEqual(t, strings.Contains(subjects[0], `"first"`), strings.Contains(subjects[1], `"first"`))
}

// A hung SendGrid never stalls the caller past its deadline, and every call
// is bounded by the client timeout even without one.
func TestSendGridHangIsBounded(t *testing.T) {
	release := make(chan struct{})
	s := sendgridAt(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	t.Cleanup(func() { close(release) })
	require.Equal(t, sendTimeout, s.http.HTTPClient.Timeout)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	require.Error(t, s.send(ctx, message("hung")))
	require.Less(t, time.Since(start), 2*time.Second)
}
