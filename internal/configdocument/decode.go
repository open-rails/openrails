// Package configdocument provides bounded, strict configuration document decoding.
package configdocument

import (
	"bytes"
	"encoding/json"
	"fmt"
	"github.com/goccy/go-yaml"
	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/parser"
	"io"
)

func YAMLToJSON(raw []byte, maxBytes int) ([]byte, error) {
	if len(raw) == 0 || len(raw) > maxBytes {
		return nil, fmt.Errorf("configuration document must contain 1..%d bytes", maxBytes)
	}
	file, err := parser.ParseBytes(raw, 0)
	if err != nil {
		return nil, err
	}
	if len(file.Docs) != 1 {
		return nil, fmt.Errorf("configuration requires exactly one YAML document")
	}
	guard := &applicationYAMLGuard{state: &applicationYAMLState{}}
	ast.Walk(guard, file.Docs[0])
	if guard.state.err != nil {
		return nil, guard.state.err
	}
	return yaml.YAMLToJSON(raw)
}

func GuardJSON(raw []byte, maxBytes int) error {
	if len(raw) == 0 || len(raw) > maxBytes {
		return fmt.Errorf("configuration document must contain 1..%d bytes", maxBytes)
	}
	guard := json.NewDecoder(bytes.NewReader(raw))
	guard.UseNumber()
	if err := applicationJSONValue(guard, 0); err != nil {
		return err
	}
	if _, err := guard.Token(); err != io.EOF {
		return fmt.Errorf("configuration contains trailing JSON")
	}
	return nil
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
		v.state.err = fmt.Errorf("configuration YAML exceeds structural limits")
		return nil
	}
	switch node.Type() {
	case ast.AnchorType, ast.AliasType, ast.MergeKeyType, ast.TagType:
		v.state.err = fmt.Errorf("configuration YAML aliases, anchors, merge keys and tags are not supported")
		return nil
	}
	return &applicationYAMLGuard{depth: v.depth + 1, state: v.state}
}

func applicationJSONValue(d *json.Decoder, depth int) error {
	if depth > 32 {
		return fmt.Errorf("configuration JSON exceeds nesting limit")
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
				return fmt.Errorf("duplicate or invalid configuration JSON field %q", key)
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
		return fmt.Errorf("unexpected configuration JSON delimiter")
	}
	_, err = d.Token()
	return err
}
