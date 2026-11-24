// Package providerposture records whether a PSP credential set loaded under
// a non-live PSP is proven to simulate money. A credential set is verified
// when it is loaded (runtime startup, credential create/rotate, or the first
// time a process loads a credential it has not seen); every provider mutation
// consults the cached verdict, proving it again once it is VerdictTTL old.
// Anything but the verdict the PSP expects disarms it: mutations are refused
// with ErrDisarmed, reads still work.
package providerposture

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/open-rails/openrails/internal/retry"
)

// ErrDisarmed refuses a provider mutation before any bytes are sent.
var ErrDisarmed = errors.New("PSP disarmed: provider posture is not verified")

// ErrPending refuses, with ErrDisarmed, a mutation whose credential no loaded
// PSP has registered: a later attempt may pass.
var ErrPending = errors.New("provider posture not verified yet")

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
	// Refused: the provider refused the credential, or the read that proves
	// it; it never arms until the credential changes.
	Refused
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
	case Refused:
		return "refused"
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
	// sandbox posture, Live under live posture.
	Expect Verdict
	// Lasting marks a verdict that stands until the key changes (a Stripe
	// key's account and mode never do): an armed one is never proven again,
	// nor undone by a check the provider did not answer.
	Lasting bool
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
	// check is the check Register or Verify named for the key: the one
	// RequireCredential checks with.
	check Check
}

// VerdictTTL is how long an arming verdict stands. An account's environment
// can change after it was proven (NMI's test mode is a dashboard toggle), so
// the first mutation after it proves it again; there is no sweep. A Lasting
// key's stands.
const VerdictTTL = time.Hour

// RefusedRecheck is how long a lasting key's Refused verdict stands before a
// write or its registration asks the provider again.
const RefusedRecheck = 5 * time.Minute

// DefaultTimeout bounds one posture check, whatever the provider client's own
// timeout: a purchase waiting on it must not hang.
const DefaultTimeout = 5 * time.Second

// Registry is a concurrency-safe verdict cache. One check runs per key at a
// time, bounded by Timeout; every caller waits for it only as long as its own
// context allows. An arming verdict is checked again once it is VerdictTTL
// old, and never used past it: a check that does not answer leaves the key
// Unknown. An Unknown verdict (the provider did not answer) is retried after
// Backoff(consecutive unknowns); a disarming verdict stays until an explicit
// Verify (credential reload) replaces it.
type Registry struct {
	Timeout time.Duration
	Backoff func(attempt int) time.Duration

	clock   atomic.Pointer[func() time.Time]
	mu      sync.Mutex
	entries map[Key]*entry
	// declarations are each PSP declaration's registered key, and
	// credentials count the declarations of each key by its credential.
	declarations map[string]Key
	credentials  map[credentialKey]map[Key]int
}

// NewRegistry returns an empty registry retrying unknown verdicts after backoff.
func NewRegistry(backoff func(attempt int) time.Duration) *Registry {
	return &Registry{Timeout: DefaultTimeout, Backoff: backoff, entries: map[Key]*entry{}}
}

var process = NewRegistry(func(attempt int) time.Duration { return retry.Backoff(attempt, time.Second, retry.Max) })

// Process is the registry shared by every provider client in this process.
// Verdicts are facts about external credentials, not about one runtime.
func Process() *Registry { return process }

// SetClock replaces the registry's time source (nil is the wall clock) and
// returns a func that restores the previous one.
func (r *Registry) SetClock(now func() time.Time) (restore func()) {
	var next *func() time.Time
	if now != nil {
		next = &now
	}
	prev := r.clock.Swap(next)
	return func() { r.clock.Store(prev) }
}

