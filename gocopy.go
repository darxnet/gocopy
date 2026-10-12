package gocopy

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"go/types"
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
	Import string
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
		imports []string
		fields  []deepField
		plain   []deepField // field-wise mode: every field that is simply assigned
	)

	// A struct with a lock or an atomic inside (even nested) cannot be copied
	// with `*dst = *src` (go vet copylocks): then every field is copied on its own.
	bulk := !containsLock(structType)

	for i := range structType.NumFields() {
		f := structType.Field(i)
		tag := reflect.StructTag(structType.Tag(i)).Get("copy")

		if tag == "" && isLockType(f.Type()) {
			return nil, fmt.Errorf("gocopy: field %s.%s holds a lock or an atomic: "+
				"tag it copy:\"-\" (keep dst's) or copy:\"shallow\" (copy anyway)", typeName, f.Name())
		}

		d, deep, err := p.deepCode(f, tag)
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

	for _, f := range fields {
		if f.Import != "" {
			imports = append(imports, f.Import)
		}
	}

	imports = slices.Compact(slices.Sorted(slices.Values(imports)))

	data := struct {
		PackageName string
		TypeName    string
		Fields      []deepField
		Plain       []deepField
		Imports     []string
		Bulk        bool
	}{
		PackageName: p.pkg.Name(),
		TypeName:    typeName,
		Fields:      fields,
		Plain:       plain,
		Imports:     imports,
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
func (p *Package) deepCode(f *types.Var, tag string) (d deepField, deep bool, err error) {
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
		typeName := p.typeName(f.Type(), &d.Import)
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
	}`, name, p.typeName(ut.Elem(), &d.Import))
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

// typeName returns the name of t as seen from the package, recording the import it needs.
func (p *Package) typeName(t types.Type, importPath *string) string {
	return types.TypeString(t, func(other *types.Package) string {
		if p.pkg == other {
			return ""
		}

		*importPath = other.Path()

		return other.Name()
	})
}

// isLockType reports whether t is a sync or atomic type, or a struct holding one.
func isLockType(t types.Type) bool {
	if named, ok := t.(*types.Named); ok {
		if p := named.Obj().Pkg(); p != nil && (p.Path() == "sync" || p.Path() == "sync/atomic") {
			return true
		}
	}

	switch ut := t.Underlying().(type) {
	case *types.Struct:
		return containsLock(ut)
	case *types.Array:
		return isLockType(ut.Elem())
	}

	return false
}

// containsLock reports whether any field of st, nested by value, is a lock type.
func containsLock(st *types.Struct) bool {
	for f := range st.Fields() {
		if isLockType(f.Type()) {
			return true
		}
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

	return ok && sig.Params().Len() == 1 && sig.Results().Len() == 0
}

var tmpl = template.Must(template.New("copy").Parse(`// Code generated by gocopy; DO NOT EDIT.

package {{.PackageName}}

{{- if .Imports}}

import (
{{- range .Imports}}
	"{{.}}"
{{- end}}
)
{{- end}}

{{- if .Bulk}}

// CopyFrom makes dst an independent copy of src: one bulk copy, then the
// fields that would share memory get dst's own, reusing its capacity.
func (dst *{{.TypeName}}) CopyFrom(src *{{.TypeName}}) {
{{- range .Fields}}
	{{.Keep}}
{{- end}}

	*dst = *src

{{- range .Fields}}
	{{.Copy}}
{{- end}}
}

// Copy returns a new independent copy of src.
func (src *{{.TypeName}}) Copy() (dst {{.TypeName}}) {
	dst.CopyFrom(src)
	return dst
}
{{- else}}

// CopyFrom makes dst an independent copy of src, field by field: the type
// holds a lock or an atomic, so it cannot be assigned or returned as a whole
// (there is no Copy method for it). Fields tagged copy:"-" keep dst's value.
func (dst *{{.TypeName}}) CopyFrom(src *{{.TypeName}}) {
{{- range .Plain}}
	{{.Copy}}
{{- end}}
{{- range .Fields}}
	{{.Direct}}
{{- end}}
}
{{- end}}
`))
