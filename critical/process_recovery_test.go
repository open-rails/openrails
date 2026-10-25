//go:build integration

package critical_test

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jonboulle/clockwork"
	"github.com/open-rails/authkit"
	"github.com/open-rails/authkit/authtest"
	"github.com/open-rails/authkit/iam"
	"github.com/open-rails/authkit/keys"
	riverhelpers "github.com/open-rails/helpers/river"
	"github.com/open-rails/openrails"
	openrailshttp "github.com/open-rails/openrails/adapters/http"
	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/stripemock"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/rivertype"
	"github.com/stretchr/testify/require"
)

// The provider owns this barrier: acceptance and the independent economic
// journal are committed before the application receives any response.
type wireGate struct {
	arrived, release chan struct{}
	once, released   sync.Once
}

func (g *wireGate) unblock() { g.released.Do(func() { close(g.release) }) }

type workerProcessConfig struct {
	Config                             openrails.Config
	At                                 time.Time
	StripeURL, FXURL, Ready, ClockFile string
	NMIURL                             string
	AfterWork                          string
	IgnoreIntents                      []string
	StopReference                      string
	BeforeStop                         bool
	HTTPAuthTenant                     iam.TenantID
	HTTPAuthPrivate                    []byte
}

// River calls this public hook after the real worker returns and before its
// completion is persisted. It changes neither the worker nor its result.
type afterWorkBarrier struct {
	river.HookDefaults
	path   string
	ignore map[string]bool
	once   sync.Once
}

func (h *afterWorkBarrier) WorkEnd(ctx context.Context, job *rivertype.JobRow, workErr error) error {
	if workErr != nil || job.Kind != "openrails.provider_operation" {
		return workErr
	}
	var args struct {
		IntentID string `json:"intent_id"`
	}
	if err := json.Unmarshal(job.EncodedArgs, &args); err != nil {
		return err
	}
	if args.IntentID == "" || h.ignore[args.IntentID] {
		return workErr
	}
	var saveErr error
	h.once.Do(func() {
		var data []byte
		data, saveErr = json.Marshal(job)
		if saveErr == nil {
			saveErr = os.WriteFile(h.path, data, 0600)
		}
	})
	if saveErr != nil {
		return saveErr
	}
	<-ctx.Done()
	return workErr
}

