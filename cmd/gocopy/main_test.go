package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const caseSource = `package c

type Request struct {
	Data []byte
	N    int
}

type Response struct {
	Body []byte
}
`

func writeCase(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	for name, content := range map[string]string{
		"go.mod": "module example.com/c\n\ngo 1.24\n",
		"c.go":   caseSource,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

func TestRun(t *testing.T) {
	t.Parallel()

	dir := writeCase(t)

	var stderr bytes.Buffer

	if code := run([]string{"-dir", dir, "-type", "Request, Response,"}, &stderr); code != 0 {
		t.Fatalf("exit code %d: %s", code, stderr.String())
	}

	for _, name := range []string{"request_copy.gen.go", "response_copy.gen.go"} {
		src, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a path under t.TempDir()
		if err != nil {
			t.Fatal(err)
		}

		if !bytes.Contains(src, []byte("CopyFrom(")) {
			t.Fatalf("%s has no CopyFrom method:\n%s", name, src)
		}
	}

	if !strings.Contains(stderr.String(), "wrote") {
		t.Fatalf("stderr = %q, want the written files", stderr.String())
	}
}

func TestRunErrors(t *testing.T) {
	t.Parallel()

	dir := writeCase(t)

	tests := []struct {
		name string
		args []string
		code int
		want string
	}{
		{name: "no type", args: []string{"-dir", dir}, code: 2, want: "-type is required"},
		{name: "bad flag", args: []string{"-nope"}, code: 2, want: "flag provided but not defined"},
		{
			name: "bad dir",
			args: []string{"-dir", filepath.Join(dir, "missing"), "-type", "Request"},
			code: 1,
			want: "load package",
		},
		{name: "missing type", args: []string{"-dir", dir, "-type", "Nope"}, code: 1, want: "not found"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var stderr bytes.Buffer

			if code := run(tt.args, &stderr); code != tt.code {
				t.Fatalf("exit code %d, want %d: %s", code, tt.code, stderr.String())
			}

			if !strings.Contains(stderr.String(), tt.want) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), tt.want)
			}
		})
	}
}

func TestRunWriteError(t *testing.T) {
	t.Parallel()

	dir := writeCase(t)

	// the output path is taken by a directory
	if err := os.Mkdir(filepath.Join(dir, "request_copy.gen.go"), 0o700); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer

	code := run([]string{"-dir", dir, "-type", "Request"}, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "write") {
		t.Fatalf("exit code %d, stderr %q, want a write error", code, stderr.String())
	}
}

// TestRunIdempotent checks that a second run writes what the first did: a
// type sees the CopyFrom of a type generated in the same run (even listed
// after it), and of itself.
func TestRunIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	for name, content := range map[string]string{
		"go.mod": "module example.com/c\n\ngo 1.24\n",
		"c.go": `package c

type Request struct {
	Inner Inner
	Ptr   *Inner
}

type Inner struct{ B []byte }

type Node struct {
	Next *Node
	Data []byte
}
`,
		"c_test.go": `package c

import "testing"

func TestCopy(t *testing.T) {
	src := Request{Inner: Inner{B: []byte("a")}, Ptr: &Inner{B: []byte("b")}}
	dst := src.Copy()
	src.Inner.B[0], src.Ptr.B[0] = 'X', 'X'

	if string(dst.Inner.B) != "a" || string(dst.Ptr.B) != "b" {
		t.Fatalf("dst = %+v", dst)
	}

	list := Node{Next: &Node{Data: []byte("n")}}
	cp := list.Copy()
	list.Next.Data[0] = 'X'

	if cp.Next == list.Next || string(cp.Next.Data) != "n" {
		t.Fatal("Next must be copied")
	}
}
`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	files := []string{"request_copy.gen.go", "inner_copy.gen.go", "node_copy.gen.go"}
	runs := make([][]string, 2)

	for i := range runs {
		var stderr bytes.Buffer

		if code := run([]string{"-dir", dir, "-type", "Request,Inner,Node"}, &stderr); code != 0 {
			t.Fatalf("run %d: exit code %d: %s", i, code, stderr.String())
		}

		for _, name := range files {
			src, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // a path under t.TempDir()
			if err != nil {
				t.Fatal(err)
			}

			runs[i] = append(runs[i], string(src))
		}
	}

	if !slices.Equal(runs[0], runs[1]) {
		t.Fatalf("the second run differs:\n%s\n--- first run ---\n%s",
			strings.Join(runs[1], "\n"), strings.Join(runs[0], "\n"))
	}

	cmd := exec.Command("go", "test", "-count=1", ".")
	cmd.Dir = dir

	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go test: %v\n%s\n%s", err, out, strings.Join(runs[0], "\n"))
	}
}
