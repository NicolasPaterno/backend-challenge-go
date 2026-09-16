// Package domain has no code of its own: this test guards every package under
// internal/. §5.1 forbids money touching a float anywhere, and §14 lists that
// as disqualifying, so the check runs under plain `go test` rather than a lint
// target someone has to remember.
//
// The walk starts at internal/ rather than internal/domain (05): the HTTP codec
// and the SQL mapping are the first float-capable code outside the domain tree,
// and "anywhere" is cheaper to enforce than to argue about per package.
package domain

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestInternalContainsNoFloat(t *testing.T) {
	banned := []string{"float32", "float64", "complex64", "complex128"}

	var checked int
	err := filepath.WalkDir("..", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		checked++
		ast.Inspect(file, func(node ast.Node) bool {
			if ident, ok := node.(*ast.Ident); ok && slices.Contains(banned, ident.Name) {
				t.Errorf("%s uses %s", path, ident.Name)
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk error = %v", err)
	}
	if checked == 0 {
		t.Fatal("no sources found, the check would pass vacuously")
	}
	t.Logf("checked %d files", checked)
}
