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

func TestErrorClassifiesLikeStatusError(t *testing.T) {
	notFound := New(http.StatusNotFound, "thing_not_found", "thing not found")
	wrapped := fmt.Errorf("load thing: %w", notFound)
	renamed := &Error{Status: notFound.Status, Code: notFound.Code, Message: "the thing is missing"}

	for _, err := range []error{notFound, wrapped, renamed} {
		require.ErrorIs(t, err, notFound)
		require.ErrorIs(t, err, openrails.ErrNotFound)
		require.NotErrorIs(t, err, openrails.ErrInvalid)
		require.NotErrorIs(t, err, openrails.ErrInternal)
		require.NotErrorIs(t, err, context.Canceled)
		var refusal *Error
		require.True(t, errors.As(err, &refusal))
		require.Equal(t, "thing_not_found", refusal.ErrorCode())
	}

	require.NotErrorIs(t, New(http.StatusNotFound, "other_not_found", "x"), notFound)
	require.ErrorIs(t, Invalidf("bad %s", "field"), openrails.ErrInvalid)
	require.Equal(t, "bad field", Invalidf("bad %s", "field").Error())
	require.ErrorIs(t, Conflictf("busy"), openrails.ErrConflict)
	require.Equal(t, "field", Invalidf("x").WithParam("field").Param)
	require.Equal(t, "", Invalidf("x").Param, "WithParam copies")
}
