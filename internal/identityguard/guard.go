// Package identityguard provides syntax checks that keep filesystem identity
// construction behind the store capture primitives. It resolves ordinary and
// generic container types declared in the inspected file and detects direct
// FileIdentity conversions; named types declared in another file remain
// outside this per-file syntax check. This package is test support only.
package identityguard

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"
)

type Kind string

const (
	InodeRead               Kind = "inode"
	HandleAssignment        Kind = "handle"
	IdentityFieldAssignment Kind = "identity-field"
	IdentityLiteral         Kind = "literal"
)

type Finding struct {
	Kind Kind
	Pos  token.Pos
}

func FirstFinding(file *ast.File) Kind {
	findings := Findings(file)
	if len(findings) == 0 {
		return ""
	}
	return findings[0].Kind
}

// Findings uses the parser's declaration bindings to resolve local types and
// shadowing. Callers must parse without parser.SkipObjectResolution.
func Findings(file *ast.File) []Finding {
	var findings []Finding
	var parents []ast.Node
	namedTypes := declaredTypes(file)
	var scopes []map[string]*ast.TypeSpec
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			parents = parents[:len(parents)-1]
			namedTypes = scopes[len(scopes)-1]
			scopes = scopes[:len(scopes)-1]
			return true
		}
		scopes = append(scopes, namedTypes)
		var parameters *ast.FieldList
		switch declaration := node.(type) {
		case *ast.FuncDecl:
			parameters = declaration.Type.TypeParams
		case *ast.TypeSpec:
			parameters = declaration.TypeParams
		}
		if parameters != nil {
			scoped := make(map[string]*ast.TypeSpec, len(namedTypes)+len(parameters.List))
			for name, spec := range namedTypes {
				scoped[name] = spec
			}
			for _, field := range parameters.List {
				for _, name := range field.Names {
					scoped[name.Name] = &ast.TypeSpec{Name: name, Assign: name.Pos(), Type: field.Type}
				}
			}
			namedTypes = scoped
		}
		kind := directFinding(node, namedTypes)
		if literal, ok := node.(*ast.CompositeLit); ok && kind == "" && len(literal.Elts) > 0 &&
			literal.Type == nil && fileIdentityTypeWithNamed(elidedLiteralType(literal, parents, namedTypes), namedTypes) {
			kind = IdentityLiteral
		}
		if kind != "" {
			findings = append(findings, Finding{Kind: kind, Pos: node.Pos()})
		}
		parents = append(parents, node)
		return true
	})
	return findings
}

func InsideAllowedFunction(file *ast.File, position token.Pos, allowed map[string]bool) bool {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Recv == nil && allowed[function.Name.Name] && function.Pos() <= position && position <= function.End() {
			return true
		}
	}
	return false
}

func directFinding(node ast.Node, namedTypes map[string]*ast.TypeSpec) Kind {
	switch node := node.(type) {
	case *ast.SelectorExpr:
		if node.Sel.Name == "Ino" || node.Sel.Name == "Dev" {
			return InodeRead
		}
	case *ast.AssignStmt:
		for _, expression := range node.Lhs {
			if kind := identityFieldFinding(expression); kind != "" {
				return kind
			}
		}
	case *ast.RangeStmt:
		if node.Tok == token.ASSIGN {
			for _, expression := range []ast.Expr{node.Key, node.Value} {
				if kind := identityFieldFinding(expression); kind != "" {
					return kind
				}
			}
		}
	case *ast.IncDecStmt:
		return identityFieldFinding(node.X)
	case *ast.UnaryExpr:
		if node.Op == token.AND {
			return identityFieldFinding(node.X)
		}
	case *ast.CompositeLit:
		if len(node.Elts) > 0 && fileIdentityTypeWithNamed(node.Type, namedTypes) {
			return IdentityLiteral
		}
	case *ast.CallExpr:
		if len(node.Args) > 0 && fileIdentityTypeWithNamed(node.Fun, namedTypes) {
			return IdentityLiteral
		}
	}
	return ""
}

func identityFieldFinding(expression ast.Expr) Kind {
	for {
		parens, ok := expression.(*ast.ParenExpr)
		if !ok {
			break
		}
		expression = parens.X
	}
	selector, ok := expression.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	switch selector.Sel.Name {
	case "Handle":
		return HandleAssignment
	case "Device", "Inode":
		return IdentityFieldAssignment
	default:
		return ""
	}
}

