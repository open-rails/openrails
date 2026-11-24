package scim

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/merchant"
)

// An erasure (DELETE /Users/{id}, alone or in a Bulk) is a destructive
// command, destructive.EraseContact, admitted to the merchant's erasure scope
// (destructive.AdmitErasure): it names the token or application that asked.
// The contact's values go at once either way: the directory can push them
// again, so that part is reversible. What cannot be undone, the customer's
// notification events and message log, runs at once within the scope's
// budget, and otherwise waits for the queue (DrainErasures). SCIM answers 204
// and then 404 for the User; a report older than the erasure loses to it.

var errNoAdmissions = errorf(http.StatusServiceUnavailable, "", "erasure is not available")

const erasurePrefix = "scim-erase:"

// erasureCommand erases row's User as it was provisioned: a later
// provisioning of the same User is another erasure.
func erasureCommand(ctx context.Context, row gen.BillingCustomerContact) (destructive.Command, error) {
	if row.ProvisionedAt == nil {
		return destructive.Command{}, errors.New("an unprovisioned User has nothing to erase")
	}
	user, at := row.CustomerID.String(), strconv.FormatInt(row.ProvisionedAt.UnixNano(), 10)
	return destructive.CommandFromContext(ctx, destructive.EraseContact, user, erasurePrefix+user+":"+at, struct{ User, Provisioned string }{user, at})
}

// erasureTarget is the User and provisioning an erasure command names.
func erasureTarget(action destructive.Action) (uuid.UUID, time.Time, bool) {
	parts := strings.Split(strings.TrimPrefix(action.Key, erasurePrefix), ":")
	if !strings.HasPrefix(action.Key, erasurePrefix) || len(parts) < 2 {
		return uuid.Nil, time.Time{}, false
	}
	user, err := uuid.Parse(parts[0])
	nanos, perr := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || perr != nil {
		return uuid.Nil, time.Time{}, false
	}
	return user, time.Unix(0, nanos).UTC(), true
}

// deleteUser erases the User's contact, notifications and message log, and
// keeps the customer's billing, at once or once its erasure's turn comes.
func (s *Server) deleteUser(ctx context.Context, mid billing.MerchantID, id string) *Error {
	if s.Admissions == nil {
		return errNoAdmissions
	}
	row, e := s.getUser(ctx, mid, id)
	if e != nil {
		return e
	}
	command, err := erasureCommand(ctx, row)
	if err != nil {
		s.Log.ErrorContext(ctx, "scim: erasure has no admitted actor", logx.Err(err))
		return errInternal
	}
	action, err := s.Admissions.AdmitErasure(ctx, command)
	if err == nil {
		switch action.State {
		case destructive.StateStarted, destructive.StateUncertain:
			err = RunErasure(ctx, s.DB, s.Admissions, action)
		case destructive.StateReserved:
			err = withdraw(ctx, s.DB, action)
		}
	}
	return s.erasureError(ctx, err)
}

// withdraw does a waiting erasure's reversible part at once: the User leaves
// the directory and its contact's values go.
func withdraw(ctx context.Context, d *db.DB, action destructive.Action) error {
	user, episode, ok := erasureTarget(action)
	if !ok {
		return errors.New("scim: an erasure names no User")
	}
	return d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.GetCustomerContactForUpdate(ctx, gen.GetCustomerContactForUpdateParams{MerchantID: action.MerchantID, CustomerID: user})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return nil
		case err != nil:
			return err
		}
		return clearContact(ctx, q, row, episode, action.AdmittedAt)
	})
}

// clearContact empties the contact of the User provisioned at episode, dated
// by the erasure's arrival so an older report cannot restore it. A User
// provisioned again since is a newer report and keeps its contact.
func clearContact(ctx context.Context, q *gen.Queries, row gen.BillingCustomerContact, episode, at time.Time) error {
	if row.ProvisionedAt != nil && !row.ProvisionedAt.Equal(episode) {
		return nil
	}
	_, err := q.UpdateCustomerContact(ctx, gen.UpdateCustomerContactParams{
		MerchantID: row.MerchantID, CustomerID: row.CustomerID, DirectoryUpdatedAt: latest(at, row.DirectoryUpdatedAt),
	})
	return err
}

// RunErasure runs a started erasure and records its outcome with it: the
// contact as withdraw clears it, and the customer's notification events and
// message log, which a later provisioning does not take back.
func RunErasure(ctx context.Context, d *db.DB, a *destructive.Admissions, action destructive.Action) error {
	user, episode, ok := erasureTarget(action)
	if !ok {
		return errors.New("scim: an erasure names no User")
	}
	ctx = merchant.WithID(ctx, billing.MerchantID(action.MerchantID))
	return d.MerchantTx(ctx, func(ctx context.Context, tx pgx.Tx) error {
		q := gen.New(tx)
		row, err := q.GetCustomerContactForUpdate(ctx, gen.GetCustomerContactForUpdateParams{MerchantID: action.MerchantID, CustomerID: user})
		switch {
		case errors.Is(err, pgx.ErrNoRows):
		case err != nil:
			return err
		default:
			if err := clearContact(ctx, q, row, episode, action.AdmittedAt); err != nil {
				return err
			}
		}
		if err := q.EraseCustomerNotificationEvents(ctx, gen.EraseCustomerNotificationEventsParams{MerchantID: action.MerchantID, CustomerID: user}); err != nil {
			return err
		}
		if err := q.EraseCustomerMessages(ctx, gen.EraseCustomerMessagesParams{MerchantID: action.MerchantID, CustomerID: user}); err != nil {
			return err
		}
		return a.FinishTx(ctx, tx, action.MerchantID, action.ID, destructive.OutcomeSucceeded, "")
	})
}

// DrainErasures runs the merchant's waiting erasures as far as its erasure
// scope's budget allows while the scope is unheld. Waiting reports any left
// for a later pass.
func DrainErasures(ctx context.Context, d *db.DB, a *destructive.Admissions, mid billing.MerchantID) (bool, error) {
	for {
		actions, waiting, err := a.StartErasures(ctx, mid.UUID(), 100)
		if err != nil {
			return false, err
		}
		for _, action := range actions {
			if err := RunErasure(ctx, d, a, action); err != nil {
				return true, err
			}
		}
		if !waiting || len(actions) == 0 {
			return waiting, nil
		}
	}
}

// erasureError is the SCIM error an erasure answers.
func (s *Server) erasureError(ctx context.Context, err error) *Error {
	var refused *Error
	switch {
	case err == nil:
		return nil
	case errors.As(err, &refused):
		return refused
	}
	s.Log.ErrorContext(ctx, "scim: erasure failed", logx.Err(err))
	return errInternal
}
