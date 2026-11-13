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
		{pkg: fieldwise, typeName: "Recursive", want: "type Tree contains itself"},
		{pkg: fieldwise, typeName: "RecursiveMap", want: "type Index contains itself"},
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

	srcs, err := pkg.GenerateAll(typeNames...)
	if err != nil {
		t.Fatalf("GenerateAll(%v): %v", typeNames, err)
	}

	gen := make(map[string]string, len(typeNames))

	for i, typeName := range typeNames {
		gen[typeName] = string(srcs[i])

		if err := os.WriteFile(filepath.Join(dir, gocopy.FileName(typeName)), srcs[i], 0o600); err != nil {
			t.Fatal(err)
		}
	}

	for _, args := range [][]string{{"vet", "./..."}, {"test", "-count=1", "./..."}} {
		cmd := exec.Command("go", args...) //nolint:gosec // fixed arguments
		cmd.Dir = dir

		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go %s: %v\n%s\n%s", strings.Join(args, " "), err, out,
				strings.Join(slices.Collect(maps.Values(gen)), "\n"))
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
	V1s []map[xv1.T]bool
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
	O  Outer
	OP *Outer
	Ot Other
	V  Value
	In Inner
}
`,
	}, "T")

	for _, field := range []string{"O", "OP", "Ot", "V"} {
		if strings.Contains(gen["T"], "dst."+field+".CopyFrom") {
			t.Errorf("field %s has no CopyFrom of its own type:\n%s", field, gen["T"])
		}
	}

	if !strings.Contains(gen["T"], "dst.In.CopyFrom(&src.In)") {
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

// TestGenerateNested checks that nested collections share nothing with src:
// slices of slices or of structs with CopyFrom, maps of slices or of maps,
// arrays of arrays of slices, and so on.
func TestGenerateNested(t *testing.T) {
	t.Parallel()

	gen := checkGenerated(t, "nested", map[string]string{
		"root.go": `package root

import "sync"

type Inner struct{ B []byte }

func (dst *Inner) CopyFrom(src *Inner) { dst.B = append(dst.B[:0], src.B...) }

type Matrix [][]float64

type T struct {
	BB     [][]byte
	Inners []Inner
	Ptrs   []*Inner
	MB     map[string][]byte
	MI     map[string]Inner
	AAB    [2][2][]byte
	ASB    [2][][]byte
	AM     [2]map[string]int
	SM     []map[string]int
	MM     map[string]map[string]int
	Mx     Matrix
	Plain  [2][2]int
}

type Locked struct {
	Mu sync.Mutex ` + "`copy:\"-\"`" + `
	BB [][]byte
}
`,
		"root_test.go": `package root

import (
	"reflect"
	"testing"
)

func source() *T {
	return &T{
		BB:     [][]byte{[]byte("bb")},
		Inners: []Inner{{B: []byte("in")}},
		Ptrs:   []*Inner{{B: []byte("p")}, nil},
		MB:     map[string][]byte{"k": []byte("mb")},
		MI:     map[string]Inner{"k": {B: []byte("mi")}},
		AAB:    [2][2][]byte{{[]byte("aab")}},
		ASB:    [2][][]byte{{[]byte("asb")}},
		AM:     [2]map[string]int{{"k": 1}},
		SM:     []map[string]int{{"k": 1}},
		MM:     map[string]map[string]int{"k": {"k": 1}},
		Mx:     Matrix{{1}},
		Plain:  [2][2]int{{1}},
	}
}

func mutate(t *T) {
	t.BB[0][0] = 'X'
	t.Inners[0].B[0] = 'X'
	t.Ptrs[0].B[0] = 'X'
	t.MB["k"][0] = 'X'
	t.MI["k"].B[0] = 'X'
	t.AAB[0][0][0] = 'X'
	t.ASB[0][0][0] = 'X'
	t.AM[0]["k"] = 9
	t.SM[0]["k"] = 9
	t.MM["k"]["k"] = 9
	t.Mx[0][0] = 9
}

func TestCopy(t *testing.T) {
	src := source()

	// dst's own memory, some of it larger than src's
	dst := T{BB: [][]byte{make([]byte, 0, 8), make([]byte, 0, 8)}, Inners: make([]Inner, 0, 4)}
	bb := &dst.BB[:1][0][:1][0]

	dst.CopyFrom(src)

	if !reflect.DeepEqual(&dst, source()) {
		t.Fatalf("dst = %+v", dst)
	}

	if &dst.BB[0][0] != bb || cap(dst.Inners) != 4 {
		t.Fatal("dst's own memory must be reused")
	}

	mutate(src)

	if !reflect.DeepEqual(&dst, source()) {
		t.Fatalf("dst changed with src: %+v", dst)
	}

	cp := source().Copy()
	mutate(&cp)

	var l Locked
	l.CopyFrom(&Locked{BB: [][]byte{[]byte("l")}})

	if string(l.BB[0]) != "l" {
		t.Fatalf("l.BB = %q", l.BB)
	}
}
`,
	}, "T", "Locked")

	if strings.Contains(gen["T"], "keepPlain") {
		t.Errorf("an array of numbers needs no code:\n%s", gen["T"])
	}
}