func elidedLiteralType(literal *ast.CompositeLit, parents []ast.Node, namedTypes map[string]*ast.TypeSpec) ast.Expr {
	if len(parents) == 0 {
		return nil
	}
	parentIndex := len(parents) - 1
	keyValue, keyed := parents[parentIndex].(*ast.KeyValueExpr)
	if keyed {
		parentIndex--
	}
	if parentIndex < 0 {
		return nil
	}
	outer, ok := parents[parentIndex].(*ast.CompositeLit)
	if !ok {
		return nil
	}
	outerType := compositeLiteralType(outer, parents[:parentIndex], namedTypes)
	if keyed {
		if mapping, ok := resolvedType(outerType, namedTypes, nil).(*ast.MapType); ok {
			if keyValue.Key == literal {
				return mapping.Key
			}
			return mapping.Value
		}
	}
	return containerElementType(outerType, namedTypes)
}

func compositeLiteralType(literal *ast.CompositeLit, parents []ast.Node, namedTypes map[string]*ast.TypeSpec) ast.Expr {
	if literal.Type != nil {
		return literal.Type
	}
	return elidedLiteralType(literal, parents, namedTypes)
}

func containerElementType(expression ast.Expr, namedTypes map[string]*ast.TypeSpec) ast.Expr {
	switch expression := resolvedType(expression, namedTypes, nil).(type) {
	case *ast.ArrayType:
		return expression.Elt
	case *ast.MapType:
		return expression.Value
	default:
		return nil
	}
}

func fileIdentityTypeWithNamed(expression ast.Expr, namedTypes map[string]*ast.TypeSpec) bool {
	return fileIdentityTypeSeen(expression, namedTypes, make(map[identityTypeVisit]bool))
}

type identityTypeVisit struct {
	expression string
	bindings   string
}

func fileIdentityTypeSeen(expression ast.Expr, namedTypes map[string]*ast.TypeSpec, seen map[identityTypeVisit]bool) bool {
	if expression == nil {
		return false
	}
	// Generic substitution allocates fresh AST nodes, so pointer identity cannot
	// detect recursive types. Include arguments in the key so Alias[Alias[T]]
	// can still reduce to T without being mistaken for a recursive definition.
	// Declaration bindings distinguish shadowed names in different scopes.
	key := typeExpressionVisit(expression)
	if seen[key] {
		return false
	}
	seen[key] = true
	switch expression := expression.(type) {
	case *ast.Ident:
		if expression.Name == "FileIdentity" {
			return true
		}
		if typeSpec := boundType(expression, namedTypes); typeSpec != nil && typeSpec.Assign != token.NoPos {
			return fileIdentityTypeSeen(typeSpec.Type, namedTypes, seen)
		}
		resolved := resolvedType(expression, namedTypes, nil)
		return resolved != expression && fileIdentityTypeSeen(resolved, namedTypes, seen)
	case *ast.SelectorExpr:
		return expression.Sel.Name == "FileIdentity"
	case *ast.StarExpr:
		return fileIdentityTypeSeen(expression.X, namedTypes, seen)
	case *ast.ParenExpr:
		return fileIdentityTypeSeen(expression.X, namedTypes, seen)
	case *ast.IndexExpr, *ast.IndexListExpr:
		resolved := resolvedType(expression, namedTypes, nil)
		return resolved != expression && fileIdentityTypeSeen(resolved, namedTypes, seen)
	case *ast.InterfaceType:
		if expression.Methods != nil {
			for _, term := range expression.Methods.List {
				if len(term.Names) == 0 && fileIdentityTypeSeen(term.Type, namedTypes, seen) {
					return true
				}
			}
		}
		return false
	default:
		return false
	}
}

func declaredTypes(file *ast.File) map[string]*ast.TypeSpec {
	types := make(map[string]*ast.TypeSpec)
	for _, declaration := range file.Decls {
		generic, ok := declaration.(*ast.GenDecl)
		if !ok || generic.Tok != token.TYPE {
			continue
		}
		for _, specification := range generic.Specs {
			typeSpec, ok := specification.(*ast.TypeSpec)
			if ok {
				types[typeSpec.Name.Name] = typeSpec
			}
		}
	}
	return types
}

