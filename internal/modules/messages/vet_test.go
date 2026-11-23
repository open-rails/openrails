package messages

import (
	"errors"
	"strings"
	"testing"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/db/models"
)

// receiptWith is a messages document whose receipt is tmpl in English.
func receiptWith(tmpl billing.MessageTemplate) billing.MessageSettings {
	return billing.MessageSettings{Events: map[string]billing.MessageEventSettings{
		string(models.NotificationOneOffPurchaseCompleted): {Templates: map[string]billing.MessageTemplate{"en": tmpl}},
	}}
}

func TestCheckTypesEveryBranch(t *testing.T) {
	for _, c := range []struct{ body, want string }{
		{`{{if eq .Event.Reason "never"}}{{.Event.Totl}}{{end}}`, "can't evaluate field Totl in type billing.MessageEvent"},
		{`{{if .Event.Amount}}x{{else}}{{.Merchant.Phone}}{{end}}`, "can't evaluate field Phone in type billing.MessageMerchant"},
		{`{{with .Event}}{{.Amount}}{{.Customer}}{{end}}`, "can't evaluate field Customer in type billing.MessageEvent"},
		{`{{with .Event.Amount}}{{.Value}}{{end}}`, "can't evaluate field Value in type string"},
		{`{{with .Event}}{{$.Customer.Nick}}{{end}}`, "can't evaluate field Nick in type billing.MessageCustomer"},
		{`{{with .Event.Amount}}x{{else with .Links.Nowhere}}y{{end}}`, "can't evaluate field Nowhere in type billing.MessageLinks"},
		{`{{default "x" (.Event).Totl}}`, "can't evaluate field Totl"},
		{`{{if and .Event.AccessEnded (eq .Event.Nope "x")}}x{{end}}`, "can't evaluate field Nope"},
		{`{{upper .Event.AccessEnded}}`, "wrong type for value; expected string; got bool"},
		{`{{upper .Event.RetryStatus}}`, "wrong type for value; expected string; got billing.NotificationRetryStatus"},
		{`{{default "x"}}`, "wrong number of args for default: want 2 got 1"},
	} {
		err := Check(receiptWith(billing.MessageTemplate{Subject: "Receipt", Text: c.body}))
		if err == nil || !strings.Contains(err.Error(), c.want) || !strings.HasPrefix(err.Error(), "events.one_off_purchase_completed: text: template: body:1:") {
			t.Errorf("%s: want %q, got %v", c.body, c.want, err)
		}
	}
	valid := []string{
		`{{if eq .Event.Reason "never"}}{{.Event.Amount}}{{else}}{{.Customer.Name}}{{end}}`,
		`{{with .Event}}{{.Amount}} {{$.Merchant.Name}}{{end}}{{.Event.Amount | default "free" | upper}}`,
		`{{if and .Event.AccessEnded (not .Event.DowngradeApplied)}}{{len .Event.ProductName}}{{slice .Event.ProductName 0 1}}{{end}}`,
		`{{if eq .Event.RetryStatus "scheduled"}}{{index .Event.ProductName 0}}{{end}}{{/* a comment */}}`,
	}
	for _, body := range valid {
		if err := Check(receiptWith(billing.MessageTemplate{Subject: "Receipt", Text: body, HTML: "<p>" + body + "</p>"})); err != nil {
			t.Errorf("%s: %v", body, err)
		}
	}
	layout := billing.MessageSettings{Layouts: map[string]billing.MessageLayout{"en": {Text: `{{if .Merchant.Name}}{{.Merchant.Slogan}}{{end}}{{template "body" .}}`}}}
	if err := Check(layout); err == nil || !strings.Contains(err.Error(), "layouts.en: text: template: layout:1:") || !strings.Contains(err.Error(), "Slogan") {
		t.Errorf("a layout's fields are checked: %v", err)
	}
}

