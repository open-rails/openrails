package contractaudit

import (
	"go/ast"
	"go/token"
	"slices"
	"strings"
)

// An internal type can still be public through an alias, an exported field or
// a function signature. Follow only that reachable surface; unrelated internal
// declarations and implementation bodies are not compatibility commitments.
type typeReference struct{ pkg, name string }
type reachableDeclaration struct {
	text string
	refs []typeReference
}

func declaredTypes(fset *token.FileSet, file *ast.File, pkg string, imports map[string]string, public bool) (map[string][]reachableDeclaration, []typeReference, error) {
	types := map[string][]reachableDeclaration{}
	var roots []typeReference
	refs := func(expr ast.Expr) []typeReference { return referencedTypes(expr, pkg, imports) }
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					copy := *s
					copy.Type = exportedShape(s.Type)
					text, err := render(fset, &copy)
					if err != nil {
						return nil, nil, err
					}
					dependencies := refs(&ast.FuncType{TypeParams: copy.TypeParams, Results: &ast.FieldList{List: []*ast.Field{{Type: copy.Type}}}})
					types[s.Name.Name] = append(types[s.Name.Name], reachableDeclaration{"type " + text, dependencies})
					if public && s.Name.IsExported() {
						roots = append(roots, typeReference{pkg, s.Name.Name})
					}
				case *ast.ValueSpec:
					for _, name := range s.Names {
						if public && name.IsExported() {
							roots = append(roots, refs(s.Type)...)
							break
						}
					}
				}
			}
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			signature := *d.Type
			if d.Recv != nil {
				signature.TypeParams = receiverParameters(d.Recv.List[0].Type)
			}
			dependencies := refs(&signature)
			if d.Recv == nil {
				if public {
					roots = append(roots, dependencies...)
				}
				continue
			}
			receiver := receiverName(d.Recv.List[0].Type)
			copy := *d
			copy.Body = nil
			text, err := render(fset, &copy)
			if err != nil {
				return nil, nil, err
			}
			types[receiver] = append(types[receiver], reachableDeclaration{text, dependencies})
			if public && exportedReceiver(d.Recv.List[0].Type) {
				roots = append(roots, typeReference{pkg, receiver})
			}
		}
	}
	return types, roots, nil
}

func receiverName(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.StarExpr:
		return receiverName(e.X)
	case *ast.IndexExpr:
		return receiverName(e.X)
	case *ast.IndexListExpr:
		return receiverName(e.X)
	}
	return ""
}

// Receiver type parameters are local names too; a same-named package type is
// not part of the method's public signature.
func receiverParameters(expr ast.Expr) *ast.FieldList {
	var args []ast.Expr
	switch e := expr.(type) {
	case *ast.StarExpr:
		return receiverParameters(e.X)
	case *ast.IndexExpr:
		args = []ast.Expr{e.Index}
	case *ast.IndexListExpr:
		args = e.Indices
	}
	params := &ast.FieldList{}
	for _, arg := range args {
		if name, ok := arg.(*ast.Ident); ok {
			params.List = append(params.List, &ast.Field{Names: []*ast.Ident{name}, Type: ast.NewIdent("any")})
		}
	}
	return params
}

func exportedShape(expr ast.Expr) ast.Expr {
	shape, ok := expr.(*ast.StructType)
	if !ok {
		return expr
	}
	copy := *shape
	fields := *shape.Fields
	fields.List = nil
	for _, field := range shape.Fields.List {
		item := *field
		item.Names = nil
		for _, name := range field.Names {
			if name.IsExported() {
				item.Names = append(item.Names, name)
			}
		}
		if len(field.Names) == 0 || len(item.Names) > 0 {
			fields.List = append(fields.List, &item)
		}
	}
	copy.Fields = &fields
	return &copy
}

func referencedTypes(expr ast.Expr, pkg string, imports map[string]string) []typeReference {
	var refs []typeReference
	bound := map[string]int{}
	var visit func(ast.Expr)
	fields := func(list *ast.FieldList) {
		if list != nil {
			for _, field := range list.List {
				visit(field.Type)
			}
		}
	}
	visit = func(expr ast.Expr) {
		switch e := expr.(type) {
		case *ast.Ident:
			if bound[e.Name] == 0 {
				refs = append(refs, typeReference{pkg, e.Name})
			}
		case *ast.SelectorExpr:
			if qualifier, ok := e.X.(*ast.Ident); ok {
				if imported := imports[qualifier.Name]; imported == Module {
					refs = append(refs, typeReference{".", e.Sel.Name})
				} else if strings.HasPrefix(imported, Module+"/") {
					refs = append(refs, typeReference{strings.TrimPrefix(imported, Module+"/"), e.Sel.Name})
				}
			}
		case *ast.StarExpr:
			visit(e.X)
		case *ast.ArrayType:
			visit(e.Elt)
		case *ast.MapType:
			visit(e.Key)
			visit(e.Value)
		case *ast.ChanType:
			visit(e.Value)
		case *ast.Ellipsis:
			visit(e.Elt)
		case *ast.ParenExpr:
			visit(e.X)
		case *ast.IndexExpr:
			visit(e.X)
			visit(e.Index)
		case *ast.IndexListExpr:
			visit(e.X)
			for _, item := range e.Indices {
				visit(item)
			}
		case *ast.UnaryExpr:
			visit(e.X)
		case *ast.BinaryExpr:
			visit(e.X)
			visit(e.Y)
		case *ast.FuncType:
			if e.TypeParams != nil {
				for _, parameter := range e.TypeParams.List {
					for _, name := range parameter.Names {
						bound[name.Name]++
					}
				}
			}
			fields(e.TypeParams)
			fields(e.Params)
			fields(e.Results)
			if e.TypeParams != nil {
				for _, parameter := range e.TypeParams.List {
					for _, name := range parameter.Names {
						bound[name.Name]--
					}
				}
			}
		case *ast.InterfaceType:
			fields(e.Methods)
		case *ast.StructType:
			fields(exportedShape(e).(*ast.StructType).Fields)
		}
	}
	visit(expr)
	return refs
}

func captureReachableTypes(out *Snapshot, files map[string]fileFacts) {
	type located struct {
		file        string
		declaration reachableDeclaration
	}
	decls := map[typeReference][]located{}
	var pending []typeReference
	for name, facts := range files {
		pending = append(pending, facts.roots...)
		for symbol, list := range facts.types {
			ref := typeReference{facts.pkg, symbol}
			for _, declaration := range list {
				decls[ref] = append(decls[ref], located{name, declaration})
			}
		}
	}
	seen := map[typeReference]bool{}
	for len(pending) > 0 {
		ref := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if seen[ref] {
			continue
		}
		seen[ref] = true
		for _, decl := range decls[ref] {
			pending = append(pending, decl.declaration.refs...)
			facts := files[decl.file]
			// Public declarations are already present; reachable internal
			// declarations join the same API section under their real package.
			if !slices.Contains(out.API[ref.pkg], decl.declaration.text) {
				out.API[ref.pkg] = append(out.API[ref.pkg], decl.declaration.text)
			}
			if len(facts.imports) > 0 {
				out.Imports[decl.file] = facts.imports
			}
		}
	}
}
