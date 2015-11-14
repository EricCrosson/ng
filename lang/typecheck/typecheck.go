// Copyright 2015 The Numgrad Authors. All rights reserved.
// See the LICENSE file for rights to use this source code.

// Package typecheck is a Numengrad type checker.
package typecheck

import (
	"bytes"
	"fmt"
	"go/constant"
	"go/importer"
	gotoken "go/token"
	gotypes "go/types"
	"math/big"

	"numgrad.io/lang/expr"
	"numgrad.io/lang/stmt"
	"numgrad.io/lang/tipe"
	"numgrad.io/lang/token"
)

type Checker struct {
	// TODO: we could put these on our AST. Should we?
	Types   map[expr.Expr]tipe.Type
	Defs    map[*expr.Ident]*Obj
	Values  map[expr.Expr]constant.Value
	NumSpec map[expr.Expr]tipe.Basic // *tipe.Call, *tipe.CompLiteral -> numeric basic type
	Errs    []error

	cur *Scope
}

func New() *Checker {
	return &Checker{
		Types:  make(map[expr.Expr]tipe.Type),
		Defs:   make(map[*expr.Ident]*Obj),
		Values: make(map[expr.Expr]constant.Value),
		cur: &Scope{
			Parent: base,
			Objs:   make(map[string]*Obj),
		},
	}
}

type partialMode int

const (
	modeInvalid partialMode = iota
	modeVoid
	modeConst
	modeVar
	modeBuiltin
	modeTypeExpr
	modeFunc
)

type partial struct {
	mode partialMode
	typ  tipe.Type
	val  constant.Value
	expr expr.Expr
}

func (c *Checker) stmt(s stmt.Stmt, retType *tipe.Tuple) {
	switch s := s.(type) {
	case *stmt.Assign:
		if len(s.Left) != len(s.Right) {
			panic("TODO artity mismatch, i.e. x, y := f()")
		}
		var partials []partial
		for _, rhs := range s.Right {
			partials = append(partials, c.expr(rhs))
		}
		if s.Decl {
			for i, lhs := range s.Left {
				p := partials[i]
				if isUntyped(p.typ) {
					c.constrainUntyped(&p, defaultType(p.typ))
				}
				obj := &Obj{
					Kind: ObjVar,
					Type: p.typ,
				}
				c.Defs[lhs.(*expr.Ident)] = obj
				c.cur.Objs[lhs.(*expr.Ident).Name] = obj
			}
		} else {
			for i, lhs := range s.Left {
				p := partials[i]
				lhsP := c.expr(lhs)
				c.assign(&p, lhsP.typ)
			}
		}

	case *stmt.Simple:
		p := c.expr(s.Expr)
		if p.mode == modeFunc {
			fn := p.expr.(*expr.FuncLiteral)
			if fn.Name != "" {
				obj := &Obj{
					Kind: ObjVar,
					Type: p.typ,
				}
				// TODO: c.Defs?
				c.cur.Objs[fn.Name] = obj
			}
		}

	case *stmt.Block:
		c.pushScope()
		defer c.popScope()
		for _, s := range s.Stmts {
			c.stmt(s, retType)
		}

	case *stmt.If:
		if s.Init != nil {
			c.pushScope()
			defer c.popScope()
			c.stmt(s.Init, retType)
		}
		c.expr(s.Cond)
		c.stmt(s.Body, retType)
		if s.Else != nil {
			c.stmt(s.Else, retType)
		}

	case *stmt.For:
		if s.Init != nil {
			c.pushScope()
			defer c.popScope()
			c.stmt(s.Init, retType)
		}
		c.expr(s.Cond)
		if s.Post != nil {
			c.stmt(s.Post, retType)
		}
		c.stmt(s.Body, retType)

	case *stmt.ClassDecl:
		var usesNum bool
		var resolved bool
		for i, f := range s.Type.Fields {
			s.Type.Fields[i], resolved = c.resolve(f)
			usesNum = usesNum || tipe.UsesNum(s.Type.Fields[i])
			if !resolved {
				return
			}
		}
		for i, f := range s.Type.Methods {
			s.Type.Methods[i], resolved = c.resolve(f)
			usesNum = usesNum || tipe.UsesNum(s.Type.Methods[i])
			if !resolved {
				return
			}
		}

		for _, m := range s.Methods {
			c.pushScope()
			if m.ReceiverName != "" {
				obj := &Obj{
					Kind: ObjVar,
					Type: s.Type,
				}
				c.cur.Objs[m.ReceiverName] = obj
			}
			c.expr(m)
			// TODO: uses num inside a method
			c.popScope()
		}

		if usesNum {
			s.Type.Spec.Num = tipe.Num
		}

		obj := &Obj{
			Kind: ObjType,
			Type: s.Type,
			Decl: s,
		}
		c.cur.Objs[s.Name] = obj

	case *stmt.Return:
		if retType == nil || len(s.Exprs) > len(retType.Elems) {
			c.errorf("too many arguments to return")
		}
		var partials []partial
		for i, e := range s.Exprs {
			partials = append(partials, c.expr(e))
			c.constrainUntyped(&partials[i], retType.Elems[i])
		}
		for _, p := range partials {
			if p.mode == modeInvalid {
				return
			}
		}
		want := retType.Elems
		if len(want) == 0 && len(partials) == 0 {
			return
		}
		var got []tipe.Type
		if tup, ok := partials[0].typ.(*tipe.Tuple); ok {
			if len(partials) != 1 {
				c.errorf("multi-value %s in single-value context", partials[0])
				return
			}
			got = tup.Elems
		} else {
			for _, p := range partials {
				if _, ok := p.typ.(*tipe.Tuple); ok {
					c.errorf("multi-value %s in single-value context", partials[0])
					return
				}
				got = append(got, p.typ)
			}
		}
		if len(got) > len(want) {
			c.errorf("too many arguments to return")
			return
		}
		if len(got) < len(want) {
			c.errorf("too few arguments to return")
			return
		}

		for i := range want {
			if !tipe.Equal(got[i], want[i]) {
				c.errorf("cannot use %s as %s (%T) in return argument", got[i], want[i])
				return
			}
		}

	case *stmt.Import:
		c.checkImport(s)

	default:
		panic(fmt.Sprintf("typecheck: unknown stmt %T", s))
	}
}

