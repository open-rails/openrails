package db

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/merchant"
)

// ErrUnscopedMerchantWork is returned by AssertMerchantScope when the handle a
// caller is about to query carries no openrails.merchant_id. Callers should treat it
// as fatal for the unit of work: SQL scoped by current_merchant_id() would
// match nothing (silently) or refuse with 42501 (loudly).
type ErrUnscopedMerchantWork struct {
	Op   string
	Want billing.MerchantID
	Got  string
}

func (e *ErrUnscopedMerchantWork) Error() string {
	if e.Got == "" {
		return fmt.Sprintf(
			"db: %s runs UNSCOPED — the connection carries no %s, so SQL scoped by current_merchant_id() "+
				"matches nothing or refuses with 42501. There is no privileged "+
				"pool: wrap the work in RunInMerchantConn/MerchantTx for merchant %s, or route a genuinely "+
				"cross-merchant read through a GenDirectory work-queue query that returns ids or aggregates only",
			e.Op, MerchantGUC, e.Want)
	}
	return fmt.Sprintf(
		"db: %s is scoped to merchant %s but the work belongs to merchant %s — the connection's %s was "+
			"pinned by someone else; every row it touches would be the wrong merchant's (or#868)",
		e.Op, e.Got, e.Want, MerchantGUC)
}

// AssertMerchantScope verifies that the handle Qx(ctx) resolves to carries the
// openrails.merchant_id GUC, naming the context's merchant when there is one.
// A read that depends on the GUC and lacks it returns nothing and no error;
// one round trip at the top of unattended work (River workers, in-process
// seams) turns that silence into a failure. It checks the session, not the
// context value.
func (d *DB) AssertMerchantScope(ctx context.Context, op string) error {
	if d == nil {
		return fmt.Errorf("db: AssertMerchantScope on nil DB")
	}
	got, err := d.Gen(ctx).CurrentSetting(ctx, MerchantGUC)
	if err != nil {
		return fmt.Errorf("db: %s could not read %s: %w", op, MerchantGUC, err)
	}
	// The session scopes the statement; some callers carry only that. When the
	// context names a merchant too, they must agree.
	want, _ := merchant.FromContext(ctx)
	if got == "" {
		return &ErrUnscopedMerchantWork{Op: op, Want: want, Got: got}
	}
	if !want.IsZero() && got != want.String() {
		return &ErrUnscopedMerchantWork{Op: op, Want: want, Got: got}
	}
	return nil
}

// RunInMerchantScope pins the connection for merchantID, asserts the pin took,
// then runs fn. Unattended per-merchant passes use it so the pin and its proof
// cannot be separated.
func (d *DB) RunInMerchantScope(ctx context.Context, merchantID billing.MerchantID, op string, fn func(ctx context.Context) error) error {
	if merchantID.IsZero() {
		return fmt.Errorf("db: %s requires a non-zero merchant id", op)
	}
	return d.RunInMerchantConn(merchant.WithID(ctx, merchantID), func(ctx context.Context) error {
		if err := d.AssertMerchantScope(ctx, op); err != nil {
			return err
		}
		return fn(ctx)
	})
}
