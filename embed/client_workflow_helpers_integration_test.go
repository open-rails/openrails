//go:build integration

package embed_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	solanaint "github.com/open-rails/openrails/internal/integrations/solana"
	"github.com/open-rails/openrails/internal/testauth"
)

// errorObservation is everything a caller can branch on, excluding the human
// message and per-request identifiers.
type errorObservation struct {
	StatusError  bool
	Status       int
	Type         string
	Code         string
	Param        string
	Metadata     map[string]any
	HasRequestID bool

	Invalid, Unauthorized, Denied, NotFound, Conflict, Internal, Unreachable bool
	InsufficientCredits, IdempotencyKeyReused, Canceled, DeadlineExceeded    bool
}

func observeClientError(t *testing.T, label string, err error) errorObservation {
	t.Helper()
	require.Error(t, err, label)
	o := errorObservation{
		Invalid:              errors.Is(err, openrails.ErrInvalid),
		Unauthorized:         errors.Is(err, openrails.ErrUnauthorized),
		Denied:               errors.Is(err, openrails.ErrDenied),
		NotFound:             errors.Is(err, openrails.ErrNotFound),
		Conflict:             errors.Is(err, openrails.ErrConflict),
		Internal:             errors.Is(err, openrails.ErrInternal),
		Unreachable:          errors.Is(err, openrails.ErrUnreachable),
		InsufficientCredits:  errors.Is(err, openrails.ErrInsufficientCredits),
		IdempotencyKeyReused: errors.Is(err, openrails.ErrIdempotencyKeyReused),
		Canceled:             errors.Is(err, context.Canceled),
		DeadlineExceeded:     errors.Is(err, context.DeadlineExceeded),
	}
	var status *openrails.StatusError
	if errors.As(err, &status) {
		o.StatusError, o.Status, o.Type, o.Code = true, status.Status, status.Type, status.Code
		o.Metadata, o.HasRequestID = status.Metadata, status.RequestID != ""
		if status.Param != nil {
			o.Param = *status.Param
		}
	}
	return o
}

// dtoShapeObservation is the id and currency spelling one deployment returned.
type dtoShapeObservation struct {
	SubscriptionID, ListedSubscriptionID, MethodSubscriptionID openrails.SubscriptionID
	CustomerID                                                 openrails.CustomerID
	ProductID, PriceProductID, ProductOwnID                    openrails.ProductID
	PriceID, PriceOwnID, CatalogPriceID                        openrails.PriceID
	PaymentMethodID, MethodID                                  openrails.PaymentMethodID
	PaymentID                                                  openrails.PaymentID
	ReadByID, HasPayment                                       bool
	DepositCurrency, BalanceCurrency, PriceCurrency            string
	RuleCurrency                                               string
}

func getRawJSON(t *testing.T, url, token string) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
	require.NoError(t, err)
	require.NoError(t, testauth.Authorize(req, token))
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	var out map[string]any
	require.NoError(t, json.Unmarshal(body, &out))
	return out
}

// errorLogHook records every error-level entry the engine emits.
type errorLogHook struct {
	mu      sync.Mutex
	entries []string
}

func (h *errorLogHook) Levels() []logrus.Level { return []logrus.Level{logrus.ErrorLevel} }

func (h *errorLogHook) Fire(e *logrus.Entry) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, e.Message)
	return nil
}

func (h *errorLogHook) drain() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := append([]string(nil), h.entries...)
	h.entries = nil
	return out
}

// microInstant is a UTC instant with a non-zero microsecond part, so a
// whole-second wire encoding would visibly lose it.
func microInstant(t time.Time) time.Time {
	t = t.UTC().Truncate(time.Microsecond)
	if t.Nanosecond() == 0 {
		t = t.Add(time.Microsecond)
	}
	return t
}

// fakeMintReader serves an initialized SPL mint at a fixed decimals count so
// the Solana projection does not depend on a chain read.
type fakeMintReader struct{ decimals uint8 }

func (r fakeMintReader) GetAccountData(context.Context, solanago.PublicKey) ([]byte, error) {
	blob := make([]byte, solanaint.MintAccountSize)
	blob[44] = r.decimals
	blob[45] = 1
	return blob, nil
}

func requireCode(t *testing.T, err error, code string) {
	t.Helper()
	var status *openrails.StatusError
	require.True(t, errors.As(err, &status), "want StatusError, got %v", err)
	require.Equal(t, code, status.Code)
}

// holdDeadline is the declared deadline every hold-placing admit must carry
// (xs-007 row 33): an hour from now, as a job would declare.
func holdDeadline() *time.Time {
	v := time.Now().Add(time.Hour)
	return &v
}
