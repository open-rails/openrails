package config

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/goccy/go-yaml"

	"github.com/open-rails/openrails/billing"
	"github.com/open-rails/openrails/internal/custodians"
	"github.com/open-rails/openrails/internal/db/models"
	"github.com/open-rails/openrails/internal/modules/payments/rails"
)

// A PSP or custodian document is flat: its rail's (or kind's) own fields sit
// beside rail, environment and archived, and only its credentials nest, under
// secrets. The rail or kind decides which fields exist; any other is refused.

// PSPDocument is a PSP as its document spells it.
type PSPDocument struct {
	Rail        string
	Environment string
	// AccountID is the PSP's account: its rail's account fields (gateway_id,
	// account_id, client_accnum and client_subacc, treasury) joined with "-".
	AccountID string
	Custodian string
	Archived  bool
	// Settings are the rail's other fields.
	Settings map[string]any
	Secrets  map[string]string
}

// PSPDocumentFields are the fields a PSP document on rail takes.
func PSPDocumentFields(rail models.Rail) []string {
	d, _ := rails.Lookup(rail)
	out := append([]string{"rail", "environment", "archived"}, d.Fields...)
	return append(append(out, d.SettingKeys...), "secrets")
}

// DecodePSPDocument reads a PSP document, refusing a field its rail does not
// take.
func DecodePSPDocument(fields map[string]any) (PSPDocument, error) {
	var d PSPDocument
	rail, err := documentString(fields, "rail")
	if err != nil {
		return d, err
	}
	desc, ok := rails.Lookup(models.Rail(rail))
	if !ok {
		names := make([]string, 0, 4)
		for _, r := range rails.All() {
			names = append(names, string(r.Rail))
		}
		slices.Sort(names)
		if strings.TrimSpace(rail) == "" {
			return d, fmt.Errorf("rail is required (%s)", strings.Join(names, ", "))
		}
		return d, fmt.Errorf("unknown rail %q (%s)", rail, strings.Join(names, ", "))
	}
	d.Rail = string(desc.Rail)
	var unknown []string
	account := map[string]string{}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		value := fields[key]
		switch {
		case key == "rail":
		case key == "environment":
			d.Environment, err = documentString(fields, key)
		case key == "archived":
			d.Archived, err = documentBool(key, value)
		case key == "secrets":
			d.Secrets, err = documentSecrets(value)
		case slices.Contains(desc.AccountFields, key):
			account[key], err = documentString(fields, key)
		case key == "custodian" && slices.Contains(desc.Fields, key):
			d.Custodian, err = documentString(fields, key)
		case slices.Contains(desc.SettingKeys, key):
			if d.Settings == nil {
				d.Settings = map[string]any{}
			}
			d.Settings[key] = value
		default:
			unknown = append(unknown, strconv.Quote(key))
		}
		if err != nil {
			return d, err
		}
	}
	if len(unknown) > 0 {
		return d, fmt.Errorf("a %s PSP has no %s (%s takes %s)", d.Rail, strings.Join(unknown, ", "), d.Rail, strings.Join(PSPDocumentFields(desc.Rail), ", "))
	}
	d.AccountID = rails.Account(desc.Rail, account)
	return d, nil
}

// Fields is the document's fields.
func (d PSPDocument) Fields() map[string]any {
	out := map[string]any{"rail": d.Rail}
	maps.Copy(out, d.Settings)
	if d.Environment != "" {
		out["environment"] = d.Environment
	}
	for name, value := range rails.AccountParts(models.Rail(d.Rail), d.AccountID) {
		if value != "" {
			out[name] = value
		}
	}
	if d.Custodian != "" {
		out["custodian"] = d.Custodian
	}
	if d.Archived {
		out["archived"] = true
	}
	if len(d.Secrets) > 0 {
		out["secrets"] = d.Secrets
	}
	return out
}

// PSPs are a merchant's declared PSPs by key.
type PSPs map[string]PSPConfig

