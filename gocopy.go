package gocopy

import (
	"bytes"
	"fmt"
	"go/format"
	"go/token"
	"go/types"
	"maps"
	"reflect"
	"slices"
	"strings"
	"text/template"
)

// FileName is the file Generate's output is meant for: <type>_copy.gen.go, lower case.
func FileName(typeName string) string {
	return strings.ToLower(typeName) + "_copy.gen.go"
}

// deepField is a field that must not share memory with src after `*dst = *src`:
// Keep saves dst's own value before the bulk copy, Copy fills it from src after.
type deepField struct {
	Field  string
	Keep   string // bulk mode: save dst's own value before `*dst = *src`
	Copy   string // bulk mode: fill from src after, using the kept value
	Direct string // field-wise mode: the same, straight on dst
}

// Generate returns the gofmt-ed source of a file declaring
// `func (dst *T) CopyFrom(src *T)` — and `func (src *T) Copy() T` when T holds
// no lock — for the struct type named typeName. It is GenerateAll for the one
// type.
func (p *Package) Generate(typeName string) ([]byte, error) {
	srcs, err := p.GenerateAll(typeName)
	if err != nil {
		return nil, err
	}

	return srcs[0], nil
}

// GenerateAll returns what Generate does for each of typeNames, as one run:
// a field of one of these types (or of a pointer to one) is copied through
// the CopyFrom being generated for it, whether or not a file of an earlier
// run declares it yet. The output thus does not depend on the order of the
// types or on the files already generated, and a type that refers to itself
// (Next *Node) copies the pointed-to value recursively from the first run.
func (p *Package) GenerateAll(typeNames ...string) ([][]byte, error) {
	pending := make(map[*types.TypeName]bool, len(typeNames))

	for _, typeName := range typeNames {
		if obj, ok := p.pkg.Scope().Lookup(typeName).(*types.TypeName); ok {
			pending[obj] = true
		}
	}

	srcs := make([][]byte, len(typeNames))

	for i, typeName := range typeNames {
		src, err := p.generate(typeName, pending)
		if err != nil {
			return nil, err
		}

		srcs[i] = src
	}

	return srcs, nil
}

func (p *Package) generate(typeName string, pending map[*types.TypeName]bool) ([]byte, error) {
	obj := p.pkg.Scope().Lookup(typeName)
	if obj == nil {
		return nil, fmt.Errorf("gocopy: type %s not found in package %s", typeName, p.pkg.Name())
	}

	structType, ok := obj.Type().Underlying().(*types.Struct)
	if !ok {
		return nil, fmt.Errorf("gocopy: %s is not a struct", typeName)
	}

	var (
		fields []deepField
		plain  []deepField // field-wise mode: every field that is simply assigned
	)

	g := &generator{
		p:       p,
		pending: pending,
		copying: make(map[string]bool),
		imports: make(map[string]goImport),
		names:   make(map[string]bool),
	}

	// a generic type's methods name its type parameters in the receiver
	recv, lockCheck := typeName, obj.Type()

	if named, ok := obj.Type().(*types.Named); ok && named.TypeParams().Len() > 0 {
		params := make([]string, named.TypeParams().Len())
		for i := range params {
			params[i] = named.TypeParams().At(i).Obj().Name()
			if params[i] == "_" { // src's type must name it
				params[i] = fmt.Sprintf("_%d", i)
			}

			g.names[params[i]] = true // an import must not be shadowed by one
		}

		recv += "[" + strings.Join(params, ", ") + "]"
		lockCheck = structType // Implements is unspecified for an uninstantiated type
	}

	// A struct with a lock or an atomic inside (even nested) cannot be copied
	// with `*dst = *src` (go vet copylocks): then every field is copied on its own.
	bulk := !isLockType(lockCheck)

	for i := range structType.NumFields() {
		f := structType.Field(i)
		tag := reflect.StructTag(structType.Tag(i)).Get("copy")

		if f.Name() == "_" {
			continue // a blank field cannot be referred to: dst keeps its own
		}

		if tag == "" && isLockType(f.Type()) {
			return nil, fmt.Errorf("gocopy: field %s.%s holds a lock or an atomic: "+
				"tag it copy:\"-\" (keep dst's) or copy:\"shallow\" (copy anyway)", typeName, f.Name())
		}

		d, deep, err := g.deepCode(f, tag)
		if err != nil {
			return nil, fmt.Errorf("gocopy: field %s.%s: %w", typeName, f.Name(), err)
		}

		if deep {
			fields = append(fields, d)

			continue
		}

		if tag != "-" {
			plain = append(plain, deepField{Field: f.Name(), Copy: fmt.Sprintf("dst.%s = src.%s", f.Name(), f.Name())})
		}
	}

	data := struct {
		PackageName string
		Recv        string
		Fields      []deepField
		Plain       []deepField
		Imports     []goImport
		Bulk        bool
	}{
		PackageName: p.pkg.Name(),
		Recv:        recv,
		Fields:      fields,
		Plain:       plain,
		Imports:     g.importList(),
		Bulk:        bulk,
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("gocopy: template: %w", err)
	}

	src, err := format.Source(buf.Bytes())
	if err != nil {
		return nil, fmt.Errorf("gocopy: format generated code: %w\n%s", err, buf.String())
	}

	return src, nil
}

