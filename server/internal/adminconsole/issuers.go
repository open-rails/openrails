package adminconsole

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/open-rails/openrails/internal/shared/httpx"
)

// ClientID is the console's public OAuth client at every trusted issuer, as
// at the server's own AuthKit. The issuer registers it with the redirect URI
// <origin>/admin/callback.
const ClientID = "openrails-console"

// Source is a trusted issuer a console sign-in may lead to.
type Source struct {
	// Key is the merchant id whose group trusts it, or "root".
	Key string
	// Name is the merchant's display name; "" for root.
	Name   string
	Issuer string
	// Operator: the operator declared it, so its metadata may be on a private
	// network. An application registered at run time is fetched only on the
	// public internet.
	Operator bool
}

// Directory derives the console's sign-in buttons from the trusted issuers:
// one per issuer whose OpenID Connect metadata, the document the console
// itself reads, names an authorization endpoint. Issuers that only sign
// assertions get none. The list is recomputed at most every listTTL and each
// issuer's metadata read at most every metadataTTL.
type Directory struct {
	sources  func(context.Context) ([]Source, error)
	resource string
	public   *http.Client
	private  *http.Client
	now      func() time.Time

	mu      sync.Mutex
	listAt  time.Time
	list    []Issuer
	signsIn map[string]checked
}

type checked struct {
	at time.Time
	ok bool
}

const (
	listTTL     = 30 * time.Second
	metadataTTL = 10 * time.Minute
	failedTTL   = time.Minute
)

// NewDirectory lists sources' sign-in buttons for tokens to resource.
func NewDirectory(sources func(context.Context) ([]Source, error), resource string) *Directory {
	declared := httpx.Policy{Allow: func(net.IP) bool { return true }}
	return &Directory{
		sources: sources, resource: resource,
		public: httpx.Policy{}.Client(5 * time.Second), private: declared.Client(5 * time.Second),
		now: time.Now, signsIn: map[string]checked{},
	}
}

// Issuers are the console's sign-in buttons, sorted by name.
func (d *Directory) Issuers(ctx context.Context) ([]Issuer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	if d.list != nil && now.Sub(d.listAt) < listTTL {
		return d.list, nil
	}
	sources, err := d.sources(ctx)
	if err != nil {
		return nil, err
	}
	byIssuer := map[string][]Source{}
	var order []string
	for _, s := range sources {
		issuer := strings.TrimRight(strings.TrimSpace(s.Issuer), "/")
		if issuer == "" {
			continue
		}
		if _, seen := byIssuer[issuer]; !seen {
			order = append(order, issuer)
		}
		byIssuer[issuer] = append(byIssuer[issuer], s)
	}
	out := []Issuer{}
	for _, issuer := range order {
		group := byIssuer[issuer]
		if !d.signsPeopleIn(ctx, issuer, slices.ContainsFunc(group, func(s Source) bool { return s.Operator }), now) {
			continue
		}
		out = append(out, Issuer{Key: issuer, URL: issuer, ClientID: ClientID, Name: buttonName(issuer, group), Resource: d.resource, Scope: DefaultScope})
	}
	slices.SortFunc(out, func(a, b Issuer) int { return strings.Compare(a.Name, b.Name) })
	d.list, d.listAt = out, now
	return out, nil
}

// buttonName is the merchant's name for an issuer one merchant trusts, else
// the issuer's host.
func buttonName(issuer string, group []Source) string {
	if len(group) == 1 && group[0].Name != "" {
		return group[0].Name
	}
	if u, err := url.Parse(issuer); err == nil && u.Host != "" {
		return u.Host
	}
	return issuer
}

// signsPeopleIn reports whether issuer's metadata names an authorization
// endpoint, as last read.
func (d *Directory) signsPeopleIn(ctx context.Context, issuer string, operator bool, now time.Time) bool {
	key := issuer
	if operator {
		key = "operator " + issuer
	}
	if c, ok := d.signsIn[key]; ok && (c.ok && now.Sub(c.at) < metadataTTL || !c.ok && now.Sub(c.at) < failedTTL) {
		return c.ok
	}
	client := d.public
	if operator {
		client = d.private
	}
	ok := authorizationEndpoint(ctx, client, issuer)
	d.signsIn[key] = checked{at: now, ok: ok}
	return ok
}

func authorizationEndpoint(ctx context.Context, client *http.Client, issuer string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, issuer+"/.well-known/openid-configuration", nil)
	if err != nil {
		return false
	}
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	var m struct {
		Issuer                string `json:"issuer"`
		AuthorizationEndpoint string `json:"authorization_endpoint"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&m) != nil {
		return false
	}
	return strings.TrimRight(m.Issuer, "/") == issuer && m.AuthorizationEndpoint != ""
}
