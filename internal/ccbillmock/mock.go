// Package ccbillmock is a stateful CCBill DataLink for tests and disposable
// sandbox stacks: the ACTIVEMEMBERS roster, REBILL transaction exports and
// the subscription-management (SMS) status and cancel actions. Point
// Config.ProviderSandbox.CCBillDataLinkURL at URL. Only tests and sandbox
// commands may import it.
//
// Times are written as CCBill's DataLink guide writes them: a roster row's
// transaction timestamp as YYYYMMDDHHMMSS, a REBILL row's and every rebill
// and expire date as YYYY-MM-DD. CCBill names no zone for them; they are UTC,
// the zone OpenRails reads them in.
//
// Known differences from real CCBill: credentials are checked only after
// RequireCredentials; members carry no amount (DataLink's roster names none);
// exports honor startTime and endTime (CCBill's MST clock), refuse a range
// over 24 hours as CCBill does, and list only rebills; there is no limit of
// one request an hour.
package ccbillmock

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// Member is one row of the ACTIVEMEMBERS roster. Rebill is the next rebill
// date; a canceled member with access left has Expiry instead.
type Member struct {
	SubscriptionID, Username, Email string
	Signup                          time.Time
	Rebill, Expiry                  *time.Time
}

// Rebill is one REBILL row: a charge CCBill made, its amount a decimal in
// major units with no currency.
type Rebill struct {
	SubscriptionID, TransactionID string
	At                            time.Time
	Amount                        string
}

// Call is one SMS action CCBill received.
type Call struct {
	Path string
	Form url.Values
}

// Export is one transaction export DataLink answered: its types and its
// startTime and endTime, both inclusive.
type Export struct {
	Types      string
	Start, End time.Time
}

// Mock is a running fake DataLink. Its methods are safe for concurrent use.
type Mock struct {
	server  *httptest.Server
	mu      sync.Mutex
	seq     int
	members []Member
	rebills []Rebill
	cancels map[string]int
	calls   []Call
	exports []Export
	odd     []string
	auth    *credentials
}

// credentials are the only DataLink login answered; refused says how others
// are turned away.
type credentials struct {
	username, password string
	status             bool
}

// mst is the clock DataLink's startTime and endTime are read in.
var mst = time.FixedZone("MST", -7*60*60)

// New starts DataLink on a loopback listener; Close stops it.
func New() *Mock {
	m := &Mock{cancels: map[string]int{}}
	m.server = httptest.NewServer(m)
	return m
}

// URL is the loopback root: Config.ProviderSandbox.CCBillDataLinkURL.
func (m *Mock) URL() string { return m.server.URL }

// Close stops the listener.
func (m *Mock) Close() { m.server.Close() }

func (m *Mock) nextID() string {
	m.seq++
	return fmt.Sprintf("10%017d", m.seq)
}

// AddMember lists a member and returns its subscription id (SubscriptionID
// when set).
func (m *Mock) AddMember(member Member) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if member.SubscriptionID == "" {
		member.SubscriptionID = m.nextID()
	}
	m.members = append(m.members, member)
	return member.SubscriptionID
}

// AddRebill records a rebill and returns its transaction id (TransactionID
// when set).
func (m *Mock) AddRebill(r Rebill) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.TransactionID == "" {
		r.TransactionID = m.nextID()
	}
	m.rebills = append(m.rebills, r)
	return r.TransactionID
}

// RequireCredentials makes DataLink answer only username and password. Any
// other login is refused as CCBill refuses one: with a 401 when status is
// set, else with an "Authentication failed" answer.
func (m *Mock) RequireCredentials(username, password string, status bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auth = &credentials{username: username, password: password, status: status}
}

// Canceled counts the cancelSubscription calls CCBill received for id.
func (m *Mock) Canceled(id string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cancels[id]
}

// Calls are every SMS action received, in order: status reads and cancels.
func (m *Mock) Calls() []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Call(nil), m.calls...)
}

