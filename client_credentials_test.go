package openrails

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestClientCredentialsCachesAndRenews(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		require.True(t, ok)
		require.Equal(t, url.QueryEscape("client: id"), id)
		require.Equal(t, url.QueryEscape("secret +:"), secret)
		require.NoError(t, r.ParseForm())
		require.Equal(t, "client_credentials", r.Form.Get("grant_type"))
		require.Equal(t, "openrails:merchant", r.Form.Get("scope"))
		require.Equal(t, "https://billing.example", r.Form.Get("resource"))
		fmt.Fprintf(w, `{"access_token":"token-%d","token_type":"Bearer","expires_in":1}`, requests.Add(1))
	}))
	defer server.Close()
	token, err := NewClientCredentialsTokenProvider(ClientCredentialsConfig{TokenURL: server.URL, ClientID: "client: id", ClientSecret: "secret +:", Resource: "https://billing.example"})
	require.NoError(t, err)
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() { got, err := token(t.Context()); require.NoError(t, err); require.Equal(t, "token-1", got) })
	}
	wg.Wait()
	require.EqualValues(t, 1, requests.Load())
	time.Sleep(time.Second)
	got, err := token(t.Context())
	require.NoError(t, err)
	require.Equal(t, "token-2", got)
}

func TestClientCredentialsWaitRespectsCancellation(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	token, err := NewClientCredentialsTokenProvider(ClientCredentialsConfig{TokenURL: server.URL, ClientID: "client", ClientSecret: "secret"})
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { _, err := token(ctx); done <- err }()
	<-entered
	waiting, stop := context.WithCancel(t.Context())
	stop()
	_, err = token(waiting)
	require.ErrorIs(t, err, context.Canceled)
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestClientCredentialsRefusesBadResponsesAndRedirects(t *testing.T) {
	for _, response := range []string{
		`{"access_token":"secret-token","token_type":"Basic","expires_in":60}`,
		`{"token_type":"Bearer","expires_in":60}`,
		`{"access_token":"secret-token","token_type":"Bearer","expires_in":-1}`,
	} {
		t.Run(response, func(t *testing.T) {
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, response) }))
			defer s.Close()
			token, err := NewClientCredentialsTokenProvider(ClientCredentialsConfig{TokenURL: s.URL, ClientID: "client", ClientSecret: "secret"})
			require.NoError(t, err)
			_, err = token(t.Context())
			require.Error(t, err)
			require.NotContains(t, err.Error(), "secret-token")
		})
	}
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer s.Close()
	token, err := NewClientCredentialsTokenProvider(ClientCredentialsConfig{TokenURL: s.URL, ClientID: "client", ClientSecret: "secret"})
	require.NoError(t, err)
	_, err = token(t.Context())
	require.ErrorContains(t, err, "307")
	require.Zero(t, targetCalls.Load())
}

func TestSingleMerchantOptionsAndPublicDiscovery(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/v1/config", r.URL.Path)
		require.Empty(t, r.Header.Get("Authorization"))
		w.Header().Set("OpenRails-Bound-Merchant", testMerchant.String())
		fmt.Fprint(w, `{"capabilities":{"route_groups":{"app":true},"features":{}}}`)
	}))
	defer server.Close()
	client, err := NewRemoteMerchant(server.URL, RemoteCredential{Token: func(context.Context) (string, error) { return "", errors.New("credential unavailable") }}, RemoteMerchantOptions{HTTPClient: server.Client(), Timeout: time.Second})
	require.NoError(t, err)
	cfg, err := client.GetPublicConfig(t.Context())
	require.NoError(t, err)
	require.True(t, cfg.Capabilities.RouteGroups["app"])
	require.True(t, client.MerchantID().IsZero(), "an anonymous discovery response cannot bind the merchant")
}