// TestGenerateNilSlices checks the nil-ness of copied slices: a non-nil src
// gives a non-nil dst, even when empty; a nil src empties dst, which keeps its
// array; so Copy, starting from a zero dst, keeps nil and empty apart.
func TestGenerateNilSlices(t *testing.T) {
	t.Parallel()

	checkGenerated(t, "nilslices", map[string]string{
		"root.go": `package root

import "sync"

type T struct {
	B  []byte
	BB [][]byte
	A  [1][]byte
	M  map[string][]int
}

type Locked struct {
	Mu sync.Mutex ` + "`copy:\"-\"`" + `
	B  []byte
}
`,
		"root_test.go": `package root

import (
	"encoding/json"
	"testing"
)

func TestNil(t *testing.T) {
	empty := T{B: []byte{}, BB: [][]byte{{}}, A: [1][]byte{{}}, M: map[string][]int{"k": {}}}
	cp := empty.Copy()

	if cp.B == nil || cp.BB == nil || cp.BB[0] == nil || cp.A[0] == nil || cp.M["k"] == nil {
		t.Fatalf("an empty src gave nil: %#v", cp)
	}

	got, _ := json.Marshal(&cp)
	want, _ := json.Marshal(&empty)

	if string(got) != string(want) {
		t.Fatalf("Copy marshals to %s, want %s", got, want)
	}

	var zero T
	if cp := zero.Copy(); cp.B != nil || cp.BB != nil || cp.A[0] != nil {
		t.Fatalf("a nil src gave non-nil: %#v", cp)
	}

	dst := T{B: make([]byte, 1, 8), BB: make([][]byte, 1, 8)}
	dst.CopyFrom(&zero)

	if len(dst.B) != 0 || cap(dst.B) != 8 || len(dst.BB) != 0 || cap(dst.BB) != 8 {
		t.Fatalf("a nil src must empty dst and keep its arrays: %#v", dst)
	}

	var l Locked
	l.CopyFrom(&Locked{B: []byte{}})

	if l.B == nil {
		t.Fatal("an empty src gave nil")
	}
}
`,
	}, "T", "Locked")
}