func (r *Registry) now() time.Time {
	if now := r.clock.Load(); now != nil {
		return (*now)()
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
// verdict, replacing any prior one; check becomes k's.
func (r *Registry) Verify(ctx context.Context, k Key, check Check) Status {
	e := r.entry(k)
	e.mu.Lock()
	e.check = check
	e.mu.Unlock()
	return r.resolve(ctx, k, check, true)
}

// Register makes k, checked by check, the registration of declaration (a
// merchant's PSP document), replacing the one it had, and starts checking k
// when its verdict does not stand: a credential is checked as soon as the PSP
// that declares it loads, archived or not, and an unarmed one again once its
// re-check is due. RequireCredential finds it by its credential.
func (r *Registry) Register(declaration string, k Key, check Check) {
	e := r.entry(k)
	r.mu.Lock()
	if prior, ok := r.declarations[declaration]; ok && prior != k {
		r.unindex(prior)
	}
	if prior, ok := r.declarations[declaration]; !ok || prior != k {
		if r.declarations == nil {
			r.declarations = map[string]Key{}
		}
		r.declarations[declaration] = k
		r.index(k)
	}
	r.mu.Unlock()
	e.mu.Lock()
	e.check = check
	var flight chan struct{}
	if !r.settled(e) && e.flight == nil {
		flight = make(chan struct{})
		e.flight = flight
	}
	e.mu.Unlock()
	if flight != nil {
		go r.record(context.Background(), e, k, check, flight)
	}
}

// Unregister drops declaration's registration: its document is gone or no
// longer holds a credential.
func (r *Registry) Unregister(declaration string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if prior, ok := r.declarations[declaration]; ok {
		r.unindex(prior)
		delete(r.declarations, declaration)
	}
}

// index counts one more declaration of k under its credential; r.mu is held.
func (r *Registry) index(k Key) {
	if r.credentials == nil {
		r.credentials = map[credentialKey]map[Key]int{}
	}
	ck := credentialKey{k.Rail, k.Endpoint, k.Credential}
	if r.credentials[ck] == nil {
		r.credentials[ck] = map[Key]int{}
	}
	r.credentials[ck][k]++
}

// unindex counts one declaration of k less; r.mu is held.
func (r *Registry) unindex(k Key) {
	ck := credentialKey{k.Rail, k.Endpoint, k.Credential}
	if r.credentials[ck][k]--; r.credentials[ck][k] <= 0 {
		delete(r.credentials[ck], k)
	}
	if len(r.credentials[ck]) == 0 {
		delete(r.credentials, ck)
	}
}

// credentialKey is one credential at one endpoint of a rail.
type credentialKey struct {
	rail, endpoint string
	credential     [sha256.Size]byte
}

// registered are the keys declarations hold for the credential, of merchant
// alone unless it is uuid.Nil.
func (r *Registry) registered(rail, endpoint string, credential [sha256.Size]byte, merchant uuid.UUID) []Key {
	r.mu.Lock()
	defer r.mu.Unlock()
	var keys []Key
	for k := range r.credentials[credentialKey{rail, endpoint, credential}] {
		if merchant == uuid.Nil || k.MerchantID == merchant {
			keys = append(keys, k)
		}
	}
	slices.SortFunc(keys, func(a, b Key) int { return strings.Compare(a.String(), b.String()) })
	return keys
}

// RequireCredential returns nil when one of merchant's declarations of the
// credential is armed (any merchant's, for uuid.Nil): a request carries the
// credential, not the PSP it acts for, and a declaration of another account,
// a typo or a wrong duplicate, disarms only itself. It never checks with a
// check of its own. A credential the merchant declares nowhere is refused at
// once with ErrPending; a first check still running is waited for, bounded by
// ctx; a verdict that no longer stands is checked again with the registered
// check.
func (r *Registry) RequireCredential(ctx context.Context, rail, endpoint string, credential [sha256.Size]byte, merchant uuid.UUID) error {
	keys := r.registered(rail, endpoint, credential, merchant)
	if len(keys) == 0 {
		return fmt.Errorf("%w: %s credential for %s: %w", ErrDisarmed, rail, endpoint, ErrPending)
	}
	var first error
	for _, k := range keys {
		e := r.entry(k)
		e.mu.Lock()
		check := e.check
		e.mu.Unlock()
		err := r.resolve(ctx, k, check, false).Error()
		if err == nil {
			return nil
		}
		if first == nil {
			first = err
		}
	}
	return first
}

// RefuseCredential records the provider's refusal of the credential (a key
// it no longer accepts) on every declaration of merchant's (any merchant's,
// for uuid.Nil): their writes stop until a re-check (RefusedRecheck) or a new
// credential arms them.
func (r *Registry) RefuseCredential(rail, endpoint string, credential [sha256.Size]byte, merchant uuid.UUID, why error) {
	for _, k := range r.registered(rail, endpoint, credential, merchant) {
		e := r.entry(k)
		e.mu.Lock()
		e.status = Status{Key: k, Verdict: Refused, Err: why, CheckedAt: r.now()}
		e.done, e.failures, e.retryAt = true, 0, time.Time{}
		e.mu.Unlock()
	}
}

// CredentialRefusal is what RequireCredential would refuse from recorded
// verdicts alone, never asking the provider: nil while a declaration of
// merchant's may pass or is still to be checked.
func (r *Registry) CredentialRefusal(rail, endpoint string, credential [sha256.Size]byte, merchant uuid.UUID) error {
	keys := r.registered(rail, endpoint, credential, merchant)
	if len(keys) == 0 {
		return fmt.Errorf("%w: %s credential for %s: %w", ErrDisarmed, rail, endpoint, ErrPending)
	}
	var first error
	for _, k := range keys {
		e := r.entry(k)
		e.mu.Lock()
		settled, status := r.settled(e), e.status
		e.mu.Unlock()
		if !settled || status.Armed() {
			return nil
		}
		if first == nil {
			first = status.Error()
		}
	}
	return first
}

// Require returns nil only for an armed key. An unseen key is verified, an
// arming verdict again once it is VerdictTTL old, and an Unknown verdict once
// its backoff has elapsed.
func (r *Registry) Require(ctx context.Context, k Key, check Check) error {
	return r.resolve(ctx, k, check, false).Error()
}

// Refresh re-verifies k only when Require would.
func (r *Registry) Refresh(ctx context.Context, k Key, check Check) Status {
	return r.resolve(ctx, k, check, false)
}

// settled reports whether e's verdict stands without asking the provider.
func (r *Registry) settled(e *entry) bool {
	if !e.done {
		return false
	}
	switch now := r.now(); {
	case e.status.Verdict == Unknown:
		return now.Before(e.retryAt)
	case e.status.Armed():
		return e.status.Key.Lasting || now.Before(e.status.CheckedAt.Add(VerdictTTL))
	case e.status.Verdict == Refused && e.status.Key.Lasting:
		// A permission may be granted, or a key restored, on the provider's
		// side: a lasting refusal is asked again, at most this often.
		return now.Before(e.status.CheckedAt.Add(RefusedRecheck))
	default:
		return true
	}
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
	if verdict == Unknown && k.Lasting && e.done && e.status.Armed() {
		e.flight = nil
		close(flight)
		return
	}
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
	psps   map[uuid.UUID]trackedPSP
	// refused are PSPs disarmed without a provider check (RefusePSP).
	refused map[uuid.UUID]refusedPSP
}

type refusedPSP struct {
	merchant uuid.UUID
	status   Status
}

// RefusePSP records the merchant's pspID as not armed without asking its
// provider: its credentials cannot be read, which purchase already refuses
// on its own (credentials_missing). Unarmed reports it until a later AddPSP
// or ForgetPSPs.
func (t *Tracked) RefusePSP(merchantID, pspID uuid.UUID, k Key, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.refused == nil {
		t.refused = map[uuid.UUID]refusedPSP{}
	}
	t.refused[pspID] = refusedPSP{merchant: merchantID, status: Status{Key: k, Verdict: Unknown, Err: err}}
}

type trackedPSP struct {
	merchant uuid.UUID
	key      Key
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

// AddPSP records k as the verified credential of the merchant's pspID,
// replacing any prior.
func (t *Tracked) AddPSP(merchantID, pspID uuid.UUID, k Key, check Check) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.checks == nil {
		t.checks = map[Key]Check{}
	}
	if t.psps == nil {
		t.psps = map[uuid.UUID]trackedPSP{}
	}
	if prior, ok := t.psps[pspID]; ok && prior.key != k {
		delete(t.checks, prior.key)
	}
	t.checks[k] = check
	t.psps[pspID] = trackedPSP{merchant: merchantID, key: k}
	delete(t.refused, pspID)
}

// ForgetPSPs drops the merchant's recorded PSPs keep refuses: no longer live,
// or about to be verified again with changed credentials.
func (t *Tracked) ForgetPSPs(merchantID uuid.UUID, keep func(pspID uuid.UUID) bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, p := range t.psps {
		if p.merchant == merchantID && !keep(id) {
			delete(t.psps, id)
			delete(t.checks, p.key)
		}
	}
	for id, p := range t.refused {
		if p.merchant == merchantID && !keep(id) {
			delete(t.refused, id)
		}
	}
}

// PSPDisarmed reports whether pspID's recorded verdict definitively refuses
// mutations. Unverified PSPs and transient Unknown verdicts are not disarmed.
func (t *Tracked) PSPDisarmed(r *Registry, pspID uuid.UUID) bool {
	t.mu.Lock()
	p, ok := t.psps[pspID]
	t.mu.Unlock()
	if !ok {
		return false
	}
	s, done := r.Lookup(p.key)
	return done && s.Verdict != Unknown && !s.Armed()
}

// PSPStatus returns pspID's recorded status, if it has one.
func (t *Tracked) PSPStatus(r *Registry, pspID uuid.UUID) (Status, bool) {
	t.mu.Lock()
	p, ok := t.psps[pspID]
	t.mu.Unlock()
	if !ok {
		return Status{}, false
	}
	return r.Lookup(p.key)
}

// Unarmed returns every tracked key that is not armed, from cached verdicts
// only: it never contacts a provider.
func (t *Tracked) Unarmed(r *Registry) []Status {
	t.mu.Lock()
	keys := make([]Key, 0, len(t.checks))
	for k := range t.checks {
		keys = append(keys, k)
	}
	var out []Status
	for _, p := range t.refused {
		out = append(out, p.status)
	}
	t.mu.Unlock()
	for _, k := range keys {
		if s, done := r.Lookup(k); done && !s.Armed() {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key.String() < out[j].Key.String() })
	return out
}