// UnmarshalYAML reads each PSP's flat document, naming the PSP a refusal is
// about.
func (p *PSPs) UnmarshalYAML(raw []byte) error {
	var docs map[string]map[string]any
	if err := yaml.Unmarshal(raw, &docs); err != nil {
		return err
	}
	out := make(PSPs, len(docs))
	for _, key := range slices.Sorted(maps.Keys(docs)) {
		var psp PSPConfig
		if err := psp.fromDocument(docs[key]); err != nil {
			return fmt.Errorf("psps.%s: %w", key, err)
		}
		out[key] = psp
	}
	*p = out
	return nil
}

// Custodians are a merchant's declared custodians by key.
type Custodians map[string]CustodianConfig

// UnmarshalYAML reads each custodian's flat document, naming the custodian a
// refusal is about.
func (c *Custodians) UnmarshalYAML(raw []byte) error {
	var docs map[string]map[string]any
	if err := yaml.Unmarshal(raw, &docs); err != nil {
		return err
	}
	out := make(Custodians, len(docs))
	for _, key := range slices.Sorted(maps.Keys(docs)) {
		d, err := DecodeCustodianDocument(docs[key], false)
		if err != nil {
			return fmt.Errorf("custodians.%s: %w", key, err)
		}
		out[key] = CustodianConfig{Kind: d.Kind, AccountID: d.AccountID, Archived: d.Archived, Settings: d.Settings, Secrets: d.Secrets}
	}
	*c = out
	return nil
}

func (p *PSPConfig) fromDocument(fields map[string]any) error {
	d, err := DecodePSPDocument(fields)
	if err != nil {
		return err
	}
	*p = PSPConfig{Rail: billing.Rail(d.Rail), Environment: d.Environment, AccountID: d.AccountID, Custodian: d.Custodian, Archived: d.Archived, Settings: d.Settings, Secrets: d.Secrets}
	return nil
}

// UnmarshalYAML reads a PSP's flat document.
func (p *PSPConfig) UnmarshalYAML(raw []byte) error {
	var fields map[string]any
	if err := yaml.Unmarshal(raw, &fields); err != nil {
		return err
	}
	return p.fromDocument(fields)
}

// MarshalYAML writes a PSP's flat document.
func (p PSPConfig) MarshalYAML() (any, error) {
	first := append([]string{"rail", "environment"}, rails.AccountFields(models.Rail(p.Rail))...)
	return orderedFields(PSPDocument{Rail: string(p.Rail), Environment: p.Environment, AccountID: p.AccountID, Custodian: p.Custodian, Archived: p.Archived, Settings: p.Settings, Secrets: p.Secrets}.Fields(), first...), nil
}

// CustodianDocument is a custodian as its document spells it.
type CustodianDocument struct {
	Kind string
	// Environment is the custodian's money class (live or test) in a stored
	// document; a declaration has none.
	Environment string
	AccountID   string
	Archived    bool
	// Settings are the kind's own fields.
	Settings map[string]any
	Secrets  map[string]string
}

// CustodianDocumentFields are the fields a custodian document of kind takes.
func CustodianDocumentFields(kind string, environment bool) []string {
	out := []string{"kind"}
	if environment {
		out = append(out, "environment")
	}
	out = append(out, "account_id", "archived")
	if d, ok := custodians.Get(kind); ok {
		out = append(out, d.SettingNames()...)
	}
	return append(out, "secrets")
}

