package reconcile

import (
	"context"
	"errors"
	"testing"
)

func TestOptionsUserKnown(t *testing.T) {
	ctx := context.Background()

	t.Run("nil checker treats every user as known (legacy behavior)", func(t *testing.T) {
		ok, err := Options{}.userKnown(ctx, "anyone")
		if err != nil || !ok {
			t.Fatalf("nil checker: got ok=%v err=%v, want ok=true err=nil", ok, err)
		}
	})

	t.Run("delegates to the injected checker", func(t *testing.T) {
		opts := Options{UserExists: func(_ context.Context, userID string) (bool, error) {
			return userID == "known", nil
		}}
		if ok, _ := opts.userKnown(ctx, "known"); !ok {
			t.Error("known user should be reported as existing")
		}
		if ok, _ := opts.userKnown(ctx, "ghost"); ok {
			t.Error("unknown user should be reported as not existing")
		}
	})

	t.Run("propagates checker errors", func(t *testing.T) {
		want := errors.New("lookup failed")
		opts := Options{UserExists: func(_ context.Context, _ string) (bool, error) {
			return false, want
		}}
		if _, err := opts.userKnown(ctx, "x"); !errors.Is(err, want) {
			t.Fatalf("got err=%v, want %v", err, want)
		}
	})
}
