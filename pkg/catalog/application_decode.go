package catalog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
)

// ParseApplicationYAML rejects executable/reference-like YAML constructs and
// converts losslessly to the same bounded typed JSON contract used by HTTP.
func ParseApplicationYAML(raw []byte) (*Application, error) {
	if len(raw) == 0 || len(raw) > MaxApplicationBytes {
		return nil, fmt.Errorf("catalog application must contain 1..%d bytes", MaxApplicationBytes)
	}
	file, err := parser.ParseBytes(raw, 0)
	if err != nil {
		return nil, fmt.Errorf("parse catalog YAML: %w", err)
	}
	if len(file.Docs) != 1 {
		return nil, fmt.Errorf("catalog application must contain exactly one YAML document")
	}
	guard := &applicationYAMLGuard{state: &applicationYAMLState{}}
	ast.Walk(guard, file.Docs[0])
	if guard.state.err != nil {
		return nil, guard.state.err
	}
	body, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("decode catalog YAML: %w", err)
	}
	return ParseApplicationJSON(body)
}

type applicationYAMLGuard struct {
	depth int
	state *applicationYAMLState
}

type applicationYAMLState struct {
	nodes int
	err   error
}

func (v *applicationYAMLGuard) Visit(node ast.Node) ast.Visitor {
	if v.state.err != nil {
		return nil
	}
	if node == nil {
		return nil
	}
	v.state.nodes++
	if v.depth > 40 || v.state.nodes > 100000 {
		v.state.err = fmt.Errorf("catalog YAML exceeds structural limits")
		return nil
	}
	switch node.Type() {
	case ast.AnchorType, ast.AliasType, ast.MergeKeyType, ast.TagType:
		v.state.err = fmt.Errorf("catalog YAML aliases, anchors, merge keys and tags are not supported")
		return nil
	}
	return &applicationYAMLGuard{depth: v.depth + 1, state: v.state}
}

func ParseApplicationJSON(raw []byte) (*Application, error) {
	if len(raw) == 0 || len(raw) > MaxApplicationBytes {
		return nil, fmt.Errorf("catalog application must contain 1..%d bytes", MaxApplicationBytes)
	}
	// encoding/json otherwise silently accepts duplicate keys. Check every
	// object before decoding so two encodings cannot acquire one receipt identity.
	guard := json.NewDecoder(bytes.NewReader(raw))
	guard.UseNumber()
	if err := applicationJSONValue(guard, 0); err != nil {
		return nil, err
	}
	if _, err := guard.Token(); err != io.EOF {
		return nil, fmt.Errorf("catalog application contains trailing JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var out Application
	if err := decoder.Decode(&out); err != nil {
		return nil, fmt.Errorf("decode catalog application: %w", err)
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return &out, nil
}

func applicationJSONValue(d *json.Decoder, depth int) error {
	if depth > 32 {
		return fmt.Errorf("catalog JSON exceeds nesting limit")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, compound := token.(json.Delim)
	if !compound {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid catalog JSON field %q", key)
			}
			seen[name] = true
			if err := applicationJSONValue(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := applicationJSONValue(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected catalog JSON delimiter")
	}
	_, err = d.Token()
	return err
}
