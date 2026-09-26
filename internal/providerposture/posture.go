// Package providerposture records whether a PSP credential set loaded under
// test_mode=sandbox is proven to simulate money. A credential set is verified
// once when it is loaded (runtime startup, credential create/rotate, or the
// first time a process loads a credential it has not seen); every provider
// mutation consults the cached verdict. Anything but a proven simulated verdict
// disarms the PSP: mutations are refused with ErrDisarmed, reads still work.
package providerposture

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/retry"
)

// ErrDisarmed refuses a provider mutation before any bytes are sent.
var ErrDisarmed = errors.New("PSP disarmed: provider posture is not verified")

// Verdict is what a provider's authoritative signal says about a credential.
type Verdict uint8

const (
	Unknown Verdict = iota
	Simulated
	Live
	// Mismatched: the credential belongs to a different account than declared.
	Mismatched
	// Unsupported: the provider exposes no authoritative test signal, so the
	// credential can never be armed under sandbox posture.
	Unsupported
)

func (v Verdict) String() string {
	switch v {
	case Simulated:
		return "simulated"
	case Live:
		return "live"
	case Mismatched:
		return "mismatched"
	case Unsupported:
		return "unsupported"
	default:
		return "unknown"
	}
}

// Key binds a verdict to the exact merchant, PSP account, endpoint and
// credential. Any change produces a different key and a fresh verification.
type Key struct {
	Rail       string
	MerchantID uuid.UUID
	PSPID      uuid.UUID
	AccountID  string
	Endpoint   string
	Credential [sha256.Size]byte
	// Expect is the verdict that arms this key: Simulated (zero value) under
	// sandbox posture, Live under live posture (SEC-33).
	Expect Verdict
}

func (k Key) expected() Verdict {
	if k.Expect == Unknown {
		return Simulated
	}
	return k.Expect
}

func (k Key) String() string {
	return fmt.Sprintf("%s account %q merchant %s psp %s endpoint %s", k.Rail, k.AccountID, k.MerchantID, k.PSPID, k.Endpoint)
}

// Fingerprint hashes credential material without retaining it.
func Fingerprint(parts ...string) [sha256.Size]byte {
	h := sha256.New()
	var n [8]byte
	for _, part := range parts {
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	var out [sha256.Size]byte
	copy(out[:], h.Sum(nil))
	return out
}

// Check runs the provider's authoritative read. A non-nil error with Unknown
// means the signal was unavailable.
type Check func(context.Context) (Verdict, error)

// Status is the recorded verdict for one key.
type Status struct {
	Key       Key
	Verdict   Verdict
	Err       error
	CheckedAt time.Time
}

// Armed reports whether mutations are permitted.
func (s Status) Armed() bool { return s.Verdict == s.Key.expected() }

// Error explains why the key is disarmed, wrapping ErrDisarmed.
func (s Status) Error() error {
	if s.Armed() {
		return nil
	}
	if s.Err != nil {
		return fmt.Errorf("%w: %s: %s: %w", ErrDisarmed, s.Key, s.Verdict, s.Err)
	}
	return fmt.Errorf("%w: %s: %s", ErrDisarmed, s.Key, s.Verdict)
}

type entry struct {
	mu       sync.Mutex
	status   Status
	done     bool
	failures int
	retryAt  time.Time
	// flight is closed when the running check has recorded its verdict.
	flight chan struct{}
}

// DefaultTimeout bounds one posture check, whatever the provider client's own
// timeout: a checkout waiting on it must not hang.
const DefaultTimeout = 5 * time.Second

// Registry is a concurrency-safe verdict cache. One check runs per key at a
// time, bounded by Timeout; every caller waits for it only as long as its own
// context allows. An Unknown verdict (the provider did not answer) is retried
// after Backoff(consecutive unknowns); live and unsupported verdicts stay
// disarmed until an explicit Verify (credential reload) replaces them.
type Registry struct {
	Timeout time.Duration
	Backoff func(attempt int) time.Duration
	Now     func() time.Time

	mu      sync.Mutex
	entries map[Key]*entry
}

// NewRegistry returns an empty registry retrying unknown verdicts after backoff.
func NewRegistry(backoff func(attempt int) time.Duration) *Registry {
	return &Registry{Timeout: DefaultTimeout, Backoff: backoff, entries: map[Key]*entry{}}
}

var process = NewRegistry(func(attempt int) time.Duration { return retry.Backoff(attempt, time.Second, retry.Max) })

// Process is the registry shared by every provider client in this process.
// Verdicts are facts about external credentials, not about one runtime.
func Process() *Registry { return process }

func (r *Registry) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Registry) entry(k Key) *entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.entries == nil {
		r.entries = map[Key]*entry{}
	}
	e := r.entries[k]
	if e == nil {
		e = &entry{}
		r.entries[k] = e
	}
	return e
}