// TestGenerateClone checks that an interface declaring Clone() itself copies
// the value inside, and that copy:"clone" copies a field through its Clone.
func TestGenerateClone(t *testing.T) {
	t.Parallel()

	srcs := checkGenerated(t, "clone", map[string]string{
		"root.go": `package root

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"sync"
)

type Shape interface {
	Area() int
	Clone() Shape
}

type Rect struct{ W, H int }

func (r *Rect) Area() int    { return r.W * r.H }
func (r *Rect) Clone() Shape { c := *r; return &c }

type Version struct{ parts []int }

func (v Version) Clone() Version { return Version{parts: append([]int(nil), v.parts...)} }

type T struct {
	S      Shape
	Shapes []Shape
	ByName map[string]Shape
	Arr    [2]Shape
	Str    fmt.Stringer
	Conf   *tls.Config ` + "`copy:\"clone\"`" + `
	Header http.Header ` + "`copy:\"clone\"`" + `
	Ver    Version     ` + "`copy:\"clone\"`" + `
}

type Locked struct {
	Mu sync.Mutex ` + "`copy:\"-\"`" + `
	S  Shape
}

type Box[V interface{ Clone() V }] struct {
	Cloned V ` + "`copy:\"clone\"`" + `
	Shared V
}
`,
		"root_test.go": `package root

import (
	"crypto/tls"
	"net/http"
	"strings"
	"testing"
)

func TestClone(t *testing.T) {
	r, sb := &Rect{1, 2}, &strings.Builder{}
	src := T{
		S:      r,
		Shapes: []Shape{r, nil},
		ByName: map[string]Shape{"r": r},
		Arr:    [2]Shape{r},
		Str:    sb,
		Conf:   &tls.Config{ServerName: "a"},
		Header: http.Header{"K": {"v"}},
		Ver:    Version{parts: []int{1}},
	}
	cp := src.Copy()

	if cp.S == Shape(r) || cp.Shapes[0] == Shape(r) || cp.ByName["r"] == Shape(r) || cp.Arr[0] == Shape(r) {
		t.Fatal("a Shape is shared with src")
	}

	if cp.S.Area() != 2 || cp.Shapes[1] != nil || cp.Arr[1] != nil {
		t.Fatalf("Copy = %+v, want the shapes cloned and nil kept nil", cp)
	}

	if cp.Str != src.Str {
		t.Fatal("an interface without Clone must be assigned")
	}

	if cp.Conf == src.Conf || cp.Conf.ServerName != "a" {
		t.Fatal("Conf is not cloned")
	}

	cp.Header["K"][0], cp.Ver.parts[0] = "w", 2
	if src.Header["K"][0] != "v" || src.Ver.parts[0] != 1 {
		t.Fatal("Header or Ver is shared with src")
	}

	dst := T{S: &Rect{3, 3}, Conf: &tls.Config{}}
	dst.CopyFrom(&T{})

	if dst.S != nil || dst.Conf != nil {
		t.Fatalf("a nil src gave %+v, want nil", dst)
	}

	var l Locked
	l.CopyFrom(&Locked{S: r})

	if l.S == Shape(r) || l.S.Area() != 2 {
		t.Fatal("Locked.S is not cloned")
	}

	b := Box[Version]{Cloned: Version{parts: []int{1}}, Shared: Version{parts: []int{1}}}
	bc := b.Copy()
	bc.Cloned.parts[0], bc.Shared.parts[0] = 2, 2

	if b.Cloned.parts[0] != 1 || b.Shared.parts[0] != 2 {
		t.Fatal("Box: Cloned must be cloned and Shared assigned")
	}
}
`,
	}, "T", "Locked", "Box")

	if strings.Contains(srcs["T"], "keepS ") {
		t.Errorf("T keeps dst's Shape, which it never uses:\n%s", srcs["T"])
	}
}

func TestGenerateCloneMissing(t *testing.T) {
	t.Parallel()

	dir := writeModule(t, "noclone", map[string]string{
		"root.go": "package root\n\ntype T struct {\n\tB []byte `copy:\"clone\"`\n}\n",
	})

	pkg, err := gocopy.Load(dir)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pkg.Generate("T"); err == nil || !strings.Contains(err.Error(), "has no method Clone() []byte") {
		t.Fatalf("Generate(T) error = %v, want no Clone", err)
	}
}
