package nmimock

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DeclineLast4 is the last four of an unregistered token whose sales decline
// with 202 (insufficient funds); its verification still passes.
const DeclineLast4 = "0002"

// Options configure a Mock. The zero value is a wall-clock gateway with no
// duplicate window and no indexing lag.
type Options struct {
	// Clock is the gateway's only time source. Nil means time.Now.
	Clock func() time.Time
	// DuplicateWindow refuses a sale or verification of the same card and
	// amount as one processed within it, whatever its order id. Zero is off.
	DuplicateWindow time.Duration
	// IndexLag hides an approved sale from the Query API until this long
	// after it processed.
	IndexLag time.Duration
	// PlanFallback answers reads of plans the account has but the mock was
	// never given (plans a legacy system created long ago).
	PlanFallback func(id string) (Plan, bool)
}

// Mock is a stateful NMI gateway. It is an http.Handler (for a listener) and
// an http.RoundTripper (for in-process clients). All methods are safe for
// concurrent use.
type Mock struct {
	opts   Options
	server *httptest.Server

	mu          sync.Mutex
	seq         int
	tokens      map[string]Card
	vaults      map[string]*Vault
	plans       map[string]*Plan
	schedules   map[string]*Schedule
	sales       []*Sale
	refunds     []*refund
	probes      map[string]bool
	attempts    []url.Values
	validations []*Validation
	calls       []Call
	reads       map[string]int
	odd         []string
	recent      []recentCharge

	refusedSaves int
	duplicate    int
	lose, lost   int
	drop         int
	failUpdates  int
	hide         int
	queryDown    bool
	failures     []*failure
	intercepts   []*intercept
}

// New starts a Mock on a loopback listener; see URL and Close.
func New(opts Options) *Mock {
	m := NewUnstarted(opts)
	m.server = httptest.NewServer(m)
	return m
}

// NewUnstarted is a Mock with no listener, for use as a RoundTripper or a
// Handler the caller serves.
func NewUnstarted(opts Options) *Mock {
	if opts.Clock == nil {
		opts.Clock = time.Now
	}
	return &Mock{opts: opts, tokens: map[string]Card{}, vaults: map[string]*Vault{}, plans: map[string]*Plan{},
		schedules: map[string]*Schedule{}, reads: map[string]int{}, probes: map[string]bool{}}
}

// URL is the loopback gateway root; OpenRails' provider_sandbox.nmi_gateway_url.
func (m *Mock) URL() string {
	if m.server == nil {
		panic("nmimock: URL of an unstarted mock")
	}
	return m.server.URL
}

// Close stops the listener, if any.
func (m *Mock) Close() {
	if m.server != nil {
		m.server.Close()
	}
}

// Transport routes every request to the mock, whatever its host.
func (m *Mock) Transport() http.RoundTripper { return m }

func (m *Mock) now() time.Time { return m.opts.Clock().UTC() }

func (m *Mock) next(prefix string) string {
	m.seq++
	return fmt.Sprintf("%s%08d", prefix, m.seq)
}

// errLost is a request whose connection failed; a listener aborts it.
var errLost = errors.New("nmimock: connection reset")

// ServeHTTP serves the gateway on a listener. A lost request or answer
// aborts the connection.
func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	res, err := m.RoundTrip(r)
	if err != nil {
		panic(http.ErrAbortHandler)
	}
	defer res.Body.Close()
	for k, v := range res.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, res.Body)
}

// RoundTrip serves r in process.
func (m *Mock) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte
	if r.Body != nil {
		body, _ = io.ReadAll(r.Body)
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	serve := func() *http.Response {
		res := m.serve(r, body).Result()
		res.Request = r
		return res
	}
	kind, _ := route(r.URL.Path, body)
	directPost := kind == routeDirectPost && !strings.Contains(string(body), "type=validate")
	m.mu.Lock()
	lost, dropped := m.lose > 0 && directPost, m.drop > 0 && directPost
	switch {
	case lost:
		m.lose--
		m.lost++
	case dropped:
		m.drop--
	}
	intercepts := append([]*intercept(nil), m.intercepts...)
	failures := append([]*failure(nil), m.failures...)
	m.mu.Unlock()
	if lost {
		return nil, fmt.Errorf("%w before the gateway received the request", errLost)
	}
	if dropped {
		serve()
		return nil, fmt.Errorf("%w before the gateway's answer arrived", errLost)
	}
	for _, f := range failures {
		if f.match(r) && f.take() {
			rec := httptest.NewRecorder()
			rec.WriteHeader(f.status)
			res := rec.Result()
			res.Request = r
			return res, nil
		}
	}
	var hook *intercept
	for _, ic := range intercepts {
		if ic.match(r) {
			hook = ic
		}
	}
	if hook != nil {
		return hook.fn(r, serve)
	}
	return serve(), nil
}

const (
	routeV5 = iota
	routeDirectPost
	routeQuery
	routeUnknown
)

// route places a request: at NMI's real paths (/api/v5/..., /api/transact.php,
// /api/query.php) or, as OpenRails' single sandbox gateway URL sends them, v5
// resources at the root and Direct Post or Query forms posted to it.
func route(path string, body []byte) (int, string) {
	if i := strings.Index(path, "/v5/"); i >= 0 {
		return routeV5, path[i+3:]
	}
	switch strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)[0] {
	case "customers", "payments", "plans", "subscriptions":
		return routeV5, path
	}
	switch {
	case strings.HasSuffix(path, "/transact.php"):
		return routeDirectPost, ""
	case strings.HasSuffix(path, "/query.php"):
		return routeQuery, ""
	case path == "" || path == "/":
		if form, _ := url.ParseQuery(string(body)); form.Has("report_type") {
			return routeQuery, ""
		}
		return routeDirectPost, ""
	}
	return routeUnknown, ""
}

func (m *Mock) serve(r *http.Request, body []byte) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	m.mu.Lock()
	defer m.mu.Unlock()
	kind, v5Path := route(r.URL.Path, body)
	if kind == routeV5 {
		m.serveV5(rec, r.Method, v5Path, r.URL.Query(), body)
		return rec
	}
	form, _ := url.ParseQuery(string(body))
	for k, v := range r.URL.Query() {
		form[k] = v
	}
	switch kind {
	case routeDirectPost:
		m.calls = append(m.calls, Call{Method: r.Method, Path: "transact.php", Form: form})
		_, _ = rec.WriteString(m.directPost(form))
	case routeQuery:
		m.reads["query:"+form.Get("report_type")]++
		m.query(rec, form)
	default:
		m.odd = append(m.odd, r.Method+" "+r.URL.Path)
		rec.WriteHeader(http.StatusNotFound)
	}
	return rec
}
