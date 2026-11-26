package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	// listenPing is how long a listener waits for a notification before it
	// pings its session: a dead one is redialed, not waited on.
	listenPing = 30 * time.Second
	// listenPingTimeout bounds that ping.
	listenPingTimeout = 10 * time.Second
	// listenRetry and listenRetryMax bound the wait before a redial.
	listenRetry    = time.Second
	listenRetryMax = 30 * time.Second
)

// Listener is one LISTEN a worker holds. A notification only wakes the
// worker; its table is the record. Connected runs after every (re)connect,
// before any notification, so the worker reads what changed while nothing
// listened; the worker's own periodic pass covers whatever is still missed.
type Listener struct {
	// Channel is the notification channel, unquoted.
	Channel string
	// Connected runs once the session listens. Nil runs nothing.
	Connected func(ctx context.Context)
	// Notified runs for each notification on Channel.
	Notified func(ctx context.Context, payload string)
	// Interrupted reports why the session ended before a redial.
	Interrupted func(ctx context.Context, err error)
}

// Listen holds l on a session of its own until ctx ends, redialing with
// backoff after any failure, a failed ping included.
func (d *DB) Listen(ctx context.Context, l Listener) {
	wait := listenRetry
	for ctx.Err() == nil {
		listening, err := d.listenOnce(ctx, l)
		if ctx.Err() != nil {
			return
		}
		if listening {
			wait = listenRetry
		}
		if l.Interrupted != nil {
			l.Interrupted(ctx, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(2*wait, listenRetryMax)
	}
}

func (d *DB) listenOnce(ctx context.Context, l Listener) (listening bool, err error) {
	conn, err := d.ConnectSession(ctx)
	if err != nil {
		return false, err
	}
	defer func() { _ = conn.Close(context.WithoutCancel(ctx)) }()
	listen := "LISTEN " + pgx.Identifier{l.Channel}.Sanitize()
	if _, err := conn.Exec(ctx, listen); err != nil {
		return false, err
	}
	if l.Connected != nil {
		l.Connected(ctx)
	}
	for {
		waitCtx, cancel := context.WithTimeout(ctx, listenPing)
		n, err := conn.WaitForNotification(waitCtx)
		cancel()
		switch {
		case err == nil:
			if l.Notified != nil {
				l.Notified(ctx, n.Payload)
			}
		case ctx.Err() != nil:
			return true, ctx.Err()
		case errors.Is(err, context.DeadlineExceeded) && !conn.IsClosed():
			// LISTEN again is a no-op round trip: the ping, and the session
			// still reads as a listener in pg_stat_activity.
			pingCtx, cancel := context.WithTimeout(ctx, listenPingTimeout)
			_, err = conn.Exec(pingCtx, listen)
			cancel()
			if err != nil {
				return true, err
			}
		default:
			return true, err
		}
	}
}
