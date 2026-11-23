package messages

import (
	"embed"
	"errors"
	"fmt"
	htemplate "html/template"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strings"
	ttemplate "text/template"
	"text/template/parse"

	"github.com/open-rails/openrails/billing"
)

// builtinLanguage is the language of the built-in templates, and the last
// a message falls back to.
const builtinLanguage = "en"

//go:embed templates
var templateFiles embed.FS

// builtin is the built-in messages, as a messages document: for each event
// type <type>.subject, <type>.txt and <type>.html, and layout.txt and
// layout.html, under templates/<language>.
var builtin = mustLoadBuiltin()

func mustLoadBuiltin() billing.MessageSettings {
	out := billing.MessageSettings{Events: map[string]billing.MessageEventSettings{}, Layouts: map[string]billing.MessageLayout{}}
	err := fs.WalkDir(templateFiles, "templates", func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := templateFiles.ReadFile(name)
		if err != nil {
			return err
		}
		lang, file := path.Base(path.Dir(name)), path.Base(name)
		base, ext := strings.TrimSuffix(file, path.Ext(file)), path.Ext(file)
		if base == "layout" {
			l := out.Layouts[lang]
			switch ext {
			case ".html":
				l.HTML = string(raw)
			case ".txt":
				l.Text = string(raw)
			default:
				return fmt.Errorf("%s: unknown layout part", name)
			}
			out.Layouts[lang] = l
			return nil
		}
		e := out.Events[base]
		if e.Templates == nil {
			e.Templates = map[string]billing.MessageTemplate{}
		}
		t := e.Templates[lang]
		switch ext {
		case ".subject":
			t.Subject = strings.TrimSpace(string(raw))
		case ".txt":
			t.Text = string(raw)
		case ".html":
			t.HTML = string(raw)
		default:
			return fmt.Errorf("%s: unknown template part", name)
		}
		e.Templates[lang] = t
		out.Events[base] = e
		return nil
	})
	if err != nil {
		panic("messages: built-in templates: " + err.Error())
	}
	return out
}

// funcs is the fixed set of functions templates call beside Go's built-in
// template functions.
var funcs = map[string]any{
	// default is v, or fallback when v is empty: {{default "there" .Customer.Name}}.
	"default": func(fallback, v string) string {
		if strings.TrimSpace(v) == "" {
			return fallback
		}
		return v
	},
	"upper": strings.ToUpper,
	"lower": strings.ToLower,
}

// languages is the order a message's parts are looked for in: the
// customer's language, the merchant's default, then the built-in one.
// Customers carry no language yet, so customer is always empty.
func languages(s billing.MessageSettings, customer string) []string {
	out := make([]string, 0, 3)
	for _, lang := range []string{customer, s.DefaultLanguage, builtinLanguage} {
		if lang != "" && !slices.Contains(out, lang) {
			out = append(out, lang)
		}
	}
	return out
}

// resolve picks eventType's template and layout for a customer whose
// language is customer: each part is the merchant's in the first language
// that has it, else the built-in one. lang is the template's language.
func resolve(s billing.MessageSettings, eventType, customer string) (t billing.MessageTemplate, layout billing.MessageLayout, lang string, ok bool) {
	chain := languages(s, customer)
	for _, l := range chain {
		if c, found := s.Events[eventType].Templates[l]; found {
			t, lang, ok = c, l, true
			break
		}
		if b, found := builtin.Events[eventType].Templates[l]; found {
			t, lang, ok = b, l, true
			break
		}
	}
	for _, sets := range []billing.MessageSettings{s, builtin} {
		for _, l := range chain {
			if layout.HTML == "" {
				layout.HTML = sets.Layouts[l].HTML
			}
			if layout.Text == "" {
				layout.Text = sets.Layouts[l].Text
			}
		}
	}
	return t, layout, lang, ok
}

// Render renders data's message in s, for a customer whose language is
// customer. data.Language is set to the language it is written in.
func Render(s billing.MessageSettings, customer string, data billing.MessageData) (billing.MessageContent, error) {
	t, layout, lang, ok := resolve(s, data.EventType, customer)
	if !ok {
		return billing.MessageContent{}, fmt.Errorf("%s has no template", data.EventType)
	}
	m, err := compile(t, layout)
	if err != nil {
		return billing.MessageContent{}, err
	}
	data.Language = lang
	return m.render(data)
}

// message is one template, compiled with its layout.
type message struct {
	subject *ttemplate.Template
	// text and html run "layout"; nil is no such part.
	text *ttemplate.Template
	html *htemplate.Template
}