// generator holds the state of one generated file: the imports its code needs.
type generator struct {
	p       *Package
	pending map[*types.TypeName]bool // the types generated in this run
	copying map[string]bool          // the slice and map types being copied, against recursion
	imports map[string]goImport      // by path
	names   map[string]bool          // the names the imports take
}

// deepCode describes how f is copied; deep is false for a field the bulk
// assignment copies correctly on its own.
func (g *generator) deepCode(f *types.Var, tag string) (d deepField, deep bool, err error) {
	name := f.Name()
	d = deepField{Field: name}

	switch tag {
	case "-":
		// left as it was in dst
		d.Keep = fmt.Sprintf("keep%s := dst.%s", name, name)
		d.Copy = fmt.Sprintf("dst.%s = keep%s", name, name)
		d.Direct = fmt.Sprintf("// dst.%s kept via tag", name)

		return d, true, nil

	case "shallow":
		return d, false, nil

	case "clone":
		// the value's own Clone makes the copy: dst's memory is not reused
		if !hasClone(f.Type()) {
			typeName := types.TypeString(f.Type(), types.RelativeTo(g.p.pkg))

			return d, false, fmt.Errorf("copy:\"clone\": %s has no method Clone() %s", typeName, typeName)
		}

		d.Copy = cloneCode("dst."+name, "src."+name, f.Type())
		d.Direct = d.Copy

		return d, true, nil
	}

	code, err := g.copyCode("dst."+name, "src."+name, f.Type(), 0)
	if err != nil || code == "" {
		return d, false, err
	}

	d.Direct = code

	if _, ok := f.Type().Underlying().(*types.Interface); ok {
		// cloned through its Clone: dst's value has nothing to give
		d.Copy = code

		return d, true, nil
	}

	d.Keep = fmt.Sprintf("keep%s := dst.%s", name, name)
	d.Copy = fmt.Sprintf("dst.%s = keep%s\n%s", name, name, code)

	if s, ok := f.Type().Underlying().(*types.Slice); ok && g.plain(s.Elem()) {
		// the slice is all there is to copy: keep its emptied array
		d.Keep = fmt.Sprintf("keep%s := dst.%s[:0]", name, name)
		d.Copy = fmt.Sprintf("dst.%[1]s = append(keep%[1]s, src.%[1]s...)\n%[2]s",
			name, nonNil("dst."+name, "src."+name, g.typeName(f.Type())))
	}

	return d, true, nil
}

// plain reports whether a value of type t is copied by assignment.
func (g *generator) plain(t types.Type) bool {
	code, _ := g.copyCode("dst", "src", t, 0)

	return code == ""
}

