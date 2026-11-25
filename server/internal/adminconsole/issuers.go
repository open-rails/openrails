package adminconsole

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-rails/openrails/internal/logx"
	"github.com/open-rails/openrails/internal/shared/httpx"
)

// ClientID is the console's public OAuth client at every trusted issuer, as
// at the server's own AuthKit. The issuer registers it with the redirect URI
// <origin>/admin/callback.
const ClientID = "openrails-console"

// CallbackPath, beneath Path, is where a trusted issuer returns staff.
const CallbackPath = "/callback"

// Source is one of root's trusted issuers.
type Source struct {
	Issuer string
	// Operator: the operator declared it, so its metadata may be on a private
	// network. An application registered at run time is fetched only on the
	// public internet.
	Operator bool
}

// Directory is the console's sign-in buttons: root's trusted issuers whose
// sign-in a probe says works. Requests read its last result and never wait
// on a probe: it lists root's issuers at most every listTTL and probes one
// again okTTL after it passed, failedTTL after it failed, in the background,
// starting at construction. Report hears each issuer's change: why it is
// hidden, or nil once it shows again or is no longer trusted.
type Directory struct {
	sources     func(context.Context) ([]Source, error)
	resource    string
	redirectURI string
	report      func(issuer string, err error)
	log         *slog.Logger
	public      *http.Client
	private     *http.Client
	now         func() time.Time

	mu         sync.Mutex
	listedAt   time.Time
	refreshing bool
	probed     map[string]probe
}

type probe struct {
	at       time.Time
	operator bool
	err      error
}

const (
	listTTL      = 5 * time.Second
	okTTL        = 10 * time.Minute
	failedTTL    = time.Minute
	probeTimeout = 5 * time.Second
)

// NewDirectory probes root's trusted issuers for tokens to resource, the
// console returning staff to redirectURI, and starts the first pass.
func NewDirectory(logger *slog.Logger, sources func(context.Context) ([]Source, error), resource, redirectURI string, report func(issuer string, err error)) *Directory {
	d := newDirectory(logger, sources, resource, redirectURI, report)
	d.refreshing = true
	go d.refresh()
	return d
}

func newDirectory(logger *slog.Logger, sources func(context.Context) ([]Source, error), resource, redirectURI string, report func(issuer string, err error)) *Directory {
	declared := httpx.Policy{Allow: func(net.IP) bool { return true }}
	return &Directory{
		sources: sources, resource: resource, redirectURI: redirectURI, report: report, log: logx.Or(logger),
		public: httpx.Policy{}.Client(probeTimeout), private: declared.Client(probeTimeout),
		now: time.Now, probed: map[string]probe{},
	}
}

// Issuers are the buttons of the issuers whose last probe passed, sorted by
// name. A pass that is due starts in the background.
func (d *Directory) Issuers() []Issuer {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	due := now.Sub(d.listedAt) >= listTTL
	out := []Issuer{}
	for issuer, p := range d.probed {
		due = due || d.stale(p, now)
		if p.err == nil {
			out = append(out, Issuer{Key: issuer, URL: issuer, ClientID: ClientID, Name: buttonName(issuer), Resource: d.resource, Scope: DefaultScope, RedirectURI: d.redirectURI})
		}
	}
	if due && !d.refreshing {
		d.refreshing = true
		go d.refresh()
	}
	slices.SortFunc(out, func(a, b Issuer) int { return cmp.Or(strings.Compare(a.Name, b.Name), strings.Compare(a.Key, b.Key)) })
	return out
}

func (d *Directory) stale(p probe, now time.Time) bool {
	return now.Sub(p.at) >= okTTL || p.err != nil && now.Sub(p.at) >= failedTTL
}