func (c *Checker) goPackage(gopkg *gotypes.Package) *tipe.Go {
	names := gopkg.Scope().Names()

	pkg := &tipe.Package{
		Exports: make(map[string]tipe.Type),
	}
	for _, name := range names {
		pkg.Exports[name] = nil // TODO
	}

	return &tipe.Go{
		GoPkg:      gopkg,
		Equivalent: pkg,
	}
}

func (c *Checker) checkImport(s *stmt.Import) {
	if s.FromGo {
		pkg, err := importer.Default().Import(s.Path)
		if err != nil {
			c.errorf("importing go package: %v", err)
			return
		}
		if s.Name == "" {
			s.Name = pkg.Name()
		}
		obj := &Obj{
			Kind: ObjVar, // TODO: new ObjKind
			Type: c.goPackage(pkg),
			// TODO Decl?
		}
		fmt.Printf("typechecking import %s\n", s.Name)
		c.cur.Objs[s.Name] = obj
	} else {
		c.errorf("TODO import of non-Go package")
	}
}

func (c *Checker) expr(e expr.Expr) (p partial) {
	// TODO more mode adjustment
	p = c.exprPartial(e)
	if p.mode == modeConst {
		c.Values[p.expr] = p.val
		c.Types[p.expr] = p.typ
	}
	return p
}

func (c *Checker) resolve(t tipe.Type) (ret tipe.Type, resolved bool) {
	switch t := t.(type) {
	case *tipe.Table:
		t.Type, resolved = c.resolve(t.Type)
		return t, resolved
	case *tipe.Unresolved:
		if t.Package != "" {
			// TODO look up package in scope, extract type from it.
			panic("TODO type in package")
		}
		obj := c.cur.LookupRec(t.Name)
		if obj == nil {
			c.errorf("type %s not declared", t.Name)
			return t, false
		}
		if obj.Kind != ObjType {
			c.errorf("symbol %s is not a type", t.Name)
			return t, false
		}
		return obj.Type, true
		// TODO many more types
	default:
		return t, true
	}
}