// This is a subprocess entry point, not a scenario. Only the parent scenarios
// can supply its private configuration, external wires and disposable schema.
func TestWorkerProcess(t *testing.T) {
	path := os.Getenv("OPENRAILS_CRITICAL_CHILD_FILE")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var config workerProcessConfig
	require.NoError(t, json.Unmarshal(data, &config))
	pool, err := pgxpool.New(t.Context(), os.Getenv("OPENRAILS_CRITICAL_DSN"))
	require.NoError(t, err)
	defer pool.Close()
	stripe, err := url.Parse(config.StripeURL)
	require.NoError(t, err)
	fx, err := url.Parse(config.FXURL)
	require.NoError(t, err)
	clock := clockwork.NewFakeClockAt(config.At)
	wire := http.DefaultTransport.(*http.Transport).Clone()
	defer wire.CloseIdleConnections()
	deps := openrails.Deps{Postgres: pool, Clock: clock, StripeTransport: loopbackWire{target: stripe, wire: wire}, FXTransport: loopbackWire{target: fx, wire: wire}}
	if config.NMIURL != "" {
		nmi, err := url.Parse(config.NMIURL)
		require.NoError(t, err)
		deps.NMITransport = loopbackWire{target: nmi, wire: wire}
	}
	var realAuth *authkit.Client
	if len(config.HTTPAuthPrivate) != 0 {
		roles := authkit.NewRoles(authkit.APIKeys)
		merchant := roles.Persona(billing.MerchantGroupPersona, authkit.APIKeys, authkit.CustomRoles)
		for _, permission := range billing.Permissions() {
			if permission.Persona() == billing.MerchantGroupPersona {
				merchant.Declare(string(permission))
			} else {
				roles.Root.Declare(string(permission))
			}
		}
		signer, err := keys.SignerFromKey("critical-issuer", ed25519.PrivateKey(config.HTTPAuthPrivate))
		require.NoError(t, err)
		realAuth, _ = authtest.New(t, authtest.WithConfig(func(c *authkit.Config) {
			c.DefaultTenantID = config.HTTPAuthTenant
			c.Database.Schema, c.Database.RiverSchema = config.Config.Database.Schema+"_auth", config.Config.Database.Schema+"_auth"
			c.Roles = roles
			c.TwoFactor.Mode = iam.TwoFactorDisabled
		}), authtest.WithDeps(func(d *authkit.Deps) {
			d.Postgres = pool
			d.KeySource = keys.Static{Active: signer, Public: map[string]crypto.PublicKey{signer.KID(): signer.Public()}}
		}))
		deps.UserInfo = realAuth.UserInfo()
	}
	engine, err := openrails.New(t.Context(), config.Config, deps)
	require.NoError(t, err)
	defer engine.Close(context.Background()) //nolint:errcheck
	w := &world{pool: pool, schema: config.Config.Database.Schema, engine: engine, clock: clock}
	ready := "running"
	if realAuth != nil {
		hostFleet(t, w) // accepts durable jobs without executing them in the request process
		mux := http.NewServeMux()
		require.NoError(t, openrailshttp.Mount(mux, engine, openrails.Routes{Auth: realAuth.Authenticator()}))
		server := httptest.NewServer(mux)
		defer server.Close()
		ready = server.URL
	} else if config.StopReference != "" {
		if !config.BeforeStop {
			client, err := engine.MerchantBySlug("alpha")
			require.NoError(t, err)
			_, err = client.StopRunningCharge(t.Context(), config.StopReference, billing.StopRunningChargeParams{StoppedAt: &config.At})
			require.NoError(t, err)
		}
	} else if config.AfterWork == "" {
		startFleet(t, w)
	} else {
		ignored := map[string]bool{}
		for _, id := range config.IgnoreIntents {
			ignored[id] = true
		}
		fleet, err := riverhelpers.New(t.Context(), pool, &river.Config{
			Schema: w.schema,
			Queues: map[string]river.QueueConfig{openrails.QueueBilling: {MaxWorkers: 2}},
			Hooks:  []rivertype.Hook{&afterWorkBarrier{path: config.AfterWork, ignore: ignored}},
		}, engine.RiverJobs())
		require.NoError(t, err)
		require.NoError(t, engine.Start(t.Context(), openrails.WithRiverClient(fleet)))
		require.NoError(t, fleet.Start(t.Context()))
	}
	require.NoError(t, os.WriteFile(config.Ready, []byte(ready), 0600))
	// This control changes only the declared clock, never financial state.
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-t.Context().Done():
			return
		case <-tick.C:
			data, err := os.ReadFile(config.ClockFile)
			if err != nil {
				continue
			}
			at, err := time.Parse(time.RFC3339Nano, string(data))
			if err != nil {
				continue
			}
			if at.After(clock.Now()) {
				clock.Advance(at.Sub(clock.Now()))
			}
			require.NoError(t, os.WriteFile(config.ClockFile+".ack", data, 0600))
		}
	}
}

type ownedWorkerProcess struct {
	command   *exec.Cmd
	done      chan error
	stopped   bool
	clockFile string
	url       string
}

func startWorkerProcess(t *testing.T, w *world, p *providerLedger, configure ...func(*workerProcessConfig)) *ownedWorkerProcess {
	t.Helper()
	dir := t.TempDir()
	config := workerProcessConfig{Config: w.cfg, At: w.clock.Now(), FXURL: w.deps.FXTransport.(loopbackWire).target.String(), Ready: filepath.Join(dir, "ready"), ClockFile: filepath.Join(dir, "clock")}
	if p != nil {
		config.StripeURL = p.server.URL
	}
	if nmi, ok := w.deps.NMITransport.(loopbackWire); ok {
		config.NMIURL = nmi.target.String()
	}
	for _, change := range configure {
		change(&config)
	}
	data, err := json.Marshal(config)
	require.NoError(t, err)
	configPath := filepath.Join(dir, "config.json")
	require.NoError(t, os.WriteFile(configPath, data, 0600))
	binary, err := os.Executable()
	require.NoError(t, err)
	logPath := filepath.Join(dir, "worker.log")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY, 0600)
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })
	command := exec.Command(binary, "-test.run=^TestWorkerProcess$", "-test.timeout=3m", "-test.v")
	command.Env = append(os.Environ(), "OPENRAILS_CRITICAL_CHILD_FILE="+configPath, "GOMAXPROCS=2")
	command.Stdout, command.Stderr = log, log
	require.NoError(t, command.Start())
	child := &ownedWorkerProcess{command: command, done: make(chan error, 1), clockFile: config.ClockFile}
	go func() { child.done <- command.Wait() }()
	t.Cleanup(func() {
		if !child.stopped {
			child.kill(t)
		}
		if t.Failed() {
			data, _ := os.ReadFile(logPath)
			t.Logf("owned worker log: %s", data)
		}
	})
	deadline := time.After(30 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case err := <-child.done:
			child.stopped = true
			data, _ := os.ReadFile(logPath)
			t.Fatalf("owned worker exited before readiness: %v\n%s", err, data)
		case <-tick.C:
			if data, err := os.ReadFile(config.Ready); err == nil {
				child.url = string(data)
				t.Logf("owned worker PID %d running", command.Process.Pid)
				return child
			}
		case <-deadline:
			t.Fatal("owned worker did not become ready")
		}
	}
}