// refresh lists root's issuers and probes those due, one at a time.
func (d *Directory) refresh() {
	defer func() {
		d.mu.Lock()
		d.refreshing = false
		d.mu.Unlock()
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sources, err := d.sources(ctx)
	d.mu.Lock()
	d.listedAt = d.now()
	d.mu.Unlock()
	if err != nil {
		// The last result stands; the server's own accounts still sign in.
		d.log.WarnContext(ctx, "admin console: listing root's trusted issuers failed", logx.Err(err))
		return
	}
	trusted := map[string]bool{}
	for _, s := range sources {
		issuer := strings.TrimSpace(s.Issuer)
		if issuer == "" || trusted[issuer] {
			continue
		}
		trusted[issuer] = true
		d.mu.Lock()
		p, seen := d.probed[issuer]
		due := !seen || p.operator != s.Operator || d.stale(p, d.now())
		d.mu.Unlock()
		if due {
			d.record(issuer, s.Operator, p.err, seen, d.check(ctx, issuer, s.Operator))
		}
	}
	d.mu.Lock()
	var gone []string
	for issuer, p := range d.probed {
		if !trusted[issuer] {
			delete(d.probed, issuer)
			if p.err != nil {
				gone = append(gone, issuer)
			}
		}
	}
	d.mu.Unlock()
	for _, issuer := range gone {
		d.report(issuer, nil)
	}
}

// record keeps issuer's probe and reports a change of state or reason.
func (d *Directory) record(issuer string, operator bool, before error, seen bool, err error) {
	d.mu.Lock()
	d.probed[issuer] = probe{at: d.now(), operator: operator, err: err}
	d.mu.Unlock()
	if seen && reason(before) == reason(err) || !seen && err == nil {
		return
	}
	d.report(issuer, err)
}

func reason(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// check is whether the console's sign-in at issuer works, as far as it can
// be told without a person: the redirect URI is an absolute URL (the client
// id is fixed, ClientID), and the issuer's OpenID Connect metadata names it
// exactly, its authorization, token and key endpoints, the authorization
// code flow and PKCE S256 where it lists what it supports, and its keys are
// published. Whether it registered the console's client only a sign-in shows.
func (d *Directory) check(ctx context.Context, issuer string, operator bool) error {
	if !absoluteURL(d.redirectURI) {
		return fmt.Errorf("the console's redirect URI %q is not an absolute URL", d.redirectURI)
	}
	client := d.public
	if operator {
		client = d.private
	}
	var m struct {
		Issuer                string   `json:"issuer"`
		AuthorizationEndpoint string   `json:"authorization_endpoint"`
		TokenEndpoint         string   `json:"token_endpoint"`
		JWKSURI               string   `json:"jwks_uri"`
		ResponseTypes         []string `json:"response_types_supported"`
		CodeChallengeMethods  []string `json:"code_challenge_methods_supported"`
	}
	if err := getJSON(ctx, client, strings.TrimSuffix(issuer, "/")+"/.well-known/openid-configuration", &m); err != nil {
		return fmt.Errorf("OpenID Connect discovery: %w", err)
	}
	switch {
	case m.Issuer != issuer:
		return fmt.Errorf("its OpenID Connect metadata names the issuer %q", m.Issuer)
	case !absoluteURL(m.AuthorizationEndpoint):
		return errors.New("its OpenID Connect metadata names no authorization_endpoint")
	case !absoluteURL(m.TokenEndpoint):
		return errors.New("its OpenID Connect metadata names no token_endpoint")
	case !absoluteURL(m.JWKSURI):
		return errors.New("its OpenID Connect metadata names no jwks_uri")
	case m.ResponseTypes != nil && !slices.Contains(m.ResponseTypes, "code"):
		return errors.New("it does not offer the authorization code flow (response_types_supported)")
	case m.CodeChallengeMethods != nil && !slices.Contains(m.CodeChallengeMethods, "S256"):
		return errors.New("it does not offer PKCE S256 (code_challenge_methods_supported)")
	}
	var set struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := getJSON(ctx, client, m.JWKSURI, &set); err != nil {
		return fmt.Errorf("its keys (jwks_uri): %w", err)
	}
	if len(set.Keys) == 0 {
		return errors.New("its key set (jwks_uri) has no keys")
	}
	return nil
}

func getJSON(ctx context.Context, client *http.Client, target string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s answered %d", target, res.StatusCode)
	}
	return httpx.DecodeJSONLimited(res.Body, 0, v)
}

// absoluteURL reports an http(s) URL with a host and no fragment.
func absoluteURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.Fragment == ""
}

// buttonName is the issuer's host.
func buttonName(issuer string) string {
	if u, err := url.Parse(issuer); err == nil && u.Host != "" {
		return u.Host
	}
	return issuer
}