func (c *Checker) exprPartial(e expr.Expr) (p partial) {
	//fmt.Printf("exprPartial(%s)\n", e.Sexp())
	p.expr = e
	switch e := e.(type) {
	case *expr.Ident:
		obj := c.cur.LookupRec(e.Name)
		if obj == nil {
			p.mode = modeInvalid
			c.errorf("undeclared identifier: %s", e.Name)
			return p
		}
		c.Defs[e] = obj // TODO Defs is more than definitions? rename?
		// TODO: is a partial's mode just an ObjKind?
		// not every partial has an Obj, but we could reuse the type.
		switch obj.Kind {
		case ObjVar:
			p.mode = modeVar
		case ObjType:
			p.mode = modeTypeExpr
		}
		p.typ = obj.Type
		return p
	case *expr.BasicLiteral:
		// TODO: use constant.Value in BasicLiteral directly.
		switch v := e.Value.(type) {
		case *big.Int:
			p.mode = modeConst
			p.typ = tipe.UntypedInteger
			p.val = constant.MakeFromLiteral(v.String(), gotoken.INT, 0)
		case *big.Float:
			p.mode = modeConst
			p.typ = tipe.UntypedFloat
			p.val = constant.MakeFromLiteral(v.String(), gotoken.FLOAT, 0)
		case string:
			p.mode = modeVar
			p.typ = tipe.String
		}
		return p
	case *expr.FuncLiteral:
		c.pushScope()
		defer c.popScope()
		if e.Type.Params != nil {
			for i, t := range e.Type.Params.Elems {
				e.Type.Params.Elems[i], _ = c.resolve(t)
				obj := &Obj{
					Kind: ObjVar,
					Type: t,
				}
				c.cur.Objs[e.ParamNames[i]] = obj
			}
		}
		if e.Type.Results != nil {
			for i, t := range e.Type.Results.Elems {
				e.Type.Results.Elems[i], _ = c.resolve(t)
			}
		}
		p.typ = e.Type
		p.mode = modeFunc
		c.stmt(e.Body.(*stmt.Block), e.Type.Results)
		return p
	case *expr.CompLiteral:
		p.mode = modeVar
		className := fmt.Sprintf("%s", e.Type)
		if t, resolved := c.resolve(e.Type); resolved {
			e.Type = t
			p.typ = t
		} else {
			p.mode = modeInvalid
			return p
		}
		class, isClass := e.Type.(*tipe.Class)
		if !isClass {
			c.errorf("cannot construct type %s with a composite literal", e.Type)
			p.mode = modeInvalid
			return p
		}
		elemsp := make([]partial, len(e.Elements))
		for i, elem := range e.Elements {
			elemsp[i] = c.expr(elem)
			if elemsp[i].mode == modeInvalid {
				p.mode = modeInvalid
				return p
			}
		}
		if len(e.Names) == 0 {
			if len(e.Elements) != len(class.Fields) {
				c.errorf("wrong number of elements, %d, when %s expects %d", len(e.Elements), className, len(class.Fields))
				p.mode = modeInvalid
				return p
			}
			for i, ft := range class.Fields {
				c.assign(&elemsp[i], ft)
				if elemsp[i].mode == modeInvalid {
					p.mode = modeInvalid
					return p
				}
			}
		} else {
			panic("TODO: named CompLiteral")
		}
		if p.mode != modeInvalid {
			p.expr = e
		}
		return p

	case *expr.TableLiteral:
		p.mode = modeVar

		var elemType tipe.Type
		if t, resolved := c.resolve(e.Type); resolved {
			t, isTable := t.(*tipe.Table)
			if !isTable {
				c.errorf("type %s is not a table", t)
				p.mode = modeInvalid
				return p
			}
			elemType = t.Type
			e.Type = t
			p.typ = t
		} else {
			p.mode = modeInvalid
			return p
		}

		for _, colNameExpr := range e.ColNames {
			colp := c.expr(colNameExpr)
			c.assign(&colp, tipe.String)
			if colp.mode == modeInvalid {
				p.mode = modeInvalid
				return p
			}
		}
		if len(e.Rows) == 0 {
			return p
		}

		// Check everyone agrees on the width.
		w := len(e.Rows[0])
		if len(e.ColNames) > 0 && len(e.ColNames) != w {
			c.errorf("table literal has %d column names but a width of %d", len(e.ColNames), w)
			p.mode = modeInvalid
			return p
		}
		for _, r := range e.Rows {
			if len(r) != w {
				c.errorf("table literal has rows of different lengths (%d and %d)", w, len(r))
				p.mode = modeInvalid
				return p
			}
			for _, elem := range r {
				elemp := c.expr(elem)
				c.assign(&elemp, elemType)
				if elemp.mode == modeInvalid {
					p.mode = modeInvalid
					return p
				}
			}
		}
		return p

	case *expr.Unary:
		switch e.Op {
		case token.LeftParen, token.Not, token.Sub:
			return c.expr(e.Expr)
		}
	case *expr.Binary:
		left := c.expr(e.Left)
		right := c.expr(e.Right)
		c.constrainUntyped(&left, right.typ)
		c.constrainUntyped(&right, left.typ)
		if left.mode == modeInvalid {
			return left
		}
		if right.mode == modeInvalid {
			return right
		}
		left.expr = e
		// TODO check for division by zero
		// TODO check for comparison
		if left.mode == modeConst && right.mode == modeConst {
			left.val = constant.BinaryOp(left.val, convGoOp(e.Op), right.val)
			// TODO check rounding
		}

		return left
	case *expr.Call:
		p := c.expr(e.Func)
		switch p.mode {
		case modeInvalid:
			return p
		case modeVar, modeFunc:
			// function call
			funct := p.typ.(*tipe.Func)
			var params, results []tipe.Type
			if funct.Params != nil {
				params = funct.Params.Elems
			}
			if funct.Results != nil {
				results = funct.Results.Elems
			}

			switch len(results) {
			case 0:
				p.typ = nil
			case 1:
				p.typ = results[0]
			default:
				p.typ = funct.Results
			}

			if len(e.Args) != len(params) {
				p.mode = modeInvalid
				c.errorf("wrong number of arguments (%d) to function %s", len(e.Args), funct)
			}

			if p.mode != modeInvalid {
				var argsp []partial
				for i, arg := range e.Args {
					t := params[i]
					argp := c.expr(arg)
					c.convert(&argp, t)
					if argp.mode == modeInvalid {
						p.mode = modeInvalid
						c.errorf("cannot use type %s as type %s in argument to function", argp.typ, t)
						break
					}
					argsp = append(argsp, argp)
				}
			}
			if p.mode == modeInvalid {
				return p
			}
			p.mode = modeVar
			p.expr = e
			return p
		case modeTypeExpr:
			// type conversion
			if len(e.Args) == 0 {
				p.mode = modeInvalid
				c.errorf("type conversion to %s is missing an argument", p.typ)
				return p
			} else if len(e.Args) != 1 {
				p.mode = modeInvalid
				c.errorf("type conversion to %s has too many arguments", p.typ)
				return p
			}
			t := p.typ
			p = c.expr(e.Args[0])
			if p.mode == modeInvalid {
				return p
			}
			c.convert(&p, t)
			p.expr = e
			return p
		default:
			panic(fmt.Sprintf("unreachable, unknown call mode: %v", p.mode))
		}
	case *expr.Selector:
		left := c.expr(e.Left)
		if left.mode == modeInvalid {
			return left
		}
		cls, ok := left.typ.(*tipe.Class)
		if !ok {
			p.mode = modeInvalid
			c.errorf("%s undefined (type %s is not a class)", e, cls)
			return p
		}
		right := e.Right.Name
		for i, name := range cls.FieldNames {
			if name == right {
				p.mode = modeVar
				p.typ = cls.Fields[i]
				return
			}
		}
		for i, name := range cls.MethodNames {
			if name == right {
				p.mode = modeVar
				p.typ = cls.Methods[i]
				return
			}
		}
		p.mode = modeInvalid
		c.errorf("%s undefined (type %s has no field or method %s)", e, cls, right)
		return p
	case *expr.Shell:
		p.mode = modeVoid
		return p
	}
	panic(fmt.Sprintf("expr TODO: %T", e))
}