// Verify checks k now (joining a check already running) and records the
// verdict, replacing any prior one.
func (r *Registry) Verify(ctx context.Context, k Key, check Check) Status {
	return r.resolve(ctx, k, check, true)
}

// Require returns nil only for an armed key. An unseen key is verified once;
// an Unknown verdict is re-verified once its backoff has elapsed.
func (r *Registry) Require(ctx context.Context, k Key, check Check) error {
	return r.resolve(ctx, k, check, false).Error()
}

// Refresh re-verifies k only when its Unknown verdict is due for retry.
func (r *Registry) Refresh(ctx context.Context, k Key, check Check) Status {
	return r.resolve(ctx, k, check, false)
}

func (r *Registry) settled(e *entry) bool {
	return e.done && (e.status.Verdict != Unknown || r.now().Before(e.retryAt))
}

func (r *Registry) resolve(ctx context.Context, k Key, check Check, force bool) Status {
	e := r.entry(k)
	e.mu.Lock()
	if !force && r.settled(e) {
		s := e.status
		e.mu.Unlock()
		return s
	}
	flight := e.flight
	if flight == nil {
		flight = make(chan struct{})
		e.flight = flight
		go r.record(context.WithoutCancel(ctx), e, k, check, flight)
	}
	e.mu.Unlock()
	select {
	case <-flight:
		e.mu.Lock()
		defer e.mu.Unlock()
		return e.status
	case <-ctx.Done():
		return Status{Key: k, Verdict: Unknown, Err: ctx.Err(), CheckedAt: r.now()}
	}
}

func (r *Registry) record(ctx context.Context, e *entry, k Key, check Check, flight chan struct{}) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	verdict, err := Unknown, error(nil)
	if check == nil {
		err = errors.New("no posture check")
	} else {
		verdict, err = check(ctx)
	}
	if verdict == k.expected() && err != nil {
		verdict = Unknown
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	now := r.now()
	e.status = Status{Key: k, Verdict: verdict, Err: err, CheckedAt: now}
	e.done = true
	if verdict == Unknown {
		if r.Backoff != nil {
			e.retryAt = now.Add(r.Backoff(e.failures))
		}
		e.failures++
	} else {
		e.failures, e.retryAt = 0, time.Time{}
	}
	e.flight = nil
	close(flight)
}

// Lookup returns the recorded status for k.
func (r *Registry) Lookup(k Key) (Status, bool) {
	r.mu.Lock()
	e := r.entries[k]
	r.mu.Unlock()
	if e == nil {
		return Status{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.status, e.done
}

// Tracked is a set of keys one runtime verified when it loaded credentials,
// with the check that re-verifies each. Readiness reports it.
type Tracked struct {
	mu     sync.Mutex
	checks map[Key]Check
	psps   map[uuid.UUID]Key
}

// Add records k as loaded by this runtime.
func (t *Tracked) Add(k Key, check Check) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.checks == nil {
		t.checks = map[Key]Check{}
	}
	t.checks[k] = check
}

// AddPSP records k as the verified credential of pspID, replacing any prior.
func (t *Tracked) AddPSP(pspID uuid.UUID, k Key, check Check) {
	t.Add(k, check)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.psps == nil {
		t.psps = map[uuid.UUID]Key{}
	}
	t.psps[pspID] = k
}

// PSPDisarmed reports whether pspID's recorded verdict definitively refuses
// mutations. Unverified PSPs and transient Unknown verdicts are not disarmed.
func (t *Tracked) PSPDisarmed(r *Registry, pspID uuid.UUID) bool {
	t.mu.Lock()
	k, ok := t.psps[pspID]
	t.mu.Unlock()
	if !ok {
		return false
	}
	s, done := r.Lookup(k)
	return done && s.Verdict != Unknown && !s.Armed()
}

// PSPStatus returns pspID's recorded status, if it has one.
func (t *Tracked) PSPStatus(r *Registry, pspID uuid.UUID) (Status, bool) {
	t.mu.Lock()
	k, ok := t.psps[pspID]
	t.mu.Unlock()
	if !ok {
		return Status{}, false
	}
	return r.Lookup(k)
}

// Unarmed returns every tracked key that is not armed, from cached verdicts
// only: it never contacts a provider.
func (t *Tracked) Unarmed(r *Registry) []Status {
	t.mu.Lock()
	keys := make([]Key, 0, len(t.checks))
	for k := range t.checks {
		keys = append(keys, k)
	}
	t.mu.Unlock()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	var out []Status
	for _, k := range keys {
		if s, done := r.Lookup(k); done && !s.Armed() {
			out = append(out, s)
		}
	}
	return out
}
