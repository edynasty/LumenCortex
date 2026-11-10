package editing

import (
	"fmt"
	"go/ast"
	"go/importer"
	"go/parser"
	"go/scanner"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// GoSyntax validates that every planned Go file parses. It is cheap and needs no dependencies, so it
// runs on every change to a .go file.
func GoSyntax() Validator {
	return func(files map[string][]byte) []Diagnostic {
		var diags []Diagnostic
		for path, content := range files {
			if !strings.HasSuffix(path, ".go") {
				continue
			}
			fset := token.NewFileSet()
			if _, err := parser.ParseFile(fset, path, content, parser.ParseComments|parser.AllErrors); err != nil {
				diags = append(diags, parseDiagnostics(err, CodeSyntax)...)
			}
		}
		sortDiagnostics(diags)
		return diags
	}
}

// GoTypes validates that the planned Go files of a package parse and type check. The package's other
// files are read from disk, so an identifier declared beside the edit still resolves. Imports are
// resolved from source for the standard library; a package that imports anything else cannot be
// type-checked here, and the validator degrades to syntax-only, reporting that it did.
func GoTypes() Validator {
	return func(files map[string][]byte) []Diagnostic {
		dirs := map[string]bool{}
		for path := range files {
			if strings.HasSuffix(path, ".go") {
				dirs[filepath.Dir(path)] = true
			}
		}
		ordered := make([]string, 0, len(dirs))
		for dir := range dirs {
			ordered = append(ordered, dir)
		}
		sort.Strings(ordered)
		var diags []Diagnostic
		for _, dir := range ordered {
			diags = append(diags, checkGoPackage(dir, files)...)
		}
		sortDiagnostics(diags)
		return diags
	}
}

// checkGoPackage parses and type checks one directory's package with the planned content in place.
func checkGoPackage(dir string, planned map[string][]byte) []Diagnostic {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []Diagnostic{{Severity: SeverityWarning, Code: CodeSyntax, Path: dir, Text: fmt.Sprintf("package directory unreadable: %v", err)}}
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") ||
			strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
			continue
		}
		paths = append(paths, filepath.Join(dir, name))
	}
	sort.Strings(paths)
	fset := token.NewFileSet()
	var astFiles []*ast.File
	fileNames := make([]string, 0, len(paths))
	var diags []Diagnostic
	pkgName := ""
	for _, path := range paths {
		content, ok := planned[canonicalPath(path)]
		if !ok {
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				continue
			}
			content = raw
		}
		file, parseErr := parser.ParseFile(fset, path, content, parser.ParseComments|parser.AllErrors)
		if parseErr != nil {
			diags = append(diags, parseDiagnostics(parseErr, CodeSyntax)...)
			continue
		}
		if pkgName == "" {
			pkgName = file.Name.Name
		} else if file.Name.Name != pkgName {
			continue
		}
		astFiles = append(astFiles, file)
		fileNames = append(fileNames, path)
	}
	if len(diags) > 0 || len(astFiles) == 0 {
		return diags
	}
	// Only the standard library can be imported from source here; anything else would resolve to an
	// empty package and produce errors that are artifacts of the missing dependency, not of the edit.
	var unresolved []string
	for _, file := range astFiles {
		for _, spec := range file.Imports {
			path := strings.Trim(spec.Path.Value, `"`)
			if !standardImport(path) {
				unresolved = append(unresolved, path)
			}
		}
	}
	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		return append(diags, Diagnostic{Severity: SeverityWarning, Code: CodeTypeCheckDegraded,
			Text: "type check degraded to syntax-only: cannot resolve imports " + strings.Join(uniqueStrings(unresolved), ", ")})
	}
	typeErrors := make([]Diagnostic, 0, 4)
	config := types.Config{
		Importer:    importer.ForCompiler(fset, "source", nil),
		FakeImportC: true,
		Error: func(err error) {
			typeErrors = append(typeErrors, typeDiagnostic(err)...)
		},
	}
	if _, err := config.Check(pkgName, fset, astFiles, nil); err != nil && len(typeErrors) == 0 {
		typeErrors = append(typeErrors, Diagnostic{Severity: SeverityError, Code: CodeType, Text: err.Error()})
	}
	return append(diags, typeErrors...)
}

// parseDiagnostics converts a parser error (a scanner.ErrorList) into per-position findings.
func parseDiagnostics(err error, code string) []Diagnostic {
	list, ok := err.(scanner.ErrorList)
	if !ok {
		return []Diagnostic{{Severity: SeverityError, Code: code, Text: err.Error()}}
	}
	out := make([]Diagnostic, 0, len(list))
	for _, item := range list {
		out = append(out, Diagnostic{Severity: SeverityError, Code: code, Path: item.Pos.Filename,
			Line: item.Pos.Line, Column: item.Pos.Column, Text: item.Msg})
	}
	return out
}

func typeDiagnostic(err error) []Diagnostic {
	if typeErr, ok := err.(types.Error); ok {
		position := typeErr.Fset.Position(typeErr.Pos)
		return []Diagnostic{{Severity: SeverityError, Code: CodeType, Path: position.Filename,
			Line: position.Line, Column: position.Column, Text: typeErr.Msg}}
	}
	return []Diagnostic{{Severity: SeverityError, Code: CodeType, Text: err.Error()}}
}

// standardImport reports whether an import path is served from the standard library, which the source
// importer can load from GOROOT. A path whose first element has a dot is a module path.
func standardImport(path string) bool {
	if path == "C" {
		return true
	}
	if path == "" {
		return false
	}
	first, _, _ := strings.Cut(path, "/")
	return !strings.Contains(first, ".")
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := in[:0]
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func sortDiagnostics(diags []Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Path != diags[j].Path {
			return diags[i].Path < diags[j].Path
		}
		if diags[i].Line != diags[j].Line {
			return diags[i].Line < diags[j].Line
		}
		return diags[i].Text < diags[j].Text
	})
}