func (c *Checker) assign(p *partial, t tipe.Type) {
	if p.mode == modeInvalid {
		return
	}
	if isUntyped(p.typ) {
		c.constrainUntyped(p, t)
		return
	}
	if !tipe.Equal(p.typ, t) { // TODO interfaces, etc
		c.errorf("cannot assign %s to %s", p.typ, t)
		p.mode = modeInvalid
	}
}

func (c *Checker) convert(p *partial, t tipe.Type) {
	fmt.Printf("Checker.convert(p=%#+v, t=%s)\n", p, t)
	_, tIsConst := t.(tipe.Basic)
	if p.mode == modeConst && tIsConst {
		// TODO or integer -> string conversion
		fmt.Printf("convert round p.typ=%s, p.val=%s, t=%s\n", p.typ, p.val, t)
		if round(p.val, t.(tipe.Basic)) == nil {
			// p.val does not fit in t
			c.errorf("constant %s does not fit in %s", p.val, t)
			p.mode = modeInvalid
			return
		}
	}

	if !convertible(p.typ, t) {
		// TODO p is assignable to t, lots of possibilities
		// (interface satisfaction, etc)
		c.errorf("cannot use %s as %s", p.typ, t)
		p.mode = modeInvalid
		return
	}

	if isUntyped(p.typ) {
		c.constrainUntyped(p, t)
	} else {
		p.typ = t
	}
}