func (p *ownedWorkerProcess) advance(t *testing.T, at time.Time) {
	t.Helper()
	value := at.Format(time.RFC3339Nano)
	require.NoError(t, os.WriteFile(p.clockFile, []byte(value), 0600))
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(p.clockFile + ".ack")
		return err == nil && string(data) == value
	}, 5*time.Second, 20*time.Millisecond)
}

func (p *ownedWorkerProcess) kill(t *testing.T) {
	t.Helper()
	if p.stopped {
		return
	}
	require.NoError(t, p.command.Process.Kill())
	select {
	case err := <-p.done:
		p.stopped = true
		var exit *exec.ExitError
		require.True(t, errors.As(err, &exit), "owned process must actually die")
		require.Equal(t, syscall.SIGKILL, exit.ProcessState.Sys().(syscall.WaitStatus).Signal())
		t.Logf("verified SIGKILL of owned worker PID %d", p.command.Process.Pid)
	case <-time.After(10 * time.Second):
		t.Fatal("owned worker did not terminate after SIGKILL")
	}
}

type replayedOperation struct{ Payload json.RawMessage }

func (replayedOperation) Kind() string                   { return "openrails.provider_operation" }
func (j replayedOperation) MarshalJSON() ([]byte, error) { return j.Payload, nil }

