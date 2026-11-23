package messages

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"text/template/parse"

	"github.com/open-rails/openrails/billing"
)

// A template's rendering is linear in its source: no loop, no template that
// calls another (but a layout's one body), no variable and no function that
// repeats or grows its argument. These bound the rest.
const (
	// maxSource is the longest a template part's source may be.
	maxSource = 64 << 10
	// maxOutput is the most a part may render.
	maxOutput = 256 << 10
)

// functions are what a template may call: Go's comparisons and logic, len
// and funcs. print, printf, println, html, js, urlquery and call are refused,
// since each can repeat or grow what it is given; index and slice, since
// version 1 data needs neither.
var functions = map[string]bool{
	"and": true, "or": true, "not": true, "eq": true, "ne": true, "lt": true, "le": true, "gt": true, "ge": true,
	"len": true, "default": true, "upper": true, "lower": true,
}

var (
	errNoBody  = errors.New(`layout: it never runs {{template "body" .}}`)
	errTooLong = fmt.Errorf("renders more than %d KiB", maxOutput>>10)
	errDefined = errors.New("define and block are not allowed")
)

// sized refuses a part's source over maxSource.
func sized(src string) error {
	if len(src) > maxSource {
		return fmt.Errorf("is %d bytes; a template part is at most %d KiB", len(src), maxSource>>10)
	}
	return nil
}

// vet refuses what tree may not do: range, a variable, a function outside
// functions, and a template call but a layout's one {{template "body" .}}.
// Its defines are refused where it is parsed.
func vet(tree *parse.Tree, layout bool) error {
	v := &vetter{tree: tree, layout: layout}
	if err := v.list(tree.Root); err != nil {
		return err
	}
	if layout && v.bodies == 0 {
		return errNoBody
	}
	return nil
}

type vetter struct {
	tree   *parse.Tree
	layout bool
	bodies int
}

func (v *vetter) errorf(n parse.Node, format string, args ...any) error {
	location, _ := v.tree.ErrorContext(n)
	return fmt.Errorf("template: %s: %s", location, fmt.Sprintf(format, args...))
}

func (v *vetter) list(l *parse.ListNode) error {
	if l == nil {
		return nil
	}
	for _, n := range l.Nodes {
		if err := v.node(n); err != nil {
			return err
		}
	}
	return nil
}

func (v *vetter) node(n parse.Node) error {
	switch n := n.(type) {
	case *parse.TextNode, *parse.CommentNode:
		return nil
	case *parse.ActionNode:
		return v.pipe(n.Pipe)
	case *parse.IfNode:
		return v.branch(&n.BranchNode)
	case *parse.WithNode:
		return v.branch(&n.BranchNode)
	case *parse.RangeNode:
		return v.errorf(n, "range is not allowed")
	case *parse.TemplateNode:
		if !v.layout || n.Name != "body" {
			return v.errorf(n, `{{template %q}} is not allowed: only a layout runs a template, {{template "body" .}}`, n.Name)
		}
		if v.bodies++; v.bodies > 1 {
			return v.errorf(n, `a layout runs {{template "body" .}} once`)
		}
		return v.pipe(n.Pipe)
	}
	return v.errorf(n, "%s is not allowed", n)
}

func (v *vetter) branch(b *parse.BranchNode) error {
	if err := v.pipe(b.Pipe); err != nil {
		return err
	}
	if err := v.list(b.List); err != nil {
		return err
	}
	return v.list(b.ElseList)
}

func (v *vetter) pipe(p *parse.PipeNode) error {
	if p == nil {
		return nil
	}
	if len(p.Decl) > 0 {
		return v.errorf(p.Decl[0], "variables are not allowed: use with, and $ for the message's data")
	}
	for _, c := range p.Cmds {
		for _, arg := range c.Args {
			if err := v.arg(arg); err != nil {
				return err
			}
		}
	}
	return nil
}

func (v *vetter) arg(n parse.Node) error {
	switch n := n.(type) {
	case *parse.IdentifierNode:
		if !functions[n.Ident] {
			return v.errorf(n, "function %q is not allowed", n.Ident)
		}
	case *parse.PipeNode:
		return v.pipe(n)
	case *parse.ChainNode:
		return v.arg(n.Node)
	}
	return nil
}

var (
	dataType   = reflect.TypeFor[billing.MessageData]()
	stringType = reflect.TypeFor[string]()
	boolType   = reflect.TypeFor[bool]()
	intType    = reflect.TypeFor[int]()
)