func convertible(dst, src tipe.Type) bool {
	if dst == src {
		return true
	}
	// TODO several other forms of "identical" types,
	// e.g. maps where keys and value are identical,

	// numerics can be converted to one another
	if tipe.IsNumeric(dst) && tipe.IsNumeric(src) {
		return true
	}

	return false
}

func (c *Checker) constrainUntyped(p *partial, t tipe.Type) {
	if p.mode == modeInvalid || isTyped(p.typ) || t == tipe.Invalid {
		return
	}

	// catch invalid constraints
	if isUntyped(t) {
		switch {
		case t == tipe.UntypedFloat && p.typ == tipe.UntypedInteger:
			// promote untyped int to float
		case t == tipe.UntypedComplex && (p.typ == tipe.UntypedInteger || p.typ == tipe.UntypedFloat):
			// promote untyped int or float to complex
		case t == tipe.Num && (p.typ == tipe.UntypedInteger || p.typ == tipe.UntypedFloat):
			// promote untyped int or float to num type parameter
		case t != p.typ:
			c.errorf("cannot convert %s to %s", p.typ, t)
		}
	} else {
		switch t := t.(type) {
		case tipe.Basic:
			switch p.mode {
			case modeConst:
				p.val = round(p.val, t)
				if p.val == nil {
					c.errorf("cannot convert const %s to %s", p.typ, t)
					// TODO more details about why
				}
			case modeVar:
				panic("TODO coerce var to basic")
			}
		}
	}

	p.typ = t
	c.constrainExprType(p.expr, p.typ)
}

// constrainExprType descends an expression constraining the type.
func (c *Checker) constrainExprType(e expr.Expr, t tipe.Type) {
	oldt := c.Types[e]
	if oldt == t {
		return
	}
	c.Types[e] = t

	switch e := e.(type) {
	case *expr.Bad, *expr.FuncLiteral: // TODO etc
		return
	case *expr.Binary:
		if c.Values[e] != nil {
			break
		}
		switch e.Op {
		case token.Equal, token.NotEqual,
			token.Less, token.LessEqual,
			token.Greater, token.GreaterEqual:
			// comparisons generate their own bool type
			return
		}
		c.constrainExprType(e.Left, t)
		c.constrainExprType(e.Right, t)
	}

	c.Types[e] = t
}

func (c *Checker) errorf(format string, args ...interface{}) {
	err := fmt.Errorf(format, args...)
	c.Errs = append(c.Errs, err)
}

func (c *Checker) pushScope() {
	c.cur = &Scope{
		Parent: c.cur,
		Objs:   make(map[string]*Obj),
	}
}
func (c *Checker) popScope() {
	c.cur = c.cur.Parent
}

func convGoOp(op token.Token) gotoken.Token {
	switch op {
	case token.Add:
		return gotoken.ADD
	case token.Sub:
		return gotoken.SUB
	case token.Mul:
		return gotoken.MUL
	case token.Div:
		return gotoken.QUO // TODO: QUO_ASSIGN for int div
	case token.Rem:
		return gotoken.REM
	case token.Pow:
		panic("TODO token.Pow")
		return gotoken.REM
	default:
		panic(fmt.Sprintf("typecheck: bad op: %s", op))
	}
}

