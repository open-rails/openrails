package db

import (
	"context"
	"fmt"

	"github.com/open-rails/openrails/pkg/merchant"
)

// ErrUnscopedMerchantWork is returned by AssertMerchantScope when the handle a
// caller is about to query carries no app.merchant_id. Callers should treat it
// as fatal for the unit of work: continuing would read zero rows (silently) or
// be denied 42501 (loudly), never do the intended thing.
type ErrUnscopedMerchantWork struct {
	Op   string
	Want merchant.ID
	Got  string
}

func (e *ErrUnscopedMerchantWork) Error() string {
	if e.Got == "" {
		return fmt.Sprintf(
			"db: %s runs UNSCOPED — the connection carries no %s, so every read of a policied table "+
				"returns zero rows and no error, and every write is denied 42501. There is no privileged "+
				"pool: wrap the work in RunInMerchantConn/MerchantTx for merchant %s, or route a genuinely "+
				"cross-merchant read through the SECURITY DEFINER helpers (migrations 0016/0021/0022) (or#868)",
			e.Op, MerchantGUC, e.Want)
	}
	return fmt.Sprintf(
		"db: %s is scoped to merchant %s but the work belongs to merchant %s — the connection's %s was "+
			"pinned by someone else; every row it touches would be the wrong merchant's (or#868)",
		e.Op, e.Got, e.Want, MerchantGUC)
}

// AssertMerchantScope verifies that the handle Qx(ctx) resolves to actually
// carries the app.merchant_id GUC, and that it names the merchant on the
// context.
//
// This exists because the failure it detects is SILENT. A GUC-less read of a
// policied table returns an empty result and no error, which is
// indistinguishable from "there was no work to do" — that is the single reason
// a whole class of background sweeps shipped, passed their tests, and never
// processed a row (#824, or#860, or#861, or#862, or#868). One cheap round trip
// at the top of an unattended unit of work converts that silence into a failure.
//
// Call it where nothing upstream pinned a connection for you: River workers,
// embedded/in-process seams, anything reachable off the HTTP request path. On
// the request path MerchantDBConnMW has already pinned, and the assertion simply
// passes.
//
// It deliberately checks the SESSION, not the Go-side context: the whole bug
// class comes from code that had a merchant in a context VALUE and believed
// that scoped the database.
func (d *DB) AssertMerchantScope(ctx context.Context, op string) error {
	if d == nil {
		return fmt.Errorf("db: AssertMerchantScope on nil DB")
	}
	var got string
	if err := d.Qx(ctx).QueryRow(ctx,
		"SELECT COALESCE(current_setting($1, true), '')", MerchantGUC,
	).Scan(&got); err != nil {
		return fmt.Errorf("db: %s could not read %s: %w", op, MerchantGUC, err)
	}
	// A merchant on the context is not required — the SESSION is what scopes the
	// statement, and some callers (fixtures, pool-level pins) carry only that.
	// When both are present they must agree, or the work belongs to one merchant
	// and the rows to another.
	want, _ := merchant.FromContext(ctx)
	if got == "" {
		return &ErrUnscopedMerchantWork{Op: op, Want: want, Got: got}
	}
	if !want.IsZero() && got != want.String() {
		return &ErrUnscopedMerchantWork{Op: op, Want: want, Got: got}
	}
	return nil
}

// RunInMerchantScope is RunInMerchantConn plus the assertion: it pins the
// connection for merchantID, proves the pin took, then runs fn. It is the one
// call an unattended per-merchant pass should make — the pin and the proof
// travel together, so a future refactor cannot drop the pin and leave a sweep
// silently processing nothing.
func (d *DB) RunInMerchantScope(ctx context.Context, merchantID merchant.ID, op string, fn func(ctx context.Context) error) error {
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