// copyCode returns the statements that make dst an independent copy of src,
// two addressable expressions of type t, reusing the memory dst owns; ""
// when `dst = src` is one already. depth names the loop variables.
func (g *generator) copyCode(dst, src string, t types.Type, depth int) (string, error) {
	switch ut := t.Underlying().(type) {
	case *types.Slice:
		return g.nested(t, func() (string, error) { return g.sliceCode(dst, src, t, ut, depth) })

	case *types.Map:
		return g.nested(t, func() (string, error) { return g.mapCode(dst, src, t, ut, depth) })

	case *types.Array:
		i := loopVar("i", depth)

		code, err := g.copyCode(dst+"["+i+"]", src+"["+i+"]", ut.Elem(), depth+1)
		if err != nil || code == "" {
			return "", err
		}

		return fmt.Sprintf("for %s := range %s {\n%s\n}", i, src, code), nil

	case *types.Struct:
		if g.hasCopyFrom(t) {
			return fmt.Sprintf("%s.CopyFrom(&%s)", dst, src), nil
		}

	case *types.Interface:
		// An interface that declares Clone() returning itself copies the
		// value inside, whatever its dynamic type. A type parameter's
		// constraint is an interface too: its value is assigned.
		if _, tparam := types.Unalias(t).(*types.TypeParam); !tparam && hasClone(t) {
			return cloneCode(dst, src, t), nil
		}

	case *types.Pointer:
		if _, ok := ut.Elem().Underlying().(*types.Struct); ok && g.hasCopyFrom(ut.Elem()) {
			// dst keeps its own object and copies into it; a nil dst gets a new one
			return fmt.Sprintf(`if %[2]s == nil {
	%[1]s = nil
} else {
	if %[1]s == nil {
		%[1]s = new(%[3]s)
	}
	%[1]s.CopyFrom(%[2]s)
}`, dst, src, g.typeName(ut.Elem())), nil
		}
	}

	return "", nil
}

// nested runs code for the slice or map type t, failing when t contains
// itself (type L []L): its copy would never end.
func (g *generator) nested(t types.Type, code func() (string, error)) (string, error) {
	key := t.String()
	if g.copying[key] {
		return "", fmt.Errorf("type %s contains itself: tag the field copy:\"-\" or copy:\"shallow\"",
			types.TypeString(t, types.RelativeTo(g.p.pkg)))
	}

	g.copying[key] = true
	defer delete(g.copying, key)

	return code()
}

func (g *generator) sliceCode(dst, src string, t types.Type, ut *types.Slice, depth int) (string, error) {
	i := loopVar("i", depth)

	code, err := g.copyCode(dst+"["+i+"]", src+"["+i+"]", ut.Elem(), depth+1)
	if err != nil {
		return "", err
	}

	if code == "" {
		return fmt.Sprintf("%[1]s = append(%[1]s[:0], %[2]s...)\n%[3]s", dst, src, nonNil(dst, src, g.typeName(t))), nil
	}

	// dst's elements, up to its capacity, are its own: copy into them
	return fmt.Sprintf(`if cap(%[1]s) < len(%[2]s) || %[1]s == nil && %[2]s != nil {
	%[1]s = make(%[3]s, len(%[2]s))
} else {
	%[1]s = %[1]s[:len(%[2]s)]
}
for %[4]s := range %[2]s {
%[5]s
}`, dst, src, g.typeName(t), i, code), nil
}

func (g *generator) mapCode(dst, src string, t types.Type, ut *types.Map, depth int) (string, error) {
	k, v, w := loopVar("k", depth), loopVar("v", depth), loopVar("w", depth)

	code, err := g.copyCode(w, v, ut.Elem(), depth+1)
	if err != nil {
		return "", err
	}

	put := fmt.Sprintf("%s[%s] = %s", dst, k, v)
	if code != "" {
		// a value is a fresh copy: dst's old values are gone with clear
		put = fmt.Sprintf("var %[1]s %[2]s\n%[3]s\n%[4]s[%[5]s] = %[1]s", w, g.typeName(ut.Elem()), code, dst, k)
	}

	return fmt.Sprintf(`clear(%[1]s)
if %[2]s != nil {
	if %[1]s == nil {
		%[1]s = make(%[3]s, len(%[2]s))
	}
	for %[4]s, %[5]s := range %[2]s {
		%[6]s
	}
}`, dst, src, g.typeName(t), k, v, put), nil
}