func resolvedType(expression ast.Expr, namedTypes map[string]*ast.TypeSpec, seen map[identityTypeVisit]bool) ast.Expr {
	if namedTypes == nil {
		return expression
	}
	switch expression := expression.(type) {
	case *ast.Ident:
		// Preserve the identity name when substituting generic arguments, even
		// when its underlying struct is declared in the inspected file.
		if expression.Name == "FileIdentity" {
			return expression
		}
		typeSpec := boundType(expression, namedTypes)
		if typeSpec == nil {
			return expression
		}
		if seen == nil {
			seen = make(map[identityTypeVisit]bool)
		}
		key := typeExpressionVisit(expression)
		if seen[key] {
			return expression
		}
		seen[key] = true
		return resolvedType(typeSpec.Type, namedTypes, seen)
	case *ast.IndexExpr:
		return resolvedInstantiatedType(expression.X, []ast.Expr{expression.Index}, namedTypes, seen)
	case *ast.IndexListExpr:
		return resolvedInstantiatedType(expression.X, expression.Indices, namedTypes, seen)
	default:
		return expression
	}
}

func resolvedInstantiatedType(expression ast.Expr, arguments []ast.Expr, namedTypes map[string]*ast.TypeSpec, seen map[identityTypeVisit]bool) ast.Expr {
	identifier, ok := expression.(*ast.Ident)
	if !ok {
		return expression
	}
	typeSpec := boundType(identifier, namedTypes)
	if typeSpec == nil || typeSpec.TypeParams == nil {
		return expression
	}
	bindings := make(map[string]ast.Expr)
	argument := 0
	for _, field := range typeSpec.TypeParams.List {
		for _, name := range field.Names {
			if argument >= len(arguments) {
				return expression
			}
			bindings[name.Name] = arguments[argument]
			argument++
		}
	}
	if argument != len(arguments) {
		return expression
	}
	return resolvedType(substituteTypeParameters(typeSpec.Type, bindings), namedTypes, seen)
}

func substituteTypeParameters(expression ast.Expr, bindings map[string]ast.Expr) ast.Expr {
	switch expression := expression.(type) {
	case *ast.Ident:
		if replacement := bindings[expression.Name]; replacement != nil {
			return replacement
		}
		return expression
	case *ast.ArrayType:
		return &ast.ArrayType{Lbrack: expression.Lbrack, Len: expression.Len, Elt: substituteTypeParameters(expression.Elt, bindings)}
	case *ast.MapType:
		return &ast.MapType{Map: expression.Map, Key: substituteTypeParameters(expression.Key, bindings), Value: substituteTypeParameters(expression.Value, bindings)}
	case *ast.StarExpr:
		return &ast.StarExpr{Star: expression.Star, X: substituteTypeParameters(expression.X, bindings)}
	case *ast.IndexExpr:
		return &ast.IndexExpr{X: substituteTypeParameters(expression.X, bindings), Lbrack: expression.Lbrack, Index: substituteTypeParameters(expression.Index, bindings), Rbrack: expression.Rbrack}
	case *ast.IndexListExpr:
		indices := make([]ast.Expr, len(expression.Indices))
		for i, index := range expression.Indices {
			indices[i] = substituteTypeParameters(index, bindings)
		}
		return &ast.IndexListExpr{X: substituteTypeParameters(expression.X, bindings), Lbrack: expression.Lbrack, Indices: indices, Rbrack: expression.Rbrack}
	default:
		return expression
	}
}

// A reference keeps the declaration it originally bound to, even if a later
// local declaration shadows that name. Synthetic type-parameter constraints
// and unresolved package names still use the traversal's named-type map.
func boundType(identifier *ast.Ident, namedTypes map[string]*ast.TypeSpec) *ast.TypeSpec {
	if identifier.Obj != nil {
		if spec, ok := identifier.Obj.Decl.(*ast.TypeSpec); ok {
			return spec
		}
		if identifier.Obj.Kind != ast.Typ {
			return nil
		}
	}
	return namedTypes[identifier.Name]
}

// Generic arguments can have the same spelling while binding to different
// declarations. Include every identifier binding, not just the outer type's,
// when deciding whether substitution has returned to an earlier expression.
func typeExpressionVisit(expression ast.Expr) identityTypeVisit {
	var bindings strings.Builder
	ast.Inspect(expression, func(node ast.Node) bool {
		if identifier, ok := node.(*ast.Ident); ok {
			fmt.Fprintf(&bindings, "/%p", identifier.Obj)
		}
		return true
	})
	return identityTypeVisit{expression: types.ExprString(expression), bindings: bindings.String()}
}
