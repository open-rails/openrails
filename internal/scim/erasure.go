package scim

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db"
	"github.com/open-rails/openrails/internal/db/gen"
	"github.com/open-rails/openrails/internal/destructive"
	"github.com/open-rails/openrails/internal/logx"
)

// An erasure (DELETE /Users/{id}, alone or in a Bulk) is a destructive
// command, destructive.EraseContact: it names the actor the route gate
// admitted (the provisioning token or the application), spends that actor's,
// its merchant's and the deployment's destructive budget, and is refused
// while a hold covers any of them.

var (
	errHeld          = errorf(http.StatusTooManyRequests, "", "erasures are held for operator review: the destructive budget is spent")
	errNoAdmissions  = errorf(http.StatusServiceUnavailable, "", "erasure is not available")
	errReprovisioned = errorf(http.StatusConflict, "", "the User was provisioned again; send the DELETE again")
)

// erasures are a Bulk request's admitted erasures, by command key, and those
// whose operation ran.
type erasures struct {
	actions map[string]destructive.Action
	ran     map[string]bool
}

// erasureCommand erases row's User as it was provisioned: a later
// provisioning of the same User is another erasure.
func erasureCommand(ctx context.Context, row gen.BillingCustomerContact) (destructive.Command, error) {
	if row.ProvisionedAt == nil {
		return destructive.Command{}, errors.New("an unprovisioned User has nothing to erase")
	}
	user, at := row.CustomerID.String(), row.ProvisionedAt.UTC().Format(time.RFC3339Nano)
	return destructive.CommandFromContext(ctx, destructive.EraseContact, user, "scim-erase:"+user+":"+at, struct{ User, Provisioned string }{user, at})
}

// deleteUser erases the User's contact, notifications and message log, and
// keeps the customer's billing. The contact row stays, without values, so an
// older report cannot restore them. batch holds a Bulk request's admissions;
// any other erasure is admitted here.
func (s *Server) deleteUser(ctx context.Context, mid billing.MerchantID, id string, batch *erasures) *Error {
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
	if action, ok := batch.prepared(command.Key); ok {
		ctx = destructive.BindPreparedActions(ctx, []destructive.Action{action})
	}
	now := s.now()
	err = destructive.RunLocal(ctx, s.DB, s.Admissions, command, func(ctx context.Context, tx pgx.Tx, _ *db.DB) (bool, error) {
		q := gen.New(tx)
		locked, err := provisioned(ctx, q, mid, id)
		if err != nil {
			return false, err
		}
		if !locked.ProvisionedAt.Equal(*row.ProvisionedAt) {
			return false, errReprovisioned
		}
		if _, err := q.UpdateCustomerContact(ctx, gen.UpdateCustomerContactParams{
			MerchantID: locked.MerchantID, CustomerID: locked.CustomerID, DirectoryUpdatedAt: latest(now, locked.DirectoryUpdatedAt),
		}); err != nil {
			return false, err
		}
		if err := q.EraseCustomerNotificationEvents(ctx, gen.EraseCustomerNotificationEventsParams{MerchantID: locked.MerchantID, CustomerID: locked.CustomerID}); err != nil {
			return false, err
		}
		return true, q.EraseCustomerMessages(ctx, gen.EraseCustomerMessagesParams{MerchantID: locked.MerchantID, CustomerID: locked.CustomerID})
	})
	return s.erasureError(ctx, err)
}

// admitErasures admits a Bulk request's erasures as one batch before any
// operation runs: over a budget, a hold opens and the request applies
// nothing. A DELETE of a User the same request creates is admitted when it
// runs; one of no provisioned User answers its own 404.
func (s *Server) admitErasures(ctx context.Context, mid billing.MerchantID, list []any) (*erasures, *Error) {
	batch := &erasures{actions: map[string]destructive.Action{}, ran: map[string]bool{}}
	var commands []destructive.Command
	for _, item := range list {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		o := lower(obj)
		method, _, _ := o.str("method")
		path, _, _ := o.str("path")
		resource, id, _ := strings.Cut(strings.TrimPrefix(path, "/"), "/")
		if !strings.EqualFold(method, http.MethodDelete) || !strings.EqualFold(resource, "Users") || id == "" || strings.HasPrefix(id, "bulkId:") {
			continue
		}
		row, e := s.getUser(ctx, mid, id)
		switch {
		case e == errNotFound:
			continue
		case e != nil:
			return nil, e
		}
		command, err := erasureCommand(ctx, row)
		if err != nil {
			s.Log.ErrorContext(ctx, "scim: erasure has no admitted actor", logx.Err(err))
			return nil, errInternal
		}
		if _, dup := batch.actions[command.Key]; dup {
			continue
		}
		batch.actions[command.Key] = destructive.Action{}
		commands = append(commands, command)
	}
	if len(commands) == 0 {
		return batch, nil
	}
	if s.Admissions == nil {
		return nil, errNoAdmissions
	}
	actions, err := s.Admissions.AdmitBatch(ctx, commands)
	if err != nil {
		return nil, s.erasureError(ctx, err)
	}
	for _, action := range actions {
		batch.actions[action.Key] = action
	}
	return batch, nil
}

// prepared is the batch's admission of key, marked as run.
func (b *erasures) prepared(key string) (destructive.Action, bool) {
	if b == nil {
		return destructive.Action{}, false
	}
	action, ok := b.actions[key]
	if ok {
		b.ran[key] = true
	}
	return action, ok
}

// release ends, as without effect, each admission whose operation never ran
// (failOnErrors stopped the request first), so it holds no budget.
func (s *Server) release(ctx context.Context, mid billing.MerchantID, batch *erasures) {
	for key, action := range batch.actions {
		if batch.ran[key] || action.Replayed || action.State != destructive.StateReserved {
			continue
		}
		if err := s.Admissions.Finish(ctx, mid.UUID(), action.ID, destructive.OutcomeNoEffect, ""); err != nil {
			s.Log.WarnContext(ctx, "scim: an erasure that never ran stays reserved", logx.Err(err), "action_id", action.ID.String())
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
	case errors.Is(err, destructive.ErrHeld):
		return errHeld
	}
	s.Log.ErrorContext(ctx, "scim: erasure failed", logx.Err(err))
	return errInternal
}