func round(v constant.Value, t tipe.Basic) constant.Value {
	switch v.Kind() {
	case constant.Unknown:
		return v
	case constant.Bool:
		if t == tipe.Bool || t == tipe.UntypedBool {
			return v
		} else {
			return nil
		}
	case constant.Int:
		switch t {
		case tipe.Integer, tipe.UntypedInteger:
			return v
		case tipe.Float, tipe.UntypedFloat, tipe.UntypedComplex:
			return v
		case tipe.Num:
			return v
		case tipe.Int64:
			if _, ok := constant.Int64Val(v); ok {
				return v
			} else {
				return nil
			}
		case tipe.Float32:
			r, _ := constant.Float32Val(v)
			return constant.MakeFloat64(float64(r))
		case tipe.Float64:
			r, _ := constant.Float64Val(v)
			return constant.MakeFloat64(float64(r))
		}
	case constant.Float:
		switch t {
		case tipe.Float, tipe.UntypedFloat, tipe.UntypedComplex:
			return v
		case tipe.Float32:
			r, _ := constant.Float32Val(v)
			return constant.MakeFloat64(float64(r))
		case tipe.Float64:
			r, _ := constant.Float64Val(v)
			return constant.MakeFloat64(float64(r))
		case tipe.Num:
			return v
		}
	}
	// TODO many more comparisons
	return nil
}

func (c *Checker) Add(s stmt.Stmt) {
	c.stmt(s, nil)
}

func (c *Checker) Lookup(name string) *Obj {
	return c.cur.LookupRec(name)
}

func (c *Checker) String() string {
	buf := new(bytes.Buffer)
	buf.WriteString("typecheck.Checker{\n")
	buf.WriteString("\tTypes: map[expr.Expr]tipe.Type{\n")
	for k, v := range c.Types {
		fmt.Fprintf(buf, "\t\t(%p)%s: %s\n", k, k.Sexp(), v.Sexp())
	}
	buf.WriteString("\t},\n")
	buf.WriteString("\tDefs: map[*expr.Ident]*Obj{\n")
	for k, v := range c.Defs {
		t := "niltype"
		if v.Type != nil {
			t = v.Type.Sexp()
		}
		fmt.Fprintf(buf, "\t\t(%p)%s: (%p)*Obj{Kind: %v, Type:%s}\n", k, k.Sexp(), v, v.Kind, t)
	}
	buf.WriteString("\t},\n")
	buf.WriteString("\tValues : map[expr.Expr]constant.Value{\n")
	for k, v := range c.Values {
		fmt.Fprintf(buf, "\t\t(%p)%s: %s\n", k, k.Sexp(), v)
	}
	buf.WriteString("\t},\n")
	buf.WriteString("}")
	return buf.String()
}

type Scope struct {
	Parent *Scope
	Objs   map[string]*Obj

	// TODO: NumSpec tipe.Type?
}

func (s *Scope) LookupRec(name string) *Obj {
	for s != nil {
		if o := s.Objs[name]; o != nil {
			return o
		}
		s = s.Parent
	}
	return nil
}

type ObjKind int

const (
	ObjUnknown ObjKind = iota
	ObjVar
	ObjType
)

func (o ObjKind) String() string {
	switch o {
	case ObjUnknown:
		return "ObjUnknown"
	case ObjVar:
		return "ObjVar"
	case ObjType:
		return "ObjType"
	default:
		return fmt.Sprintf("ObjKind(%d)", int(o))
	}
}

// An Obj represents a declared constant, type, variable, or function.
type Obj struct {
	Kind ObjKind
	Type tipe.Type
	Decl interface{} // *expr.FuncLiteral, *stmt.ClassDecl
	Used bool
}

func isTyped(t tipe.Type) bool {
	return t != tipe.Invalid && !isUntyped(t)
}

func isUntyped(t tipe.Type) bool {
	switch t {
	case tipe.UntypedBool, tipe.UntypedInteger, tipe.UntypedFloat, tipe.UntypedComplex:
		return true
	}
	return false
}

func defaultType(t tipe.Type) tipe.Type {
	b, ok := t.(tipe.Basic)
	if !ok {
		return t
	}
	switch b {
	case tipe.UntypedBool:
		return tipe.Bool
	case tipe.UntypedInteger:
		return tipe.Num
	case tipe.UntypedFloat:
		return tipe.Num
	}
	return t
}
