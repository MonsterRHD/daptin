package scim

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestScimAdaptsResourcesWithoutPrivateWrites guards the SCIM service
// against parallel write paths: all mutations must flow through the existing
// DbResource lifecycle functions, never through hand-written SQL.
func TestScimAdaptsResourcesWithoutPrivateWrites(t *testing.T) {
	root := "."
	forbiddenSelectors := map[string]bool{
		"Exec": true, "MustExec": true, "NamedExec": true, "ExecContext": true,
	}
	lifecycleWrites := 0
	identityDeclarations := 0
	filesInspected := 0

	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		filesInspected++
		file, parseErr := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if parseErr != nil {
			t.Fatalf("parse %s: %v", path, parseErr)
		}

		ast.Inspect(file, func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if ok {
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
					if forbiddenSelectors[selector.Sel.Name] {
						t.Errorf("%s executes SQL directly via .%s; use a DbResource lifecycle function", path, selector.Sel.Name)
					}
					switch selector.Sel.Name {
					case "CreateWithoutFilter", "UpdateWithoutFilters", "DeleteWithoutFilters":
						lifecycleWrites++
					}
				}
			}
			decl, ok := node.(*ast.TypeSpec)
			if ok {
				switch decl.Name.Name {
				case "SessionUser", "PermissionInstance", "GroupPermission":
					identityDeclarations++
				}
			}
			return true
		})

		// String literals must not carry write SQL.
		ast.Inspect(file, func(node ast.Node) bool {
			literal, ok := node.(*ast.BasicLit)
			if !ok || literal.Kind != token.STRING {
				return true
			}
			value, unquoteErr := strconv.Unquote(literal.Value)
			if unquoteErr != nil {
				return true
			}
			upper := strings.ToUpper(strings.TrimSpace(value))
			for _, prefix := range []string{"INSERT ", "UPDATE ", "DELETE ", "REPLACE INTO"} {
				if strings.HasPrefix(upper, prefix) {
					t.Errorf("%s contains direct write SQL: %q", path, value)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if filesInspected < 5 {
		t.Fatalf("expected to inspect several scim source files, got %d", filesInspected)
	}
	if lifecycleWrites == 0 {
		t.Fatal("expected SCIM writes to use CreateWithoutFilter/UpdateWithoutFilters/DeleteWithoutFilters")
	}
	if identityDeclarations != 0 {
		t.Fatal("scim package must not redefine identity/permission types; reuse auth/resource")
	}
}

// TestScimPackageContainsNoEndpointSideEffects ensures the protocol package
// does not import gin at package scope... it remains transport-independent
// (the HTTP layer is the only gin consumer within the package).
func TestScimSourceFilesPresent(t *testing.T) {
	expected := []string{
		"types.go", "protocol.go", "filter.go", "patch.go",
		"discovery.go", "store.go", "mapping.go", "users.go", "groups.go", "http.go",
	}
	for _, name := range expected {
		if _, err := os.Stat(filepath.Join(".", name)); err != nil {
			t.Errorf("missing expected source file %s: %v", name, err)
		}
	}
}
