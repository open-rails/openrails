package merchantdocs

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/destructive"
	log "github.com/sirupsen/logrus"
)

// SetDestructiveAdmissions binds the shared admission owner and the stable
// Vault fingerprint key. Configuration credentials are never unkeyed hashes.
func (c *Cache) SetDestructiveAdmissions(a *destructive.Admissions, key []byte) error {
	if a == nil || len(key) != 32 {
		return errors.New("configuration admission and stable fingerprint key are required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.admissions = a
	c.fingerprintKey = append([]byte(nil), key...)
	return nil
}

type configurationOriginKey struct{}

// WithAutonomousWrites is only for explicitly named startup/reconciliation
// code. Admission rejects a verified staff/host identity on this path.
func WithAutonomousWrites(ctx context.Context, source string) context.Context {
	return context.WithValue(ctx, configurationOriginKey{}, source)
}

type configurationEditKey struct{}
type configurationEdit struct {
	digest   string
	action   *destructive.Action
	noEffect bool
	retryCAS bool
	owner    bool // only the caller that created the receipt may prove its own CAS no-effect
}

// BeginEdit preserves one logical request through known CAS races. Its terms
// live only for this call and are immediately reduced to a keyed digest. The
// returned cleanup releases only a positively known no-effect reservation.
func (c *Cache) BeginEdit(ctx context.Context, terms any) (context.Context, func(), error) {
	c.mu.Lock()
	key := append([]byte(nil), c.fingerprintKey...)
	admissions := c.admissions
	c.mu.Unlock()
	digest, err := configurationFingerprint(key, terms)
	if err != nil {
		return ctx, nil, err
	}
	edit := &configurationEdit{digest: digest, noEffect: true}
	return context.WithValue(ctx, configurationEditKey{}, edit), func() {
		if edit.action == nil || !edit.owner || !edit.noEffect || admissions == nil {
			return
		}
		finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if err := admissions.Finish(finish, edit.action.MerchantID, edit.action.ID, destructive.OutcomeNoEffect, ""); err != nil {
			log.WithError(err).Warn("configuration edit: retaining reservation after no-effect finalization failure")
		}
	}, nil
}

// ErrWriteUncertain keeps a lost-response write reserved until source readback
// can prove its result. It never rebases an uncertain write onto a newer CAS.
var ErrWriteUncertain = errors.New("configuration write outcome is uncertain; reconcile its original revision before another edit")

func documentValue(set Set, name string) (any, int64) {
	if name == MerchantDoc {
		if !set.HasMerchant {
			return nil, 0
		}
		return set.Merchant.Value, set.Merchant.Revision
	}
	psp, custodian := ParseDoc(name)
	if psp != "" {
		if d, ok := set.PSPs[psp]; ok {
			return d.Value, d.Revision
		}
		return nil, 0
	}
	if d, ok := set.Custodians[custodian]; ok {
		return d.Value, d.Revision
	}
	return nil, 0
}

func configurationRevision(key string) (int64, error) {
	// Keys have the source revision before the first attempted write. Targets and
	// request digests remain separate fields in the audit.
	n, _, ok := strings.Cut(key, ":")
	if !ok {
		return 0, ErrWriteUncertain
	}
	return strconv.ParseInt(n, 10, 64)
}

func (c *Cache) admittedWrite(ctx context.Context, id billing.MerchantID, name string, next any, cas int64, put func() error) (Set, error) {
	if !c.Writable() {
		return Set{}, ErrReadOnly
	}
	c.mu.Lock()
	admissions := c.admissions
	key := append([]byte(nil), c.fingerprintKey...)
	c.mu.Unlock()
	// Read the durable source, never an old cached value, before deciding this is
	// cosmetic, a replay, or a new admission.
	before, err := c.source.Load(ctx, id)
	if err != nil {
		return Set{}, err
	}
	current, revision := documentValue(before, name)
	changes, err := configurationChanges(key, name, current, next)
	if err != nil {
		return Set{}, err
	}
	edit, _ := ctx.Value(configurationEditKey{}).(*configurationEdit)
	if edit == nil {
		ctx, finish, err := c.BeginEdit(ctx, next)
		if err != nil {
			return Set{}, err
		}
		defer finish()
		return c.admittedWrite(ctx, id, name, next, cas, put)
	}
	actor, actorErr := destructive.ActorFromContext(ctx)
	origin, _ := ctx.Value(configurationOriginKey{}).(string)
	if origin != "" {
		if actorErr == nil {
			return Set{}, errors.New("verified configuration command cannot become autonomous")
		}
		actor = destructive.Actor{Issuer: "openrails:automation", ID: origin}
		actorErr = nil
	}
	if admissions != nil && actorErr == nil && edit.action == nil {
		pending, found, err := admissions.FindPending(ctx, id.UUID(), name, actor, edit.digest)
		if err != nil {
			return Set{}, err
		}
		if found {
			edit.action = &pending
			edit.owner = false
			edit.noEffect = false
		}
	}
	// Cosmetic writes and exact no-ops never borrow a destructive reservation.
	// Look up a prior unresolved request first so readback can finish it.
	if len(changes) == 0 && edit.action == nil {
		if revision != cas {
			return Set{}, ErrRevisionMismatch
		}
		if current != nil {
			equal, err := configurationFingerprint(key, current)
			if err != nil {
				return Set{}, err
			}
			desired, err := configurationFingerprint(key, next)
			if err != nil {
				return Set{}, err
			}
			if equal == desired {
				return c.Reload(ctx, id)
			}
		}
		if revision != cas {
			return Set{}, ErrRevisionMismatch
		}
		return c.write(ctx, id, put)
	}
	if admissions == nil {
		return Set{}, errors.New("configuration destructive admission is not wired")
	}
	if actorErr != nil {
		return Set{}, actorErr
	}
	if edit.action != nil && !edit.retryCAS {
		original, err := configurationRevision(edit.action.Key)
		if err != nil {
			return Set{}, err
		}
		if revision != original {
			if len(changes) != 0 {
				return Set{}, ErrWriteUncertain
			}
			set, err := c.Reload(ctx, id)
			if err != nil {
				return Set{}, err
			}
			if err = admissions.Finish(ctx, id.UUID(), edit.action.ID, destructive.OutcomeSucceeded, fmt.Sprintf("revision:%d", revision)); err != nil {
				return Set{}, err
			}
			return set, nil
		}
	}
	if revision != cas {
		return Set{}, ErrRevisionMismatch
	}
	if len(changes) == 0 {
		return c.write(ctx, id, put)
	}
	if edit.action == nil {
		command := destructive.Command{MerchantID: id.UUID(), Kind: destructive.ConfigurationChange, Target: name,
			Key: fmt.Sprintf("%d:%s:%s", cas, name, edit.digest), Digest: edit.digest, Units: int64(len(changes)), Actor: actor}
		var action destructive.Action
		if origin != "" {
			action, err = admissions.AdmitAutonomous(ctx, command, origin)
		} else {
			action, err = admissions.Admit(ctx, command)
		}

		if err != nil {
			return Set{}, err
		}
		edit.action = &action
		edit.owner = !action.Replayed
	} else if int64(len(changes)) > edit.action.Units {
		return Set{}, ErrWriteUncertain
	}
	if _, err = admissions.StartPending(ctx, id.UUID(), edit.action.ID); err != nil {
		return Set{}, err
	}
	edit.noEffect = false
	edit.retryCAS = false
	putErr := put()
	set, err := Set{}, putErr
	if putErr == nil {
		set, err = c.write(ctx, id, func() error { return nil })
	}
	if errors.Is(putErr, ErrRevisionMismatch) {
		// A replaying caller did not own the shared receipt. A CAS miss is
		// proof only that this caller wrote nothing, never that the shared
		// action had no effect. Readback/finalization belongs to its owner.
		edit.noEffect = edit.owner
		edit.retryCAS = edit.owner
		return Set{}, err
	}
	finish, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	outcome := destructive.OutcomeSucceeded
	if err != nil {
		outcome = destructive.OutcomeUncertain
	}
	finishErr := admissions.Finish(finish, id.UUID(), edit.action.ID, outcome, fmt.Sprintf("revision:%d", cas+1))
	return set, errors.Join(err, finishErr)
}