// nonNil returns the statement that makes the slice dst, after an append of
// src's elements, non-nil when src is: an empty src must not come out nil.
func nonNil(dst, src, typeName string) string {
	return fmt.Sprintf(`if %[1]s == nil && %[2]s != nil {
	%[1]s = make(%[3]s, 0)
}`, dst, src, typeName)
}

// loopVar is the name of a variable of the generated code at depth: i, i_1, i_2...
func loopVar(name string, depth int) string {
	if depth == 0 {
		return name
	}

	return fmt.Sprintf("%s_%d", name, depth)
}

// goImport is one import of the generated file; Name is set when it differs
// from the package's own name.
type goImport struct {
	Name, Path string
}

// typeName returns the name of t as seen from the generated file, importing
// every package it refers to.
func (g *generator) typeName(t types.Type) string {
	return types.TypeString(t, g.qualifier)
}

// qualifier is the types.Qualifier of the generated file: a package gets its
// own name unless that is taken by another import, a declaration of the
// package, a predeclared identifier or a local variable of the generated code.
func (g *generator) qualifier(other *types.Package) string {
	if other == g.p.pkg {
		return ""
	}

	if imp, ok := g.imports[other.Path()]; ok {
		if imp.Name != "" {
			return imp.Name
		}

		return other.Name()
	}

	name := other.Name()
	for n := 2; g.taken(name); n++ {
		name = fmt.Sprintf("%s_%d", other.Name(), n)
	}

	imp := goImport{Path: other.Path()}
	if name != other.Name() {
		imp.Name = name
	}

	g.imports[other.Path()] = imp
	g.names[name] = true

	return name
}

func (g *generator) taken(name string) bool {
	return g.names[name] || g.p.pkg.Scope().Lookup(name) != nil || types.Universe.Lookup(name) != nil ||
		isLocalName(name)
}

// isLocalName reports whether name may be a variable of the generated code:
// dst, src, keep<Field>, or a loop variable i, k, v, w (with a _<depth> suffix
// when nested).
func isLocalName(name string) bool {
	if name == "dst" || name == "src" || strings.HasPrefix(name, "keep") {
		return true
	}

	if !strings.ContainsAny(name[:1], "ikvw") {
		return false
	}

	rest, ok := strings.CutPrefix(name[1:], "_")
	if !ok {
		return rest == ""
	}

	return rest != "" && strings.Trim(rest, "0123456789") == ""
}

// importList returns the imports of the generated file, sorted by path.
func (g *generator) importList() []goImport {
	list := make([]goImport, 0, len(g.imports))
	for _, path := range slices.Sorted(maps.Keys(g.imports)) {
		list = append(list, g.imports[path])
	}

	return list
}

// locker is sync.Locker, built here: the package being generated need not import sync.
var locker = func() *types.Interface {
	sig := types.NewSignatureType(nil, nil, nil, nil, nil, false)

	return types.NewInterfaceType([]*types.Func{
		types.NewFunc(token.NoPos, nil, "Lock", sig),
		types.NewFunc(token.NoPos, nil, "Unlock", sig),
	}, nil).Complete()
}()

// isLockType reports whether a value of type t must not be copied: a sync or
// sync/atomic type other than an interface, a struct whose pointer is a
// sync.Locker while the value is not (the rule of go vet copylocks, which
// catches a custom noCopy), or a struct or array holding one by value.
func isLockType(t types.Type) bool {
	return lockType(t, make(map[types.Type]bool))
}

