package gocopy_test

import (
	"bytes"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/darxnet/gocopy"
)

// TestGenerateMatchesCommitted checks the generator against the files it
// produced for the test cases: any change in the output shows up as a diff,
// and the committed files are known to be current.
func TestGenerateMatchesCommitted(t *testing.T) {
	t.Parallel()

	cases := []struct {
		dir, typeName, pkgName string
	}{
		{dir: "internal/cases/bulk", typeName: "Request", pkgName: "bulk"},
		{dir: "internal/cases/fieldwise", typeName: "State", pkgName: "fieldwise"},
	}

	for _, tc := range cases {
		t.Run(tc.typeName, func(t *testing.T) {
			t.Parallel()

			pkg, err := gocopy.Load(tc.dir)
			if err != nil {
				t.Fatal(err)
			}

			if pkg.Name() != tc.pkgName {
				t.Fatalf("Name() = %q, want %q", pkg.Name(), tc.pkgName)
			}

			got, err := pkg.Generate(tc.typeName)
			if err != nil {
				t.Fatal(err)
			}

			want, err := os.ReadFile(filepath.Join(tc.dir, gocopy.FileName(tc.typeName)))
			if err != nil {
				t.Fatal(err)
			}

			if !bytes.Equal(got, want) {
				t.Fatalf("generated code differs from the committed file; run go generate ./...\n"+
					"--- got ---\n%s\n--- want ---\n%s", got, want)
			}
		})
	}
}

func TestGenerateModes(t *testing.T) {
	t.Parallel()

	bulk, err := gocopy.Load("internal/cases/bulk")
	if err != nil {
		t.Fatal(err)
	}

	src, err := bulk.Generate("Request")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"*dst = *src", // bulk assignment
		"dst.Data = append(keepData, src.Data...)",       // slices into dst's arrays
		"make(map[string]consts.Level, len(src.Levels))", // maps, with the imported value type
		`"github.com/darxnet/gocopy/internal/cases/consts"`,
		"dst.Inner.CopyFrom(&src.Inner)",     // nested CopyFrom
		"dst.InnerPtr = new(Inner)",          // pointer: dst gets its own object
		"dst.Arr[i] = append(dst.Arr[i][:0]", // array of slices
		"dst.Inners[i].CopyFrom(&src.Inners[i])",
		"dst.Keep = keepKeep", // copy:"-"
		"func (src *Request) Copy() (dst Request)",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("bulk output lacks %q", want)
		}
	}

	if strings.Contains(string(src), "Shared") {
		t.Error("a copy:\"shallow\" field needs no code of its own")
	}

	fieldwise, err := gocopy.Load("internal/cases/fieldwise")
	if err != nil {
		t.Fatal(err)
	}

	src, err = fieldwise.Generate("State")
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"dst.N = src.N", // plain fields one by one
		"// dst.Mu kept via tag",
		"// dst.Count kept via tag",
		"// dst.Stats kept via tag",
		"dst.Data = append(dst.Data[:0], src.Data...)",
		"dst.Inner.CopyFrom(&src.Inner)",
	} {
		if !strings.Contains(string(src), want) {
			t.Errorf("field-wise output lacks %q", want)
		}
	}

	if strings.Contains(string(src), "*dst = *src") || strings.Contains(string(src), "Copy()") {
		t.Error("a struct with a lock gets neither a bulk assignment nor a Copy method")
	}
}

func TestGenerateErrors(t *testing.T) {
	t.Parallel()

	bulk, err := gocopy.Load("internal/cases/bulk")
	if err != nil {
		t.Fatal(err)
	}

	fieldwise, err := gocopy.Load("internal/cases/fieldwise")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		pkg      *gocopy.Package
		typeName string
		want     string
	}{
		{pkg: bulk, typeName: "Missing", want: "not found"},
		{pkg: bulk, typeName: "NotAStruct", want: "not a struct"},
		{pkg: fieldwise, typeName: "Untagged", want: "holds a lock"},
		{pkg: fieldwise, typeName: "ArrayOfMaps", want: "array of maps"},
	}

	for _, tt := range tests {
		if _, err := tt.pkg.Generate(tt.typeName); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Errorf("Generate(%s) error = %v, want %q", tt.typeName, err, tt.want)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	t.Parallel()

	if _, err := gocopy.Load(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("Load of a missing directory must fail")
	}

	empty := writeModule(t, "empty", map[string]string{})

	if _, err := gocopy.Load(empty); err == nil {
		t.Fatal("Load of a directory without Go files must fail")
	}

	broken := writeModule(t, "broken", map[string]string{
		"dep/dep.go": "package dep\n\nvar X = undefined\n",
		"root.go": "package broken\n\nimport \"example.com/broken/dep\"\n\n" +
			"var _ = dep.X\n\ntype T struct{ Data []byte }\n",
	})

	if _, err := gocopy.Load(broken); err == nil || !strings.Contains(err.Error(), "dependency example.com/broken/dep") {
		t.Fatalf("Load with a broken dependency = %v, want its error", err)
	}
}