func compile(t billing.MessageTemplate, layout billing.MessageLayout) (*message, error) {
	if strings.TrimSpace(t.Subject) == "" {
		return nil, errors.New("subject is required")
	}
	if strings.TrimSpace(t.Text) == "" && strings.TrimSpace(t.HTML) == "" {
		return nil, errors.New("a text or html body is required")
	}
	m := &message{}
	var err error
	if m.subject, err = ttemplate.New("subject").Funcs(funcs).Option("missingkey=error").Parse(t.Subject); err != nil {
		return nil, fmt.Errorf("subject: %w", err)
	}
	if strings.TrimSpace(t.Text) != "" {
		if m.text, err = compileText(layout.Text, t.Text); err != nil {
			return nil, fmt.Errorf("text: %w", err)
		}
	}
	if strings.TrimSpace(t.HTML) != "" {
		if m.html, err = compileHTML(layout.HTML, t.HTML); err != nil {
			return nil, fmt.Errorf("html: %w", err)
		}
	}
	return m, nil
}

func compileText(layout, body string) (*ttemplate.Template, error) {
	t, err := ttemplate.New("layout").Funcs(funcs).Option("missingkey=error").Parse(layout)
	if err != nil {
		return nil, fmt.Errorf("layout: %w", err)
	}
	if !callsBody(t.Tree) {
		return nil, errNoBody
	}
	if _, err := t.New("body").Parse(body); err != nil {
		return nil, err
	}
	return t, nil
}

func compileHTML(layout, body string) (*htemplate.Template, error) {
	t, err := htemplate.New("layout").Funcs(funcs).Option("missingkey=error").Parse(layout)
	if err != nil {
		return nil, fmt.Errorf("layout: %w", err)
	}
	if !callsBody(t.Tree) {
		return nil, errNoBody
	}
	if _, err := t.New("body").Parse(body); err != nil {
		return nil, err
	}
	return t, nil
}

var errNoBody = errors.New(`layout: it never runs {{template "body" .}}`)

// callsBody reports whether the tree runs the body template.
func callsBody(t *parse.Tree) bool {
	if t == nil {
		return false
	}
	var walk func(parse.Node) bool
	walk = func(n parse.Node) bool {
		switch n := n.(type) {
		case *parse.ListNode:
			if n == nil {
				return false
			}
			for _, c := range n.Nodes {
				if walk(c) {
					return true
				}
			}
		case *parse.TemplateNode:
			return n.Name == "body"
		case *parse.IfNode:
			return walk(n.List) || walk(n.ElseList)
		case *parse.RangeNode:
			return walk(n.List) || walk(n.ElseList)
		case *parse.WithNode:
			return walk(n.List) || walk(n.ElseList)
		}
		return false
	}
	return walk(t.Root)
}

func (m *message) render(data billing.MessageData) (billing.MessageContent, error) {
	var out billing.MessageContent
	var b strings.Builder
	if err := m.subject.Execute(&b, data); err != nil {
		return out, fmt.Errorf("subject: %w", err)
	}
	if out.Subject = strings.Join(strings.Fields(b.String()), " "); out.Subject == "" {
		return out, errors.New("subject: renders empty")
	}
	if m.text != nil {
		b.Reset()
		if err := m.text.ExecuteTemplate(&b, "layout", data); err != nil {
			return out, fmt.Errorf("text: %w", err)
		}
		out.Text = tidy(b.String())
	}
	if m.html != nil {
		b.Reset()
		if err := m.html.ExecuteTemplate(&b, "layout", data); err != nil {
			return out, fmt.Errorf("html: %w", err)
		}
		out.HTML = strings.TrimSpace(b.String()) + "\n"
	}
	if strings.TrimSpace(out.Text) == "" && strings.TrimSpace(out.HTML) == "" {
		return out, errors.New("the body renders empty")
	}
	return out, nil
}

var (
	trailingSpace = regexp.MustCompile(`[ \t]+\n`)
	blankLines    = regexp.MustCompile(`\n{3,}`)
)

// tidy keeps a text body readable whatever its conditionals left: no
// trailing spaces, no run of blank lines, one final newline.
func tidy(text string) string {
	text = trailingSpace.ReplaceAllString(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	text = blankLines.ReplaceAllString(text, "\n\n")
	if text = strings.TrimSpace(text); text == "" {
		return ""
	}
	return text + "\n"
}