// typecheck refuses a field tree names that its data does not have, in any
// branch, and a value a function cannot take: a render meets only the
// branches its data takes. body is the tree a layout's {{template "body" .}}
// runs, nil for a part without one.
func typecheck(tree, body *parse.Tree) error {
	return (&checker{tree: tree, root: dataType, body: body}).list(tree.Root, dataType)
}

type checker struct {
	tree *parse.Tree
	// root is $, the data tree runs over.
	root reflect.Type
	body *parse.Tree
}

func (c *checker) errorf(n parse.Node, format string, args ...any) error {
	location, context := c.tree.ErrorContext(n)
	return fmt.Errorf("template: %s: at <%s>: %s", location, context, fmt.Sprintf(format, args...))
}

func (c *checker) list(l *parse.ListNode, dot reflect.Type) error {
	if l == nil {
		return nil
	}
	for _, n := range l.Nodes {
		if err := c.node(n, dot); err != nil {
			return err
		}
	}
	return nil
}

func (c *checker) node(n parse.Node, dot reflect.Type) error {
	switch n := n.(type) {
	case *parse.ActionNode:
		_, err := c.pipe(n.Pipe, dot)
		return err
	case *parse.IfNode:
		if _, err := c.pipe(n.Pipe, dot); err != nil {
			return err
		}
		if err := c.list(n.List, dot); err != nil {
			return err
		}
		return c.list(n.ElseList, dot)
	case *parse.WithNode:
		inner, err := c.pipe(n.Pipe, dot)
		if err != nil {
			return err
		}
		if err := c.list(n.List, inner); err != nil {
			return err
		}
		return c.list(n.ElseList, dot)
	case *parse.TemplateNode:
		data, err := c.pipe(n.Pipe, dot)
		if err != nil || c.body == nil {
			return err
		}
		return (&checker{tree: c.body, root: data}).list(c.body.Root, data)
	}
	return nil
}

func (c *checker) pipe(p *parse.PipeNode, dot reflect.Type) (reflect.Type, error) {
	if p == nil {
		return nil, nil
	}
	var result reflect.Type
	for i, cmd := range p.Cmds {
		t, err := c.command(cmd, dot, i > 0, result)
		if err != nil {
			return nil, err
		}
		result = t
	}
	return result, nil
}

// command is cmd's type; piped is whether the previous command's result, of
// type in, is its last argument.
func (c *checker) command(cmd *parse.CommandNode, dot reflect.Type, piped bool, in reflect.Type) (reflect.Type, error) {
	args := make([]reflect.Type, 0, len(cmd.Args))
	for _, a := range cmd.Args[1:] {
		t, err := c.operand(a, dot)
		if err != nil {
			return nil, err
		}
		args = append(args, t)
	}
	if fn, ok := cmd.Args[0].(*parse.IdentifierNode); ok {
		if piped {
			args = append(args, in)
		}
		return c.call(fn, args)
	}
	t, err := c.operand(cmd.Args[0], dot)
	if err == nil && (len(args) > 0 || piped) && !c.method(cmd.Args[0], dot) {
		err = c.errorf(cmd.Args[0], "%s is not a function: it takes no arguments", cmd.Args[0])
	}
	return t, err
}

// method reports whether n names a method, which may take arguments.
func (c *checker) method(n parse.Node, dot reflect.Type) bool {
	t, idents := dot, []string(nil)
	switch n := n.(type) {
	case *parse.FieldNode:
		idents = n.Ident
	case *parse.VariableNode:
		t, idents = c.root, n.Ident[1:]
	case *parse.ChainNode:
		t, _ = c.operand(n.Node, dot)
		idents = n.Field
	}
	if len(idents) == 0 {
		return false
	}
	for _, name := range idents[:len(idents)-1] {
		t, _ = member(t, name)
	}
	if t == nil {
		return false
	}
	_, ok := t.MethodByName(idents[len(idents)-1])
	return ok
}

func (c *checker) operand(n parse.Node, dot reflect.Type) (reflect.Type, error) {
	switch n := n.(type) {
	case *parse.DotNode:
		return dot, nil
	case *parse.FieldNode:
		return c.fields(n, dot, n.Ident)
	case *parse.VariableNode:
		// $, the only variable vet admits.
		return c.fields(n, c.root, n.Ident[1:])
	case *parse.ChainNode:
		t, err := c.operand(n.Node, dot)
		if err != nil {
			return nil, err
		}
		return c.fields(n, t, n.Field)
	case *parse.PipeNode:
		return c.pipe(n, dot)
	case *parse.IdentifierNode:
		return c.call(n, nil)
	case *parse.StringNode:
		return stringType, nil
	case *parse.BoolNode:
		return boolType, nil
	case *parse.NumberNode:
		switch {
		case n.IsInt:
			return intType, nil
		case n.IsFloat:
			return reflect.TypeFor[float64](), nil
		}
	}
	return nil, nil
}

