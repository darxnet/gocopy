package gocopy_test

import (
	"bytes"
	"os"
	"path/filepath"
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
