package gocopy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ErrNoPackage is returned by Load when dir holds no Go package.
var ErrNoPackage = errors.New("gocopy: no package found")

// Package is a loaded Go package whose struct types Generate can describe.
type Package struct {
	pkg   *types.Package
	sizes types.Sizes
}

// Name is the package name, as the generated file declares it.
func (p *Package) Name() string {
	return p.pkg.Name()
}

// listed is the part of a `go list -json` entry the loader reads.
type listed struct {
	ImportPath      string            `json:"ImportPath"`
	Dir             string            `json:"Dir"`
	Export          string            `json:"Export"`
	CompiledGoFiles []string          `json:"CompiledGoFiles"`
	ImportMap       map[string]string `json:"ImportMap"`
	Error           *listError        `json:"Error"`
	DepOnly         bool              `json:"DepOnly"`
}

type listError struct {
	Err string `json:"Err"`
}

// Load type-checks the package in dir (the current directory when empty).
//
// The dependencies come as export data from `go list -export`; the package
// itself is checked from source and its own type errors are tolerated, so a
// stale generated file (one that names a field that no longer exists) does
// not stop the type from being generated again. A dependency that does not
// compile is an error.
func Load(dir string) (*Package, error) {
	root, exports, err := goList(dir)
	if err != nil {
		return nil, fmt.Errorf("gocopy: load package: %w", err)
	}

	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(root.CompiledGoFiles))

	for _, name := range root.CompiledGoFiles {
		if !filepath.IsAbs(name) {
			name = filepath.Join(root.Dir, name)
		}

		file, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("gocopy: load package: %w", err)
		}

		files = append(files, file)
	}

	lookup := func(path string) (io.ReadCloser, error) {
		if mapped, ok := root.ImportMap[path]; ok {
			path = mapped
		}

		export := exports[path]
		if export == "" {
			return nil, fmt.Errorf("no export data for %q", path)
		}

		return os.Open(export) //nolint:gosec // a build cache path reported by go list
	}

	conf := types.Config{
		Importer: importer.ForCompiler(fset, "gc", lookup),
		Sizes:    types.SizesFor("gc", build.Default.GOARCH),
		Error:    func(error) {}, // keep checking past the package's own errors
	}

	// with Error set, Check reports the first error but returns the whole package
	pkg, _ := conf.Check(root.ImportPath, fset, files, nil)

	return &Package{pkg: pkg, sizes: conf.Sizes}, nil
}

// goList runs go list for the package in dir and its dependencies: it returns
// the package's own entry and the export data file of every dependency.
func goList(dir string) (root listed, exports map[string]string, err error) {
	cmd := exec.Command("go", "list", "-e",
		"-json=ImportPath,Dir,Export,CompiledGoFiles,ImportMap,Error,DepOnly",
		"-export", "-compiled", "-deps", ".")
	cmd.Dir = dir

	var stderr bytes.Buffer

	cmd.Stderr = &stderr

	out, err := cmd.Output()
	if err != nil {
		return listed{}, nil, fmt.Errorf("go list: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	exports = make(map[string]string)
	found := false
	dec := json.NewDecoder(bytes.NewReader(out))

	for {
		var p listed

		err := dec.Decode(&p)
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return listed{}, nil, fmt.Errorf("go list output: %w", err)
		}

		if !p.DepOnly {
			root, found = p, true

			continue
		}

		if p.Error != nil {
			return listed{}, nil, fmt.Errorf("dependency %s: %s", p.ImportPath, strings.TrimSpace(p.Error.Err))
		}

		exports[p.ImportPath] = p.Export
	}

	if !found {
		return listed{}, nil, ErrNoPackage
	}

	if root.Error != nil && len(root.CompiledGoFiles) == 0 {
		return listed{}, nil, errors.New(strings.TrimSpace(root.Error.Err))
	}

	return root, exports, nil
}
