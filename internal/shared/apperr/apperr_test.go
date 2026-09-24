package apperr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
)

// A service refusal classifies exactly like the StatusError a Client receives.
func TestErrorClassifiesLikeStatusError(t *testing.T) {
	notFound := New(http.StatusNotFound, "thing_not_found", "thing not found")
	for _, err := range []error{notFound, fmt.Errorf("load: %w", notFound), &Error{Status: http.StatusNotFound, Code: "thing_not_found", Message: "reworded"}} {
		require.ErrorIs(t, err, notFound, "identity is status+code, never the message")
		require.ErrorIs(t, err, openrails.ErrNotFound)
		for _, other := range []error{openrails.ErrInvalid, openrails.ErrInternal, context.Canceled} {
			require.NotErrorIs(t, err, other)
		}
		var refusal *Error
		require.True(t, errors.As(err, &refusal))
		require.Equal(t, "thing_not_found", refusal.ErrorCode())
	}
	require.NotErrorIs(t, New(http.StatusNotFound, "other_not_found", "x"), notFound)
	require.NotErrorIs(t, New(http.StatusGone, "thing_not_found", "x"), notFound)

	invalid := Invalidf("bad %s", "field")
	require.ErrorIs(t, invalid, openrails.ErrInvalid)
	require.Equal(t, "bad field", invalid.Error())
	require.ErrorIs(t, Conflictf("busy"), openrails.ErrConflict)
	require.ErrorIs(t, New(http.StatusServiceUnavailable, "down", ""), openrails.ErrInternal)
	require.Equal(t, "down", New(http.StatusServiceUnavailable, "down", "").Error(), "an empty message falls back to the code")

	named := invalid.WithParam("field")
	require.Equal(t, "field", named.Param)
	require.Empty(t, invalid.Param, "WithParam copies")
	require.ErrorIs(t, named, invalid)
}