// DecodeCustodianDocument reads a custodian document, refusing a field its
// kind does not take. environment says whether it carries its money class.
func DecodeCustodianDocument(fields map[string]any, environment bool) (CustodianDocument, error) {
	var d CustodianDocument
	kind, err := documentString(fields, "kind")
	if err != nil {
		return d, err
	}
	desc, err := custodians.Require(kind)
	if err != nil {
		return d, err
	}
	d.Kind = desc.Kind
	var unknown []string
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		value := fields[key]
		_, setting := desc.Setting(key)
		switch {
		case key == "kind":
		case key == "environment" && environment:
			d.Environment, err = documentString(fields, key)
		case key == "account_id":
			d.AccountID, err = documentString(fields, key)
		case key == "archived":
			d.Archived, err = documentBool(key, value)
		case key == "secrets":
			d.Secrets, err = documentSecrets(value)
		case setting && key == strings.ToLower(key):
			if d.Settings == nil {
				d.Settings = map[string]any{}
			}
			d.Settings[key] = value
		default:
			unknown = append(unknown, strconv.Quote(key))
		}
		if err != nil {
			return d, err
		}
	}
	if len(unknown) > 0 {
		return d, fmt.Errorf("a %s custodian has no %s (%s takes %s)", d.Kind, strings.Join(unknown, ", "), d.Kind, strings.Join(CustodianDocumentFields(d.Kind, environment), ", "))
	}
	return d, nil
}

// Fields is the document's fields.
func (d CustodianDocument) Fields() map[string]any {
	out := map[string]any{"kind": d.Kind}
	maps.Copy(out, d.Settings)
	if d.Environment != "" {
		out["environment"] = d.Environment
	}
	if d.AccountID != "" {
		out["account_id"] = d.AccountID
	}
	if d.Archived {
		out["archived"] = true
	}
	if len(d.Secrets) > 0 {
		out["secrets"] = d.Secrets
	}
	return out
}

// UnmarshalYAML reads a custodian's flat document.
func (c *CustodianConfig) UnmarshalYAML(raw []byte) error {
	var fields map[string]any
	if err := yaml.Unmarshal(raw, &fields); err != nil {
		return err
	}
	d, err := DecodeCustodianDocument(fields, false)
	if err != nil {
		return err
	}
	*c = CustodianConfig{Kind: d.Kind, AccountID: d.AccountID, Archived: d.Archived, Settings: d.Settings, Secrets: d.Secrets}
	return nil
}

// MarshalYAML writes a custodian's flat document.
func (c CustodianConfig) MarshalYAML() (any, error) {
	return orderedFields(CustodianDocument{Kind: c.Kind, AccountID: c.AccountID, Archived: c.Archived, Settings: c.Settings, Secrets: c.Secrets}.Fields(), "kind", "account_id"), nil
}

// orderedFields renders fields with first leading, the rest sorted and
// secrets last.
func orderedFields(fields map[string]any, first ...string) yaml.MapSlice {
	out := yaml.MapSlice{}
	for _, key := range first {
		if v, ok := fields[key]; ok {
			out = append(out, yaml.MapItem{Key: key, Value: v})
		}
	}
	for _, key := range slices.Sorted(maps.Keys(fields)) {
		if slices.Contains(first, key) || key == "secrets" {
			continue
		}
		out = append(out, yaml.MapItem{Key: key, Value: fields[key]})
	}
	if v, ok := fields["secrets"]; ok {
		out = append(out, yaml.MapItem{Key: "secrets", Value: v})
	}
	return out
}

func documentString(fields map[string]any, key string) (string, error) {
	switch v := fields[key].(type) {
	case nil:
		return "", nil
	case string:
		return v, nil
	case int, int64, uint64, float64:
		return "", fmt.Errorf("%s must be a string: quote it (%s: \"%v\")", key, key, v)
	default:
		return "", fmt.Errorf("%s must be a string", key)
	}
}

func documentBool(key string, value any) (bool, error) {
	switch v := value.(type) {
	case nil:
		return false, nil
	case bool:
		return v, nil
	default:
		return false, fmt.Errorf("%s must be true or false", key)
	}
}

func documentSecrets(value any) (map[string]string, error) {
	if value == nil {
		return nil, nil
	}
	in, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("secrets must be a map of names to values")
	}
	out := make(map[string]string, len(in))
	for key, v := range in {
		switch s := v.(type) {
		case string:
			out[key] = s
		case nil:
		default:
			return nil, fmt.Errorf("secrets.%s must be a string", key)
		}
	}
	return out, nil
}