// Mutations are the SMS actions that change a subscription (cancels).
func (m *Mock) Mutations() []Call {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Call
	for _, c := range m.calls {
		if c.Form.Get("action") != "viewSubscriptionStatus" {
			out = append(out, c)
		}
	}
	return out
}

// Exports are the transaction exports answered, in order.
func (m *Mock) Exports() []Export {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Export(nil), m.exports...)
}

// Unexpected lists requests the mock does not model; tests assert it empty.
func (m *Mock) Unexpected() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]string(nil), m.odd...)
	sort.Strings(out)
	return out
}

func (m *Mock) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	m.mu.Lock()
	defer m.mu.Unlock()
	if a := m.auth; a != nil && r.URL.Path == "/data/main.cgi" && (r.Form.Get("username") != a.username || r.Form.Get("password") != a.password) {
		if a.status {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, "Error: Authentication failed.")
		return
	}
	switch {
	case r.URL.Path == "/utils/subscriptionManagement.cgi":
		m.calls = append(m.calls, Call{Path: r.URL.Path, Form: r.Form})
		id := r.Form.Get("subscriptionId")
		switch r.Form.Get("action") {
		case "viewSubscriptionStatus":
			status := "2" // rebilling
			if m.cancels[id] > 0 {
				status = "1" // canceled, access through the paid period
			}
			_, _ = io.WriteString(w, "<results><subscriptionStatus>"+status+"</subscriptionStatus></results>")
		case "cancelSubscription":
			m.cancels[id]++
			_, _ = io.WriteString(w, "<results>1</results>")
		default:
			m.odd = append(m.odd, "sms "+r.Form.Get("action"))
			w.WriteHeader(http.StatusBadRequest)
		}
	case r.URL.Path == "/data/main.cgi" && r.Form.Get("transactionTypes") == "ACTIVEMEMBERS":
		var rows []string
		for _, mem := range m.members {
			rows = append(rows, fmt.Sprintf(`"ACTIVEMEMBERS","999999","0000","%s","%s","%s","%s","1","%s","%s"`,
				mem.SubscriptionID, mem.Signup.UTC().Format("20060102150405"), mem.Username, mem.Email, date(mem.Rebill), date(mem.Expiry)))
		}
		_, _ = io.WriteString(w, strings.Join(rows, "\n"))
	case r.URL.Path == "/data/main.cgi" && r.Form.Get("transactionTypes") != "":
		start, err1 := time.ParseInLocation("20060102150405", r.Form.Get("startTime"), mst)
		end, err2 := time.ParseInLocation("20060102150405", r.Form.Get("endTime"), mst)
		if err1 != nil || err2 != nil {
			m.odd = append(m.odd, "export without a window")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch {
		case end.Before(start):
			m.odd = append(m.odd, "export ending before it starts")
			_, _ = io.WriteString(w, "Error: Start date is greater than end date.")
			return
		case end.Sub(start) > 24*time.Hour:
			m.odd = append(m.odd, "export over 24 hours")
			_, _ = io.WriteString(w, "Error: Selected period is greater than 24 hours.")
			return
		}
		m.exports = append(m.exports, Export{Types: r.Form.Get("transactionTypes"), Start: start, End: end})
		var rows []string
		if strings.Contains(r.Form.Get("transactionTypes"), "REBILL") {
			for _, rb := range m.rebills {
				if at := rb.At.Truncate(time.Second); at.Before(start) || at.After(end) {
					continue
				}
				rows = append(rows, fmt.Sprintf(`"REBILL","999999","0000","%s","%s","%s","%s"`, rb.SubscriptionID, date(&rb.At), rb.TransactionID, rb.Amount))
			}
		}
		_, _ = io.WriteString(w, strings.Join(rows, "\n"))
	default:
		m.odd = append(m.odd, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func date(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.DateOnly)
}
