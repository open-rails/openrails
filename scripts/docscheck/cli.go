package main

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"
)

// The operator CLI's command tree, read from its source: each
// &cobra.Command{Use: …} and the AddCommand calls that attach it. Use and
// the commands passed may come through function parameters, local
// variables, closures and range loops; scope is the function the literal
// was built in, with its arguments bound.

type scope struct {
	body   *ast.BlockStmt
	params map[string]boundArg
	outer  *scope
}

type boundArg struct {
	expr ast.Expr
	in   *scope
}

type cmdInst struct {
	lit *ast.CompositeLit
	in  *scope
}

type cliTree struct {
	funcs map[string]*ast.FuncDecl
}

func cliCommands(dir string) ([]string, error) {
	fset := token.NewFileSet()
	pkgs, err := parseGoDir(fset, dir)
	if err != nil {
		return nil, err
	}
	t := &cliTree{funcs: map[string]*ast.FuncDecl{}}
	for _, files := range pkgs {
		for _, f := range files {
			for _, d := range f.Decls {
				if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil && fd.Body != nil {
					t.funcs[fd.Name.Name] = fd
				}
			}
		}
	}
	var root *cmdInst
	for _, fd := range t.funcs {
		s := &scope{body: fd.Body}
		inspectBody(fd.Body, func(n ast.Node) {
			if lit, ok := n.(*ast.CompositeLit); ok && isCommand(lit) && root == nil {
				for _, name := range t.names(cmdInst{lit, s}) {
					if name == "openrails" {
						root = &cmdInst{lit, s}
					}
				}
			}
		})
	}
	if root == nil {
		return nil, fmt.Errorf("%s: no openrails root command", dir)
	}
	seen := map[string]bool{}
	var walk func(prefix string, c cmdInst, depth int)
	walk = func(prefix string, c cmdInst, depth int) {
		if depth > 8 {
			return
		}
		for _, child := range t.children(c) {
			for _, name := range t.names(child) {
				p := strings.TrimSpace(prefix + " " + name)
				if !seen[p] {
					seen[p] = true
					walk(p, child, depth+1)
				}
			}
		}
	}
	walk("", *root, 0)
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

func isCommand(lit *ast.CompositeLit) bool {
	sel, ok := lit.Type.(*ast.SelectorExpr)
	return ok && sel.Sel.Name == "Command" && fmt.Sprint(sel.X) == "cobra"
}

// inspectBody visits a function body without entering nested closures.
func inspectBody(body *ast.BlockStmt, visit func(ast.Node)) {
	ast.Inspect(body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if n != nil {
			visit(n)
		}
		return true
	})
}

// names is the first word of each value a command's Use can take.
func (t *cliTree) names(c cmdInst) []string {
	var out []string
	for _, e := range c.lit.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok || fmt.Sprint(kv.Key) != "Use" {
			continue
		}
		for _, s := range t.strs(kv.Value, c.in, 0) {
			if f := strings.Fields(s); len(f) > 0 {
				out = append(out, f[0])
			}
		}
	}
	return out
}

func (t *cliTree) strs(e ast.Expr, s *scope, depth int) []string {
	if depth > 12 {
		return nil
	}
	switch x := e.(type) {
	case *ast.BasicLit:
		if v, err := strconv.Unquote(x.Value); err == nil {
			return []string{v}
		}
	case *ast.ParenExpr:
		return t.strs(x.X, s, depth+1)
	case *ast.BinaryExpr:
		if x.Op == token.ADD {
			var out []string
			for _, l := range t.strs(x.X, s, depth+1) {
				for _, r := range t.strs(x.Y, s, depth+1) {
					out = append(out, l+r)
				}
			}
			return out
		}
	case *ast.Ident:
		var out []string
		for _, v := range t.values(x.Name, s) {
			out = append(out, t.strs(v.expr, v.in, depth+1)...)
		}
		return out
	}
	return nil
}

// values are the expressions a name can hold in scope s: a bound
// parameter, or each assignment to the local, or each element ranged over.
func (t *cliTree) values(name string, s *scope) []boundArg {
	for in := s; in != nil; in = in.outer {
		if b, ok := in.params[name]; ok {
			return []boundArg{b}
		}
		var out []boundArg
		inspectBody(in.body, func(n ast.Node) {
			switch st := n.(type) {
			case *ast.AssignStmt:
				for i, l := range st.Lhs {
					if id, ok := l.(*ast.Ident); ok && id.Name == name && i < len(st.Rhs) {
						out = append(out, boundArg{st.Rhs[i], in})
					}
				}
			case *ast.ValueSpec:
				for i, id := range st.Names {
					if id.Name == name && i < len(st.Values) {
						out = append(out, boundArg{st.Values[i], in})
					}
				}
			case *ast.RangeStmt:
				if id, ok := st.Value.(*ast.Ident); ok && id.Name == name {
					if lit, ok := st.X.(*ast.CompositeLit); ok {
						for _, e := range lit.Elts {
							out = append(out, boundArg{e, in})
						}
					}
				}
			}
		})
		if len(out) > 0 {
			return out
		}
	}
	return nil
}

// cmds are the command instances an expression evaluates to.
func (t *cliTree) cmds(e ast.Expr, s *scope, depth int) []cmdInst {
	if depth > 12 {
		return nil
	}
	switch x := e.(type) {
	case *ast.UnaryExpr:
		return t.cmds(x.X, s, depth+1)
	case *ast.ParenExpr:
		return t.cmds(x.X, s, depth+1)
	case *ast.CompositeLit:
		if isCommand(x) {
			return []cmdInst{{x, s}}
		}
	case *ast.Ident:
		var out []cmdInst
		for _, v := range t.values(x.Name, s) {
			out = append(out, t.cmds(v.expr, v.in, depth+1)...)
		}
		return out
	case *ast.CallExpr:
		id, ok := x.Fun.(*ast.Ident)
		if !ok {
			return nil
		}
		bind := func(fields *ast.FieldList, body *ast.BlockStmt, outer *scope) *scope {
			in := &scope{body: body, params: map[string]boundArg{}, outer: outer}
			i := 0
			for _, f := range fields.List {
				for _, n := range f.Names {
					if i < len(x.Args) {
						in.params[n.Name] = boundArg{x.Args[i], s}
					}
					i++
				}
			}
			return in
		}
		var out []cmdInst
		returns := func(in *scope) {
			inspectBody(in.body, func(n ast.Node) {
				if r, ok := n.(*ast.ReturnStmt); ok && len(r.Results) > 0 {
					out = append(out, t.cmds(r.Results[0], in, depth+1)...)
				}
			})
		}
		if fd := t.funcs[id.Name]; fd != nil {
			returns(bind(fd.Type.Params, fd.Body, nil))
			return out
		}
		for _, v := range t.values(id.Name, s) {
			if fl, ok := v.expr.(*ast.FuncLit); ok {
				returns(bind(fl.Type.Params, fl.Body, v.in))
			}
		}
		return out
	}
	return nil
}

// children are the commands AddCommand attaches to c in the scope it was
// built in.
func (t *cliTree) children(c cmdInst) []cmdInst {
	var out []cmdInst
	inspectBody(c.in.body, func(n ast.Node) {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "AddCommand" {
			return
		}
		mine := false
		for _, r := range t.cmds(sel.X, c.in, 0) {
			mine = mine || r.lit == c.lit
		}
		if !mine {
			return
		}
		for _, a := range call.Args {
			out = append(out, t.cmds(a, c.in, 0)...)
		}
	})
	return out
}