func TestProcessRecovery(t *testing.T) {
	t.Run("OR-J14-S05/settled-renewal-survives-death-before-queue-ack", func(t *testing.T) {
		a, p, sub := purchasedMembership(t)
		due := *sub.CurrentPeriodEndsAt
		a.clock.Advance(due.Sub(a.clock.Now()))
		queue := hostFleet(t, a.world)
		prior, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
		require.NoError(t, err)
		var ignored []string
		for _, job := range prior.Jobs {
			var args map[string]string
			require.NoError(t, json.Unmarshal(job.EncodedArgs, &args))
			ignored = append(ignored, args["intent_id"])
		}
		atCommit := filepath.Join(t.TempDir(), "after-work.json")
		child := startWorkerProcess(t, a.world, p, func(c *workerProcessConfig) {
			c.AfterWork, c.IgnoreIntents = atCommit, ignored
		})
		_, err = queue.Insert(t.Context(), registeredJob{KindName: "openrails.dunning"}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		var operation rivertype.JobRow
		require.Eventually(t, func() bool {
			data, err := os.ReadFile(atCommit)
			return err == nil && json.Unmarshal(data, &operation) == nil
		}, 20*time.Second, 20*time.Millisecond, "real worker did not finish at the before-ACK barrier")
		renewalSettled(t, a, p, sub, due)
		job, err := queue.JobGet(t.Context(), operation.ID)
		require.NoError(t, err)
		require.Equal(t, rivertype.JobStateRunning, job.State, "local money committed before River acknowledgment")
		child.kill(t)
		startWorkerProcess(t, a.world, p)
		replay, err := queue.Insert(t.Context(), replayedOperation{Payload: operation.EncodedArgs}, &river.InsertOpts{Queue: openrails.QueueBilling})
		require.NoError(t, err)
		require.Eventually(t, func() bool {
			job, err := queue.JobGet(t.Context(), replay.Job.ID)
			return err == nil && job.State == rivertype.JobStateCompleted
		}, 20*time.Second, 20*time.Millisecond, "retained operation did not complete on replay")
		renewalSettled(t, a, p, sub, due)
		require.Len(t, p.core.Submitted("/v1/payment_intents"), 2, "settled operation replay must not submit a third debit")
	})
	for _, scenario := range []struct {
		id       string
		accepted bool
	}{{"OR-J29-S01", false}, {"OR-J29-S04", true}} {
		t.Run(scenario.id, func(t *testing.T) {
			a, p, sub := purchasedMembership(t)
			due := *sub.CurrentPeriodEndsAt
			a.clock.Advance(due.Sub(a.clock.Now()))
			var arrived <-chan struct{}
			var release func()
			if scenario.accepted {
				gate := &wireGate{arrived: make(chan struct{}), release: make(chan struct{})}
				p.mu.Lock()
				p.responseGate = gate
				p.mu.Unlock()
				arrived, release = gate.arrived, gate.unblock
			} else {
				// Hold the mandatory history read BEFORE the adapter submits a
				// payment POST. The process dies with an accepted operation and
				// no submitted provider mutation, not merely a lost POST response.
				gate := p.core.Hold(func(r *http.Request) bool { return r.Method == http.MethodGet && r.URL.Path == "/v1/payment_intents" }, stripemock.HoldRequest)
				arrived, release = gate.Arrived(), gate.Release
			}
			defer release()
			queue := hostFleet(t, a.world)
			prior, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
			require.NoError(t, err)
			priorIntents := map[string]bool{}
			for _, job := range prior.Jobs {
				var args map[string]string
				require.NoError(t, json.Unmarshal(job.EncodedArgs, &args))
				priorIntents[args["intent_id"]] = true
			}
			child := startWorkerProcess(t, a.world, p)
			_, err = queue.Insert(t.Context(), registeredJob{KindName: "openrails.dunning"}, &river.InsertOpts{Queue: openrails.QueueBilling})
			require.NoError(t, err)
			select {
			case <-arrived:
			case <-time.After(15 * time.Second):
				t.Fatal("worker never reached external protocol barrier")
			}
			jobs, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
			require.NoError(t, err)
			var running []*rivertype.JobRow
			intents := map[string]bool{}
			for _, job := range jobs.Jobs {
				var args map[string]string
				require.NoError(t, json.Unmarshal(job.EncodedArgs, &args))
				if job.State == rivertype.JobStateRunning && !priorIntents[args["intent_id"]] {
					running = append(running, job)
					intents[args["intent_id"]] = true
				}
			}
			// Several host wakeups may target one financial operation. The
			// operation's persisted lease, not queue-job cardinality, owns money.
			require.NotEmpty(t, running, "accepted renewal must have a durable executing host job")
			require.Len(t, intents, 1, "all current wakeups must refer to one accepted financial operation")
			operation := running[0]
			if scenario.accepted {
				require.Len(t, p.effects(t), 2)
			} else {
				require.Len(t, p.effects(t), 1)
				require.Len(t, p.core.Submitted("/v1/payment_intents"), 1, "only the original enrollment was submitted before process death")
			}
			child.kill(t)
			release()
			a.clock.Advance(20 * time.Minute)
			restarted := startWorkerProcess(t, a.world, p)
			// Two real queue workers compete for the same retained operation after
			// its owner died and its financial lease expired. No store/runner is
			// substituted and no database state is repaired by the fixture.
			var recoveredJobs []int64
			for range 2 {
				job, err := queue.Insert(t.Context(), replayedOperation{Payload: operation.EncodedArgs}, &river.InsertOpts{Queue: openrails.QueueBilling})
				require.NoError(t, err)
				recoveredJobs = append(recoveredJobs, job.Job.ID)
			}
			if !scenario.accepted {
				// Each competing wakeup must first finish its observation or
				// durably snooze. Empty authoritative history schedules a safe
				// resend; its backoff remains enforced by the declared clock.
				require.Eventually(t, func() bool {
					for _, id := range recoveredJobs {
						job, err := queue.JobGet(t.Context(), id)
						if err != nil || (job.State != rivertype.JobStateScheduled && job.State != rivertype.JobStateCompleted) {
							return false
						}
					}
					return true
				}, 15*time.Second, 20*time.Millisecond)
				require.Len(t, p.effects(t), 1, "no debit before safe resend is eligible")
				a.clock.Advance(time.Hour)
				restarted.advance(t, a.clock.Now())
				pending, err := queue.JobList(t.Context(), river.NewJobListParams().Kinds("openrails.provider_operation"))
				require.NoError(t, err)
				for _, job := range pending.Jobs {
					var args map[string]string
					require.NoError(t, json.Unmarshal(job.EncodedArgs, &args))
					if intents[args["intent_id"]] && (job.State == rivertype.JobStateScheduled || job.State == rivertype.JobStateRetryable) {
						_, err = queue.JobRetry(t.Context(), job.ID)
						require.NoError(t, err)
					}
				}
			}
			renewalSettled(t, a, p, sub, due)
			for _, id := range recoveredJobs {
				job, err := queue.JobGet(t.Context(), id)
				require.NoError(t, err)
				if job.State == rivertype.JobStateScheduled {
					_, err = queue.JobRetry(t.Context(), id)
					require.NoError(t, err)
				}
				require.Eventually(t, func() bool {
					job, err := queue.JobGet(t.Context(), id)
					return err == nil && job.State == rivertype.JobStateCompleted
				}, 20*time.Second, 20*time.Millisecond, "both competing recovery jobs must run and converge")
			}
			calls := p.core.Submitted("/v1/payment_intents")
			require.GreaterOrEqual(t, len(calls), 2)
			acceptedKey := calls[1].IdempotencyKey
			require.NotEmpty(t, acceptedKey)
			for _, call := range calls[1:] {
				require.Equal(t, acceptedKey, call.IdempotencyKey)
			}
		})
	}
}
