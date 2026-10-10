//go:build e2e && integration

package subscriptions_test

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/open-rails/helpers/smtp/smtptest"
	"github.com/open-rails/helpers/userinfo"
	"github.com/riverqueue/river"
	"github.com/stretchr/testify/require"

	"github.com/open-rails/openrails"
	"github.com/open-rails/openrails/openrailstest"
)

// smtpGate holds each armed message at RCPT until want messages are held or
// wait passes, so senders racing for one notification all reach the server
// before any of them finishes.
type smtpGate struct {
	want  int
	wait  time.Duration
	armed atomic.Bool
	mu    sync.Mutex
	held  int
	all   chan struct{}
}

func newSMTPGate(want int, wait time.Duration) *smtpGate {
	return &smtpGate{want: want, wait: wait, all: make(chan struct{})}
}

func (g *smtpGate) hold(string) string {
	if !g.armed.Load() {
		return ""
	}
	g.mu.Lock()
	g.held++
	if g.held == g.want {
		close(g.all)
	}
	g.mu.Unlock()
	select {
	case <-g.all:
	case <-time.After(g.wait):
	}
	return ""
}

func (g *smtpGate) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held
}

// A receipt is emailed once while every replica delivers it at the same
// moment: the job its payment queued, and the email sweep on each replica.
func TestReplicasEmailAReceiptOnce(t *testing.T) {
	t.Parallel()
	gate := newSMTPGate(4, 3*time.Second)
	srv := smtptest.Start(t, smtptest.Options{Username: "apikey", Password: "SG.e2e-key", RefuseRecipient: gate.hold})
	directory := &openrailstest.UserInfo{}
	f := startFleet(t, 3, false, nil, func(w *world) {
		w.cfg = func(c *openrails.Config) {
			c.SMTP = &openrails.SMTPConfig{Host: srv.Host, Port: srv.Port, Username: "apikey", Password: "SG.e2e-key",
				From: openrails.EmailAddress{Name: "Merchant Billing", Address: "noreply@deploy.test"}}
		}
		w.deps = func(d *openrails.Deps) { d.UserInfo = directory }
	})
	a := f.named("a")
	life := a.lifetime("replicas:receipt", 25_000_000)
	c := a.newCustomer()
	const to = "replicas@host.test"
	directory.Put(userinfo.User{ID: c.id, Email: to, Username: "replicas"})
	card := c.saveCard("nmi", visa)

	gate.armed.Store(true)
	paid := c.order(http.MethodPost, "/orders", "buy-"+uuid.NewString(), map[string]any{"lines": []any{line(life, 0)}, "expected_total": micros(25_000_000), "payment": map[string]any{"payment_method_id": card}})
	require.Equal(t, "complete", paid.body["status"], "%v", paid.body)
	require.Eventually(t, func() bool { return gate.count() > 0 }, 20*time.Second, 10*time.Millisecond, "the receipt's own job is sending it")

	var sweeps []pass
	for _, r := range f.live() {
		res, err := f.any().jobs.Insert(t.Context(), emailSweep{}, &river.InsertOpts{Queue: r.replica.queue})
		require.NoError(t, err)
		sweeps = append(sweeps, pass{r: r, id: res.Job.ID})
	}
	f.waitPassJobs(sweeps)
	a.mailSettled()

	var sent int
	for _, m := range srv.Messages() {
		if len(m.To) == 1 && m.To[0] == to {
			sent++
		}
	}
	require.Equal(t, 1, sent, "one receipt, however many replicas deliver it")
	require.Len(t, c.receipts(), 1)
}
