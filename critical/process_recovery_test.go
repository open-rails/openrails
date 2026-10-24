//go:build integration

package critical_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
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
	"github.com/open-rails/openrails"
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
	engine, err := openrails.New(t.Context(), config.Config, openrails.Deps{Postgres: pool, Clock: clock, StripeTransport: loopbackWire{target: stripe, wire: wire}, FXTransport: loopbackWire{target: fx, wire: wire}})
	require.NoError(t, err)
	defer engine.Close(context.Background()) //nolint:errcheck
	w := &world{pool: pool, schema: config.Config.Database.Schema, engine: engine, clock: clock}
	startFleet(t, w)
	require.NoError(t, os.WriteFile(config.Ready, []byte("running"), 0600))
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
}

func startWorkerProcess(t *testing.T, w *world, p *providerLedger) *ownedWorkerProcess {
	t.Helper()
	dir := t.TempDir()
	config := workerProcessConfig{Config: w.cfg, At: w.clock.Now(), StripeURL: p.server.URL, FXURL: w.deps.FXTransport.(loopbackWire).target.String(), Ready: filepath.Join(dir, "ready"), ClockFile: filepath.Join(dir, "clock")}
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
			if _, err := os.Stat(config.Ready); err == nil {
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
