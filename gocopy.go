package gocopy

import (
	"bytes"
	"errors"
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
// no lock — for the struct type named typeName.
func (p *Package) Generate(typeName string) ([]byte, error) {
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

	g := &generator{p: p, imports: make(map[string]goImport), names: make(map[string]bool)}

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

var errArrayOfMaps = errors.New("an array of maps: tag it copy:\"-\" or copy:\"shallow\"")

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
	}

	switch ut := f.Type().Underlying().(type) {
	case *types.Slice:
		// nil src with nil dst stays nil
		d.Keep = fmt.Sprintf("keep%s := dst.%s[:0]", name, name)
		d.Copy = fmt.Sprintf("dst.%s = append(keep%s, src.%s...)", name, name, name)
		d.Direct = fmt.Sprintf("dst.%s = append(dst.%s[:0], src.%s...)", name, name, name)

		return d, true, nil

	case *types.Map:
		typeName := g.typeName(f.Type())
		d.Keep = fmt.Sprintf("keep%s := dst.%s", name, name)
		d.Direct = fmt.Sprintf(`clear(dst.%[1]s)
	if src.%[1]s != nil {
		if dst.%[1]s == nil {
			dst.%[1]s = make(%[2]s, len(src.%[1]s))
		}
		for k, v := range src.%[1]s {
			dst.%[1]s[k] = v
		}
	}`, name, typeName)
		d.Copy = fmt.Sprintf("dst.%[1]s = keep%[1]s\n\t%[2]s", name, d.Direct)

		return d, true, nil

	case *types.Struct:
		if hasCopyFrom(f.Type()) {
			d.Keep = fmt.Sprintf("keep%s := dst.%s", name, name)
			d.Copy = fmt.Sprintf("dst.%s = keep%s\n\tdst.%s.CopyFrom(&src.%s)", name, name, name, name)
			d.Direct = fmt.Sprintf("dst.%s.CopyFrom(&src.%s)", name, name)

			return d, true, nil
		}

	case *types.Array:
		return arrayCode(name, ut)

	case *types.Pointer:
		if _, ok := ut.Elem().Underlying().(*types.Struct); ok && hasCopyFrom(ut.Elem()) {
			// dst keeps its own object and copies into it; a nil dst gets a new one
			d.Keep = fmt.Sprintf("keep%s := dst.%s", name, name)
			d.Direct = fmt.Sprintf(`if src.%[1]s == nil {
		dst.%[1]s = nil
	} else {
		if dst.%[1]s == nil {
			dst.%[1]s = new(%[2]s)
		}
		dst.%[1]s.CopyFrom(src.%[1]s)
	}`, name, g.typeName(ut.Elem()))
			d.Copy = fmt.Sprintf("dst.%[1]s = keep%[1]s\n\t%[2]s", name, d.Direct)

			return d, true, nil
		}
	}

	return d, false, nil
}

func arrayCode(name string, ut *types.Array) (deepField, bool, error) {
	d := deepField{Field: name}

	switch ut.Elem().Underlying().(type) {
	case *types.Slice:
		// an array of slices would share every element's memory; copy element-wise
		d.Keep = fmt.Sprintf("keep%s := dst.%s", name, name)
		d.Direct = fmt.Sprintf(`for i := range src.%[1]s {
		dst.%[1]s[i] = append(dst.%[1]s[i][:0], src.%[1]s[i]...)
	}`, name)
		d.Copy = fmt.Sprintf("dst.%[1]s = keep%[1]s\n\t%[2]s", name, d.Direct)

		return d, true, nil

	case *types.Struct:
		if !hasCopyFrom(ut.Elem()) {
			return d, false, nil
		}

		d.Keep = fmt.Sprintf("keep%s := dst.%s", name, name)
		d.Direct = fmt.Sprintf(`for i := range src.%[1]s {
		dst.%[1]s[i].CopyFrom(&src.%[1]s[i])
	}`, name)
		d.Copy = fmt.Sprintf("dst.%[1]s = keep%[1]s\n\t%[2]s", name, d.Direct)

		return d, true, nil

	case *types.Map:
		return d, false, errArrayOfMaps

	default:
		return d, false, nil
	}
}

// generator holds the state of one generated file: the imports its code needs.
type generator struct {
	p       *Package
	imports map[string]goImport // by path
	names   map[string]bool     // the names the imports take
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

	if !strings.Contains("ikvw", name[:1]) {
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

// hasCopyFrom reports whether *T has a method CopyFrom(*T).
func hasCopyFrom(t types.Type) bool {
	meth, _, _ := types.LookupFieldOrMethod(types.NewPointer(t), true, nil, "CopyFrom")
	if meth == nil {
		return false
	}

	sig, ok := meth.Type().(*types.Signature)

	return ok && sig.Params().Len() == 1 && sig.Results().Len() == 0 &&
		types.Identical(sig.Params().At(0).Type(), types.NewPointer(t))
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
	{{.Keep}}
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