func TestTemplatesCannotGrowWithoutBound(t *testing.T) {
	for _, c := range []struct {
		name string
		tmpl billing.MessageTemplate
		want string
	}{
		{"range over an integer", billing.MessageTemplate{Subject: "x", Text: "{{range 1000000000}}x{{end}}"}, "text: template: body:1:8: range is not allowed"},
		{"a define", billing.MessageTemplate{Subject: "x", Text: `{{define "a"}}{{template "a" .}}{{end}}x`}, "text: define and block are not allowed"},
		{"a block", billing.MessageTemplate{Subject: "x", HTML: `{{block "a" .}}x{{end}}`}, "html: define and block are not allowed"},
		{"a body redefining the layout", billing.MessageTemplate{Subject: "x", Text: `{{define "layout"}}y{{end}}x`}, "define and block are not allowed"},
		{"a template call", billing.MessageTemplate{Subject: "x", Text: `{{template "layout" .}}`}, `{{template "layout"}} is not allowed`},
		{"a subject's template call", billing.MessageTemplate{Subject: `{{template "subject" .}}`, Text: "x"}, `subject: template: subject:1:11: {{template "subject"}} is not allowed`},
		{"a variable", billing.MessageTemplate{Subject: "x", Text: `{{$a := .Event.Amount}}{{$a}}`}, "variables are not allowed"},
		{"printf", billing.MessageTemplate{Subject: "x", Text: `{{printf "%[1]s%[1]s" .Event.Amount}}`}, `function "printf" is not allowed`},
		{"html", billing.MessageTemplate{Subject: "x", Text: `{{html (html .Event.Amount)}}`}, `function "html" is not allowed`},
		{"print in a parenthesis", billing.MessageTemplate{Subject: "x", Text: `{{default "x" (print .Event.Amount)}}`}, `function "print" is not allowed`},
		{"a long source", billing.MessageTemplate{Subject: "x", Text: strings.Repeat("x", maxSource+1)}, "text: is 65537 bytes; a template part is at most 64 KiB"},
	} {
		err := Check(receiptWith(c.tmpl))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want %q, got %v", c.name, c.want, err)
		}
	}
	twice := billing.MessageSettings{Layouts: map[string]billing.MessageLayout{"en": {Text: `{{template "body" .}}{{template "body" .}}`}}}
	if err := Check(twice); err == nil || !strings.Contains(err.Error(), `a layout runs {{template "body" .}} once`) {
		t.Errorf("a layout runs its body once: %v", err)
	}
	long := billing.MessageSettings{Layouts: map[string]billing.MessageLayout{"en": {HTML: strings.Repeat(" ", maxSource) + `{{template "body" .}}`}}}
	if err := Check(long); err == nil || !strings.Contains(err.Error(), "layouts.en: html: layout: is 65557 bytes") {
		t.Errorf("a layout's source is capped: %v", err)
	}
}

func TestARenderStopsAtItsCap(t *testing.T) {
	data := Sample(string(models.NotificationOneOffPurchaseCompleted))
	data.Customer.Name = strings.Repeat("a", 4096)
	body := strings.Repeat("{{.Customer.Name}}", 100)
	_, err := Render(receiptWith(billing.MessageTemplate{Subject: "x", Text: body}), "", data)
	if !errors.Is(err, errTooLong) || !strings.HasPrefix(err.Error(), "text: ") {
		t.Fatalf("text: %v", err)
	}
	_, err = Render(receiptWith(billing.MessageTemplate{Subject: "x", HTML: body}), "", data)
	if !errors.Is(err, errTooLong) || !strings.HasPrefix(err.Error(), "html: ") {
		t.Fatalf("html: %v", err)
	}
	data.Customer.Name = strings.Repeat("a", 1024)
	if _, err := Render(receiptWith(billing.MessageTemplate{Subject: "x", Text: body}), "", data); err != nil {
		t.Fatalf("under the cap: %v", err)
	}
}
