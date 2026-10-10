package static

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestNoExecImports asserts that the import graph of the permission packages
// (test files included) contains no os/exec and no syscall, so neither the
// classifier nor its tests can ever run a command.
func TestNoExecImports(t *testing.T) {
	forbidden := map[string]bool{"os/exec": true, "syscall": true}
	seen := map[string]bool{}
	var queue []string

	// Package directories relative to this test's working directory.
	for _, pkg := range []string{".", ".."} {
		dir, err := filepath.Abs(pkg)
		if err != nil {
			t.Fatal(err)
		}
		queue = append(queue, dir)
	}
	modRoot := moduleRoot(t)

	for len(queue) > 0 {
		dir := queue[0]
		queue = queue[1:]
		if seen[dir] {
			continue
		}
		seen[dir] = true
		includeTests := dir == mustAbs(t, ".") || dir == mustAbs(t, "..")
		for _, imp := range importsOf(t, dir, includeTests) {
			if forbidden[imp] {
				t.Errorf("package %s imports forbidden %q", dir, imp)
			}
			if strings.HasPrefix(imp, "github.com/unix/ugh/") {
				rel := strings.TrimPrefix(imp, "github.com/unix/ugh/")
				queue = append(queue, filepath.Join(modRoot, filepath.FromSlash(rel)))
			}
		}
	}
}

func mustAbs(t *testing.T, p string) string {
	t.Helper()
	a, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir := mustAbs(t, ".")
	for i := 0; i < 12; i++ {
		if b, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil && strings.HasPrefix(string(b), "module ") {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("go.mod not found")
	return ""
}

// importsOf lists the import paths of a package directory. Non-test files
// always count; test files only when includeTests (the package's own tests
// are part of its test binary, other packages' tests are not).
func importsOf(t *testing.T, dir string, includeTests bool) []string {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi fs.FileInfo) bool {
		name := fi.Name()
		if !strings.HasSuffix(name, ".go") {
			return false
		}
		if strings.HasSuffix(name, "_test.go") {
			return includeTests
		}
		return true
	}, parser.ImportsOnly)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	var out []string
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			for _, imp := range f.Imports {
				s, err := strconv.Unquote(imp.Path.Value)
				if err != nil {
					t.Fatalf("bad import %s: %v", imp.Path.Value, err)
				}
				out = append(out, s)
			}
		}
	}
	return out
}