// TestLoadStaleGeneratedFile checks that a generated file that no longer
// compiles (its field was removed) does not stop the type from being
// generated again.
func TestLoadStaleGeneratedFile(t *testing.T) {
	t.Parallel()

	dir := writeModule(t, "stale", map[string]string{
		"t.go":               "package stale\n\nimport \"time\"\n\ntype T struct {\n\tWhen time.Time\n\tData []byte\n}\n",
		gocopy.FileName("T"): "package stale\n\nfunc (r *T) CopyFrom(src *T) {\n\tr.Removed = nil\n}\n",
	})

	pkg, err := gocopy.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	src, err := pkg.Generate("T")
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(src), "Data") || strings.Contains(string(src), "Removed") {
		t.Fatalf("generated from a stale package:\n%s", src)
	}
}

// writeModule creates a throwaway module named example.com/<name> with files in it.
func writeModule(t *testing.T, name string, files map[string]string) string {
	t.Helper()

	dir := t.TempDir()
	files["go.mod"] = "module example.com/" + name + "\n\ngo 1.24\n"

	for file, content := range files {
		path := filepath.Join(dir, file)

		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func TestFileName(t *testing.T) {
	t.Parallel()

	if got := gocopy.FileName("LoadVars"); got != "loadvars_copy.gen.go" {
		t.Fatalf("FileName = %q", got)
	}
}

// checkGenerated writes a throwaway module, generates the CopyFrom of each
// type in typeNames in its root package and runs go vet and go test on the
// result: the generated code must compile, pass vet and the module's own
// tests. It returns the generated sources by type name.
func checkGenerated(t *testing.T, name string, files map[string]string, typeNames ...string) map[string]string {
	t.Helper()

	dir := writeModule(t, name, files)

	pkg, err := gocopy.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	gen := make(map[string]string, len(typeNames))

	for _, typeName := range typeNames {
		src, err := pkg.Generate(typeName)
		if err != nil {
			t.Fatalf("Generate(%s): %v", typeName, err)
		}

		gen[typeName] = string(src)

		if err := os.WriteFile(filepath.Join(dir, gocopy.FileName(typeName)), src, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{{"vet", "./..."}, {"test", "-count=1", "./..."}} {
		cmd := exec.Command("go", args...)
		cmd.Dir = dir

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %s: %v\n%s\n%s", strings.Join(args, " "), err, out, strings.Join(slices.Collect(maps.Values(gen)), "\n"))
		}
	}

	return gen
}

// TestGenerateImports checks that every package a field's type names is
// imported, and that two packages with the same name get distinct names.
func TestGenerateImports(t *testing.T) {
	t.Parallel()

	gen := checkGenerated(t, "imports", map[string]string{
		"a/a.go":     "package a\n\ntype K string\n",
		"b/b.go":     "package b\n\ntype V int\n",
		"x/v1/v1.go": "package v1\n\ntype T int\n",
		"y/v1/v1.go": "package v1\n\ntype U int\n",
		"box/box.go": "package box\n\ntype Box[T any] struct{ X T }\n\n" +
			"func (dst *Box[T]) CopyFrom(src *Box[T]) { *dst = *src }\n",
		"root.go": `package root

import (
	"example.com/imports/a"
	"example.com/imports/b"
	"example.com/imports/box"
	xv1 "example.com/imports/x/v1"
	yv1 "example.com/imports/y/v1"
)

// v1 takes the name, so neither package may be imported as v1
var v1 = 1

type T struct {
	AB  map[a.K]b.V
	V1  map[xv1.T]yv1.U
	Box *box.Box[a.K]
}
`,
		"root_test.go": `package root

import (
	"testing"

	"example.com/imports/a"
	"example.com/imports/b"
	"example.com/imports/box"
)

func TestCopy(t *testing.T) {
	src := T{AB: map[a.K]b.V{"k": 1}, Box: &box.Box[a.K]{X: "x"}}
	dst := src.Copy()

	if dst.AB["k"] != 1 || dst.Box == src.Box || dst.Box.X != "x" || v1 != 1 {
		t.Fatalf("dst = %+v", dst)
	}
}
`,
	}, "T")

	for _, want := range []string{
		`v1_2 "example.com/imports/x/v1"`,
		`v1_3 "example.com/imports/y/v1"`,
		"make(map[v1_2.T]v1_3.U, len(src.V1))",
		"new(box.Box[a.K])",
	} {
		if !strings.Contains(gen["T"], want) {
			t.Errorf("generated code lacks %q:\n%s", want, gen["T"])
		}
	}
}

func TestIsLocalName(t *testing.T) {
	t.Parallel()

	for name, want := range map[string]bool{
		"dst": true, "src": true, "keepData": true, "i": true, "k": true, "v_2": true, "w_10": true,
		"v1": false, "v_": false, "v_x": false, "vv": false, "a": false, "kv": false,
	} {
		if got := gocopy.IsLocalName(name); got != want {
			t.Errorf("isLocalName(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestGenerateCopyFromSignature checks that only CopyFrom(*T) counts as T's
// CopyFrom: one promoted from an embedded type, or taking another type, is
// not called.
func TestGenerateCopyFromSignature(t *testing.T) {
	t.Parallel()

	gen := checkGenerated(t, "signature", map[string]string{
		"root.go": `package root

type Inner struct{ B []byte }

func (dst *Inner) CopyFrom(src *Inner) { dst.B = append(dst.B[:0], src.B...) }

// Outer gets Inner's CopyFrom(*Inner) by embedding
type Outer struct {
	Inner
	N int
}

type Other struct{ N int }

func (dst *Other) CopyFrom(src *Inner) { dst.N = len(src.B) }

type Value struct{ N int }

func (dst *Value) CopyFrom(src Value) { dst.N = src.N }

type T struct {
	Outer    Outer
	OuterPtr *Outer
	Other    Other
	Value    Value
	Inner    Inner
}
`,
	}, "T")

	for _, field := range []string{"Outer", "OuterPtr", "Other", "Value"} {
		if strings.Contains(gen["T"], "dst."+field+".CopyFrom") {
			t.Errorf("%s has no CopyFrom(*%s):\n%s", field, field, gen["T"])
		}
	}

	if !strings.Contains(gen["T"], "dst.Inner.CopyFrom(&src.Inner)") {
		t.Errorf("Inner's CopyFrom is not called:\n%s", gen["T"])
	}
}

// TestGenerateLocks checks lock detection against go vet copylocks: a type
// whose pointer is a sync.Locker and value is not (a custom noCopy) is a lock,
// a sync.Locker interface is not, and a blank field is never referred to.
func TestGenerateLocks(t *testing.T) {
	t.Parallel()

	const locks = `package root

import "sync"

type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

type NoCopy struct {
	nc   noCopy ` + "`copy:\"-\"`" + `
	Data []byte
}

type Blank struct {
	_    noCopy
	_    []byte
	Data []byte
}

type Locker struct {
	L    sync.Locker
	Data []byte
}

type Untagged struct {
	nc [2]noCopy
}
`

	gen := checkGenerated(t, "locks", map[string]string{
		"root.go": locks,
		"root_test.go": `package root

import (
	"sync"
	"testing"
)

func TestCopy(t *testing.T) {
	var mu sync.Mutex

	src := Locker{L: &mu, Data: []byte("x")}
	dst := src.Copy()

	if dst.L != &mu || string(dst.Data) != "x" {
		t.Fatalf("dst = %+v", dst)
	}

	var b Blank
	b.CopyFrom(&Blank{Data: []byte("b")})

	if string(b.Data) != "b" {
		t.Fatalf("b.Data = %q", b.Data)
	}
}
`,
	}, "NoCopy", "Blank", "Locker")

	for typeName, bulk := range map[string]bool{"NoCopy": false, "Blank": false, "Locker": true} {
		if got := strings.Contains(gen[typeName], "Copy() (dst"); got != bulk {
			t.Errorf("%s: Copy method generated = %v, want %v:\n%s", typeName, got, bulk, gen[typeName])
		}
	}

	pkg, err := gocopy.Load(writeModule(t, "untagged", map[string]string{"root.go": locks}))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pkg.Generate("Untagged"); err == nil || !strings.Contains(err.Error(), "holds a lock") {
		t.Fatalf("Generate(Untagged) error = %v, want a lock error", err)
	}
}

// TestGenerateGeneric checks the methods of generic structs: the receiver
// lists the type parameters.
func TestGenerateGeneric(t *testing.T) {
	t.Parallel()

	gen := checkGenerated(t, "generic", map[string]string{
		"root.go": `package root

import "sync"

type Box[T any] struct {
	Items []T
	M     map[string]T
	X     T
}

type Pair[K comparable, V any] struct {
	M   map[K]V
	Box Box[V]
}

type Locked[_ any] struct {
	Mu   sync.Mutex ` + "`copy:\"-\"`" + `
	Data []byte
}
`,
		"root_test.go": `package root

import "testing"

func TestCopy(t *testing.T) {
	src := Box[string]{Items: []string{"a"}, M: map[string]string{"k": "v"}, X: "x"}
	dst := src.Copy()
	src.Items[0], src.M["k"] = "A", "V"

	if dst.Items[0] != "a" || dst.M["k"] != "v" || dst.X != "x" {
		t.Fatalf("dst = %+v", dst)
	}

	var p Pair[int, []byte]
	p.CopyFrom(&Pair[int, []byte]{M: map[int][]byte{1: nil}})

	if len(p.M) != 1 {
		t.Fatalf("p = %+v", p)
	}

	var l Locked[int]
	l.CopyFrom(&Locked[int]{Data: []byte("d")})

	if string(l.Data) != "d" {
		t.Fatalf("l.Data = %q", l.Data)
	}
}
`,
	}, "Box", "Pair", "Locked")

	for typeName, want := range map[string]string{
		"Box":    "func (src *Box[T]) Copy() (dst Box[T])",
		"Pair":   "func (dst *Pair[K, V]) CopyFrom(src *Pair[K, V])",
		"Locked": "func (dst *Locked[_0]) CopyFrom(src *Locked[_0])",
	} {
		if !strings.Contains(gen[typeName], want) {
			t.Errorf("%s lacks %q:\n%s", typeName, want, gen[typeName])
		}
	}
}