func lockType(t types.Type, seen map[types.Type]bool) bool {
	if seen[t] {
		return false
	}

	seen[t] = true

	if named, ok := types.Unalias(t).(*types.Named); ok {
		if p := named.Obj().Pkg(); p != nil && (p.Path() == "sync" || p.Path() == "sync/atomic") && !types.IsInterface(t) {
			return true
		}
	}

	switch ut := t.Underlying().(type) {
	case *types.Struct:
		if types.Implements(types.NewPointer(t), locker) && !types.Implements(t, locker) {
			return true
		}

		for f := range ut.Fields() {
			if lockType(f.Type(), seen) {
				return true
			}
		}
	case *types.Array:
		return lockType(ut.Elem(), seen)
	}

	return false
}

// hasCopyFrom reports whether *T has a method CopyFrom(*T), or will have
// once this run is over.
func (g *generator) hasCopyFrom(t types.Type) bool {
	if named, ok := types.Unalias(t).(*types.Named); ok && g.pending[named.Origin().Obj()] {
		return true
	}

	meth, _, _ := types.LookupFieldOrMethod(types.NewPointer(t), true, nil, "CopyFrom")
	if meth == nil {
		return false
	}

	sig, ok := meth.Type().(*types.Signature)

	return ok && sig.Params().Len() == 1 && sig.Results().Len() == 0 &&
		types.Identical(sig.Params().At(0).Type(), types.NewPointer(t))
}

// hasClone reports whether a value of type t has a method Clone() t: an
// interface that declares it, or a type like *tls.Config or http.Header.
func hasClone(t types.Type) bool {
	meth, _, _ := types.LookupFieldOrMethod(t, true, nil, "Clone")
	if meth == nil {
		return false
	}

	sig, ok := meth.Type().(*types.Signature)

	return ok && sig.Params().Len() == 0 && sig.Results().Len() == 1 &&
		types.Identical(sig.Results().At(0).Type(), t)
}

// cloneCode makes dst the Clone of src; a nil src, which Clone might not
// take, gives nil. A type parameter cannot be compared with nil: its Clone
// is called whatever the value.
func cloneCode(dst, src string, t types.Type) string {
	if _, tparam := types.Unalias(t).(*types.TypeParam); !tparam {
		switch t.Underlying().(type) {
		case *types.Pointer, *types.Interface, *types.Map, *types.Slice, *types.Chan, *types.Signature:
			return fmt.Sprintf(`%[1]s = nil
if %[2]s != nil {
	%[1]s = %[2]s.Clone()
}`, dst, src)
		}
	}

	return fmt.Sprintf("%s = %s.Clone()", dst, src)
}

var tmpl = template.Must(template.New("copy").Parse(`// Code generated by gocopy; DO NOT EDIT.

package {{.PackageName}}

{{- if .Imports}}

import (
{{- range .Imports}}
	{{with .Name}}{{.}} {{end}}"{{.Path}}"
{{- end}}
)
{{- end}}

{{- if .Bulk}}

// CopyFrom makes dst an independent copy of src: one bulk copy, then the
// fields that would share memory get dst's own, reusing its capacity.
func (dst *{{.Recv}}) CopyFrom(src *{{.Recv}}) {
{{- range .Fields}}
{{- if .Keep}}
	{{.Keep}}
{{- end}}
{{- end}}

	*dst = *src

{{- range .Fields}}
	{{.Copy}}
{{- end}}
}

// Copy returns a new independent copy of src.
func (src *{{.Recv}}) Copy() (dst {{.Recv}}) {
	dst.CopyFrom(src)
	return dst
}
{{- else}}

// CopyFrom makes dst an independent copy of src, field by field: the type
// holds a lock or an atomic, so it cannot be assigned or returned as a whole
// (there is no Copy method for it). Fields tagged copy:"-" keep dst's value.
func (dst *{{.Recv}}) CopyFrom(src *{{.Recv}}) {
{{- range .Plain}}
	{{.Copy}}
{{- end}}
{{- range .Fields}}
	{{.Direct}}
{{- end}}
}
{{- end}}
`))