// fields is the type idents name from t, each a field or method of the last.
func (c *checker) fields(n parse.Node, t reflect.Type, idents []string) (reflect.Type, error) {
	for _, name := range idents {
		next, ok := member(t, name)
		if !ok {
			if t == nil {
				return nil, c.errorf(n, "can't evaluate field %s of a value whose type is not known", name)
			}
			return nil, c.errorf(n, "can't evaluate field %s in type %s", name, t)
		}
		t = next
	}
	return t, nil
}

// member is the type of t's exported field or method name.
func member(t reflect.Type, name string) (reflect.Type, bool) {
	if t == nil {
		return nil, false
	}
	if m, ok := t.MethodByName(name); ok && m.IsExported() {
		if m.Type.NumOut() == 0 {
			return nil, true
		}
		return m.Type.Out(0), true
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct:
		f, ok := t.FieldByName(name)
		if !ok || !f.IsExported() {
			return nil, false
		}
		return f.Type, true
	case reflect.Map:
		return t.Elem(), t.Key().Kind() == reflect.String
	}
	return nil, false
}

// call is the type fn returns given args of these types; nil when it is not
// known.
func (c *checker) call(fn *parse.IdentifierNode, args []reflect.Type) (reflect.Type, error) {
	switch fn.Ident {
	case "not":
		return boolType, nil
	case "eq", "ne", "lt", "le", "gt", "ge":
		return boolType, c.compared(fn, args)
	case "len":
		return intType, nil
	case "and", "or":
		// One of its arguments: their type when they share one.
		if len(args) == 0 {
			return nil, nil
		}
		for _, a := range args[1:] {
			if a != args[0] {
				return nil, nil
			}
		}
		return args[0], nil
	}
	f, ok := funcs[fn.Ident]
	if !ok {
		return nil, nil
	}
	ft := reflect.TypeOf(f)
	if len(args) != ft.NumIn() {
		return nil, c.errorf(fn, "wrong number of args for %s: want %d got %d", fn.Ident, ft.NumIn(), len(args))
	}
	for i, a := range args {
		if a != nil && !a.AssignableTo(ft.In(i)) {
			return nil, c.errorf(fn, "wrong type for value; expected %s; got %s", ft.In(i), a)
		}
	}
	return ft.Out(0), nil
}

// compared refuses what fn cannot compare: a value with one of another kind
// (int and uint compare with each other), and for an ordering a bool, a
// complex number or a value of no basic kind. eq takes two values or more,
// the others two.
func (c *checker) compared(fn *parse.IdentifierNode, args []reflect.Type) error {
	ordered := fn.Ident != "eq" && fn.Ident != "ne"
	if len(args) < 2 || ordered && len(args) != 2 {
		return c.errorf(fn, "wrong number of args for %s: got %d", fn.Ident, len(args))
	}
	first := args[0]
	for _, t := range args {
		if t == nil || first == nil {
			continue
		}
		k, kb := comparison(t)
		k0, k0b := comparison(first)
		switch {
		case ordered && (!kb || k == reflect.Bool || k == reflect.Complex128):
			return c.errorf(fn, "%s cannot order a %s", fn.Ident, t)
		case kb && k0b && k != k0, kb != k0b, !kb && (t != first || !t.Comparable()):
			return c.errorf(fn, "incompatible types for comparison: %s and %s", first, t)
		}
	}
	return nil
}

// comparison is t's kind as a comparison sees it: every integer is Int, every
// float Float64, every complex Complex128; false for no basic kind.
func comparison(t reflect.Type) (reflect.Kind, bool) {
	switch t.Kind() {
	case reflect.Bool, reflect.String:
		return t.Kind(), true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return reflect.Int, true
	case reflect.Float32, reflect.Float64:
		return reflect.Float64, true
	case reflect.Complex64, reflect.Complex128:
		return reflect.Complex128, true
	}
	return reflect.Invalid, false
}

// capped is a rendering that fails past maxOutput.
type capped struct{ strings.Builder }

func (c *capped) Write(p []byte) (int, error) {
	if c.Len()+len(p) > maxOutput {
		return 0, errTooLong
	}
	return c.Builder.Write(p)
}
