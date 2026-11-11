// Package goedit performs structural edits on a Go package. go/parser and go/ast locate the target —
// a function, a type, a parameter, a field, an import — and the edit lands as a byte splice on the
// exact bytes that were parsed, so comments, blank lines and formatting outside the target survive.
// A scope-aware rename uses go/ast's own object resolution, so it never renames a shadowed name and
// refuses a name that would be captured. Nothing here writes: the caller commits the resulting
// editing.FileChange values through editing.Engine, which validates and applies them atomically.
package goedit

import (
	"bytes"
	"fmt"
	"go/ast"
	"go/format"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/edynasty/LumenCortex/internal/editing"
)

// Source reads a package's files. The toolset backs it with the workspace-guarded reader.
type Source interface {
	Read(path string) ([]byte, error)
	List(dir string) ([]string, error)
}

// Operation kinds.
const (
	OpFind          = "find"
	OpReplaceBody   = "replace_body"
	OpReplaceDecl   = "replace_decl"
	OpAddParam      = "add_param"
	OpRemoveParam   = "remove_param"
	OpRenameParam   = "rename_param"
	OpRename        = "rename"
	OpAddMember     = "add_member"
	OpRemoveMember  = "remove_member"
	OpReplaceMember = "replace_member"
	OpAddImport     = "add_import"
	OpRemoveImport  = "remove_import"
)

// Op is one structural edit.
type Op struct {
	Kind       string `json:"op"`
	Symbol     string `json:"symbol,omitempty"`
	Name       string `json:"name,omitempty"`
	NewName    string `json:"new_name,omitempty"`
	Text       string `json:"text,omitempty"`
	ImportPath string `json:"import_path,omitempty"`
}

// Symbol is one declaration a call located or edited.
type Symbol struct {
	Symbol string `json:"symbol"`
	Kind   string `json:"kind"`
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
}

// Result is one call's plan: the files whose content changes, and the symbols it touched.
type Result struct {
	Changes []editing.FileChange `json:"changes"`
	Symbols []Symbol             `json:"symbols,omitempty"`
	Notes   []string             `json:"notes,omitempty"`
}

// Plan resolves ops against the package that holds target and returns the file changes they make.
// Every change carries the revision of the bytes the edit was computed from, so the commit refuses a
// file that changed in between. Ops apply in order; each is resolved against the content the previous
// one produced.
func Plan(src Source, target string, ops []Op) (Result, error) {
	if len(ops) == 0 {
		return Result{}, editing.Errorf(editing.CodeInvalid, "ops must contain at least one operation")
	}
	dir := filepath.Dir(target)
	files, err := src.List(dir)
	if err != nil {
		return Result{}, err
	}
	disk := make(map[string][]byte, len(files))
	for _, path := range files {
		raw, readErr := src.Read(path)
		if readErr != nil {
			continue
		}
		disk[path] = raw
	}
	original, ok := disk[target]
	if !ok {
		raw, readErr := src.Read(target)
		if readErr != nil {
			return Result{}, readErr
		}
		disk[target] = raw
		original = raw
	}
	content := make(map[string][]byte, len(disk))
	for path, raw := range disk {
		content[path] = append([]byte(nil), raw...)
	}
	var symbols []Symbol
	var notes []string
	for index, op := range ops {
		p, err := load(src, dir, target, content)
		if err != nil {
			return Result{}, err
		}
		splices, found, note, err := p.apply(op)
		if err != nil {
			return Result{}, opError(index, op, err)
		}
		symbols = append(symbols, found...)
		if note != "" {
			notes = append(notes, fmt.Sprintf("op %d: %s", index+1, note))
		}
		if err := applySplices(content, splices); err != nil {
			return Result{}, opError(index, op, err)
		}
	}
	paths := make([]string, 0, len(content))
	for path := range content {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	result := Result{Symbols: symbols, Notes: notes}
	for _, path := range paths {
		if bytes.Equal(content[path], disk[path]) {
			continue
		}
		next := content[path]
		result.Changes = append(result.Changes, editing.FileChange{
			Path:    path,
			BaseRev: editing.RevOf(disk[path]),
			Content: &next,
		})
	}
	if len(result.Changes) == 0 {
		notes = append(notes, "no file changed")
	}
	_ = original
	result.Notes = notes
	return result, nil
}

func opError(index int, op Op, err error) error {
	if editErr, ok := editing.AsError(err); ok {
		editErr.Detail = fmt.Sprintf("op %d (%s): %s", index+1, op.Kind, editErr.Detail)
		return editErr
	}
	return editing.Errorf(editing.CodeInvalid, "op %d (%s): %v", index+1, op.Kind, err)
}

// splice replaces [start, end) of one file's bytes.
type splice struct {
	file       string
	start, end int
	text       string
}

// applySplices applies one op's splices to the in-memory content, grouped per file.
func applySplices(content map[string][]byte, splices []splice) error {
	byFile := map[string][]editing.Splice{}
	for _, item := range splices {
		byFile[item.file] = append(byFile[item.file], editing.Splice{Start: item.start, End: item.end, Text: item.text})
	}
	files := make([]string, 0, len(byFile))
	for file := range byFile {
		files = append(files, file)
	}
	sort.Strings(files)
	for _, file := range files {
		list := byFile[file]
		if err := editing.ValidateSplices(list); err != nil {
			return err
		}
		next, err := editing.ApplySplices(editing.DecodeDoc(content[file]), list)
		if err != nil {
			return err
		}
		content[file] = next
	}
	return nil
}

// pkg is a parsed package: every file of the directory, one FileSet, and the package scope go/ast's
// resolver built across them.
type pkg struct {
	fset   *token.FileSet
	dir    string
	target string
	name   string
	files  map[string]*ast.File
	src    map[string][]byte
	scope  *ast.Scope
}

func load(src Source, dir, target string, content map[string][]byte) (*pkg, error) {
	names, err := src.List(dir)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		names = []string{target}
	}
	names = append(names, target)
	sort.Strings(names)
	fset := token.NewFileSet()
	files := make(map[string]*ast.File, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		raw, ok := content[name]
		if !ok {
			raw, err = src.Read(name)
			if err != nil {
				continue
			}
		}
		file, parseErr := parser.ParseFile(fset, name, raw, parser.ParseComments)
		if parseErr != nil {
			return nil, editing.Errorf(editing.CodeSyntax, "%s: %v", filepath.Base(name), parseErr)
		}
		files[name] = file
	}
	targetFile, ok := files[target]
	if !ok {
		return nil, editing.Errorf(editing.CodeInvalid, "%s is not a Go file of its package", target)
	}
	// Package objects resolve across files; an import resolves to an empty package object, because the
	// editor never needs the dependency's contents.
	importer := func(imports map[string]*ast.Object, path string) (*ast.Object, error) {
		if obj, ok := imports[path]; ok {
			return obj, nil
		}
		obj := ast.NewObj(ast.Pkg, filepath.Base(path))
		obj.Data = ast.NewScope(nil)
		imports[path] = obj
		return obj, nil
	}
	parsed, err := ast.NewPackage(fset, files, importer, nil)
	if err != nil && parsed == nil {
		return nil, editing.Errorf(editing.CodeInvalid, "cannot resolve the package: %v", err)
	}
	p := &pkg{fset: fset, dir: dir, target: target, name: targetFile.Name.Name, files: files, src: content}
	if parsed != nil {
		p.scope = parsed.Scope
	} else {
		p.scope = ast.NewScope(nil)
	}
	return p, nil
}

// offset is the byte offset in the file of a position.
func (p *pkg) offset(pos token.Pos) int { return p.fset.Position(pos).Offset }

// fileOf is the file a position belongs to.
func (p *pkg) fileOf(pos token.Pos) string { return p.fset.Position(pos).Filename }

// lineColumn is the 1-based line and column of a position.
func (p *pkg) lineColumn(pos token.Pos) (int, int) {
	position := p.fset.Position(pos)
	return position.Line, position.Column
}

// isTypeName reports whether name is a type declared in this package.
func (p *pkg) isTypeName(name string) bool {
	obj := p.scope.Lookup(strings.TrimPrefix(name, "*"))
	return obj != nil && obj.Kind == ast.Typ
}

// splitSymbol splits "(*T).M", "T.M", "pkg.Name" or "Name" into a receiver and a name. An empty
// receiver means a package-level name.
func splitSymbol(spec string) (recv, name string) {
	spec = strings.TrimSpace(spec)
	if strings.HasPrefix(spec, "(") {
		if end := strings.Index(spec, ")"); end > 0 {
			recv = strings.TrimSuffix(strings.TrimPrefix(spec[:end], "("), "")
			rest := strings.TrimPrefix(spec[end+1:], ".")
			return strings.TrimSpace(recv), strings.TrimSpace(rest)
		}
	}
	if first, rest, ok := strings.Cut(spec, "."); ok && !strings.Contains(rest, ".") {
		return strings.TrimSpace(first), strings.TrimSpace(rest)
	}
	return "", spec
}

// resolveFunc finds the one function or method the symbol names.
func (p *pkg) resolveFunc(spec string) (*ast.FuncDecl, string, error) {
	recv, name := splitSymbol(spec)
	if recv != "" && !p.isTypeName(recv) {
		if recv == p.name {
			recv = ""
		} else {
			return nil, "", editing.Errorf(editing.CodeUnknownSymbol,
				"%q: %q is not a type of package %s; write (*T).M, T.M or Name", spec, recv, p.name)
		}
	}
	if name == "" {
		return nil, "", editing.Errorf(editing.CodeInvalid, "%q names no function", spec)
	}
	var found []*ast.FuncDecl
	var where []string
	for _, file := range p.sortedFiles() {
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Name.Name != name {
				continue
			}
			if (recv == "") != (fd.Recv == nil) {
				continue
			}
			if fd.Recv != nil && len(fd.Recv.List) == 1 && !receiverMatches(fd.Recv.List[0].Type, recv) {
				continue
			}
			found = append(found, fd)
			line, column := p.lineColumn(fd.Name.Pos())
			where = append(where, fmt.Sprintf("%s:%d:%d", filepath.Base(p.fileOf(fd.Name.Pos())), line, column))
		}
	}
	switch len(found) {
	case 1:
		return found[0], p.fileOf(found[0].Name.Pos()), nil
	case 0:
		return nil, "", editing.Errorf(editing.CodeUnknownSymbol, "no declaration of %q in package %s", spec, p.name)
	default:
		return nil, "", editing.Errorf(editing.CodeAmbiguous, "%q resolves to %d declarations: %s", spec, len(found), strings.Join(where, ", "))
	}
}

// resolveType finds the one type the symbol names.
func (p *pkg) resolveType(spec string) (*ast.TypeSpec, string, error) {
	recv, name := splitSymbol(spec)
	if recv != "" && recv != p.name {
		return nil, "", editing.Errorf(editing.CodeInvalid, "%q names a method or field, not a type", spec)
	}
	var found []*ast.TypeSpec
	var where []string
	for _, file := range p.sortedFiles() {
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, item := range gen.Specs {
				ts, ok := item.(*ast.TypeSpec)
				if !ok || ts.Name.Name != name {
					continue
				}
				found = append(found, ts)
				line, column := p.lineColumn(ts.Name.Pos())
				where = append(where, fmt.Sprintf("%s:%d:%d", filepath.Base(p.fileOf(ts.Name.Pos())), line, column))
			}
		}
	}
	switch len(found) {
	case 1:
		return found[0], p.fileOf(found[0].Name.Pos()), nil
	case 0:
		return nil, "", editing.Errorf(editing.CodeUnknownSymbol, "no type %q in package %s", name, p.name)
	default:
		return nil, "", editing.Errorf(editing.CodeAmbiguous, "type %q resolves to %d declarations: %s", name, len(found), strings.Join(where, ", "))
	}
}

func receiverMatches(expr ast.Expr, recv string) bool {
	recv = strings.TrimPrefix(strings.TrimSpace(recv), "*")
	switch typed := expr.(type) {
	case *ast.StarExpr:
		return receiverMatches(typed.X, recv) || recv == strings.TrimPrefix(exprString(typed), "*")
	case *ast.Ident:
		return typed.Name == recv
	case *ast.IndexExpr:
		return receiverMatches(typed.X, recv)
	case *ast.IndexListExpr:
		return receiverMatches(typed.X, recv)
	}
	return exprString(expr) == recv
}

// exprString renders the little of an expression a symbol path or a member name needs.
func exprString(expr ast.Expr) string {
	var b bytes.Buffer
	if err := formatNode(&b, expr); err != nil {
		return ""
	}
	return b.String()
}

// sortedFiles returns the package files in a deterministic order.
func (p *pkg) sortedFiles() []*ast.File {
	names := make([]string, 0, len(p.files))
	for name := range p.files {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]*ast.File, 0, len(names))
	for _, name := range names {
		out = append(out, p.files[name])
	}
	return out
}

// apply runs one operation and returns its splices.
func (p *pkg) apply(op Op) ([]splice, []Symbol, string, error) {
	switch strings.TrimSpace(op.Kind) {
	case OpFind:
		symbols, err := p.find(op.Symbol)
		return nil, symbols, "", err
	case OpReplaceBody:
		splices, symbols, err := p.replaceBody(op)
		return splices, symbols, "", err
	case OpReplaceDecl:
		splices, symbols, err := p.replaceDecl(op)
		return splices, symbols, "", err
	case OpAddParam:
		splices, symbols, err := p.addParam(op)
		return splices, symbols, "", err
	case OpRemoveParam:
		splices, symbols, err := p.removeParam(op)
		return splices, symbols, "", err
	case OpRenameParam:
		splices, symbols, err := p.renameParam(op)
		return splices, symbols, "", err
	case OpRename:
		splices, symbols, note, err := p.rename(op)
		return splices, symbols, note, err
	case OpAddMember, OpRemoveMember, OpReplaceMember:
		splices, symbols, err := p.member(op)
		return splices, symbols, "", err
	case OpAddImport:
		splices, symbols, note, err := p.addImport(op)
		return splices, symbols, note, err
	case OpRemoveImport:
		splices, symbols, note, err := p.removeImport(op)
		return splices, symbols, note, err
	}
	return nil, nil, "", editing.Errorf(editing.CodeInvalid, "unknown op %q", op.Kind)
}

// find reports where a symbol is declared.
func (p *pkg) find(spec string) ([]Symbol, error) {
	if fd, _, err := p.resolveFunc(spec); err == nil {
		line, column := p.lineColumn(fd.Name.Pos())
		return []Symbol{{Symbol: qualified(p.name, spec), Kind: funcKind(fd), Path: p.fileOf(fd.Name.Pos()), Line: line, Column: column}}, nil
	}
	if ts, _, err := p.resolveType(spec); err == nil {
		line, column := p.lineColumn(ts.Name.Pos())
		return []Symbol{{Symbol: qualified(p.name, spec), Kind: "type", Path: p.fileOf(ts.Name.Pos()), Line: line, Column: column}}, nil
	}
	_, name := splitSymbol(spec)
	if obj := p.scope.Lookup(name); obj != nil {
		pos := obj.Pos()
		line, column := p.lineColumn(pos)
		return []Symbol{{Symbol: qualified(p.name, spec), Kind: objectKind(obj), Path: p.fileOf(pos), Line: line, Column: column}}, nil
	}
	return nil, editing.Errorf(editing.CodeUnknownSymbol, "no symbol %q in package %s", spec, p.name)
}

func funcKind(fd *ast.FuncDecl) string {
	if fd.Recv != nil {
		return "method"
	}
	return "func"
}

func objectKind(obj *ast.Object) string {
	switch obj.Kind {
	case ast.Typ:
		return "type"
	case ast.Var:
		return "var"
	case ast.Con:
		return "const"
	case ast.Fun:
		return "func"
	}
	return "object"
}

func qualified(pkgName, spec string) string {
	if strings.Contains(spec, ".") && strings.HasPrefix(spec, pkgName+".") {
		return spec
	}
	return pkgName + "." + spec
}

// replaceBody replaces a function's body, addressed by its symbol path.
func (p *pkg) replaceBody(op Op) ([]splice, []Symbol, error) {
	fd, file, err := p.resolveFunc(op.Symbol)
	if err != nil {
		return nil, nil, err
	}
	if fd.Body == nil {
		return nil, nil, editing.Errorf(editing.CodeInvalid, "%s has no body", op.Symbol)
	}
	if strings.TrimSpace(op.Text) == "" {
		return nil, nil, editing.Errorf(editing.CodeInvalid, "replace_body needs text")
	}
	text := op.Text
	if !strings.HasPrefix(strings.TrimSpace(text), "{") {
		text = "{\n" + strings.TrimRight(text, "\n") + "\n}"
	}
	start, end := p.offset(fd.Body.Lbrace), p.offset(fd.Body.Rbrace)+1
	line, column := p.lineColumn(fd.Name.Pos())
	return []splice{{file: file, start: start, end: end, text: text}},
		[]Symbol{{Symbol: op.Symbol, Kind: funcKind(fd), Path: file, Line: line, Column: column}}, nil
}

// replaceDecl replaces a whole function or type declaration.
func (p *pkg) replaceDecl(op Op) ([]splice, []Symbol, error) {
	if fd, file, err := p.resolveFunc(op.Symbol); err == nil {
		splices := []splice{{file: file, start: p.offset(fd.Pos()), end: p.offset(fd.End()), text: op.Text}}
		line, column := p.lineColumn(fd.Name.Pos())
		return splices, []Symbol{{Symbol: op.Symbol, Kind: funcKind(fd), Path: file, Line: line, Column: column}}, nil
	}
	ts, file, err := p.resolveType(op.Symbol)
	if err != nil {
		return nil, nil, err
	}
	splices := []splice{{file: file, start: p.offset(ts.Pos()), end: p.offset(ts.End()), text: op.Text}}
	line, column := p.lineColumn(ts.Name.Pos())
	return splices, []Symbol{{Symbol: op.Symbol, Kind: "type", Path: file, Line: line, Column: column}}, nil
}

// addParam appends a parameter to a function's signature.
func (p *pkg) addParam(op Op) ([]splice, []Symbol, error) {
	fd, file, err := p.resolveFunc(op.Symbol)
	if err != nil {
		return nil, nil, err
	}
	if op.Name == "" || strings.TrimSpace(op.Text) == "" {
		return nil, nil, editing.Errorf(editing.CodeInvalid, "add_param needs name and text (the type)")
	}
	if !token.IsIdentifier(op.Name) {
		return nil, nil, editing.Errorf(editing.CodeInvalid, "%q is not an identifier", op.Name)
	}
	if params := fd.Type.Params; params != nil && len(params.List) > 0 {
		last := params.List[len(params.List)-1]
		if _, variadic := last.Type.(*ast.Ellipsis); variadic {
			return nil, nil, editing.Errorf(editing.CodeInvalid, "cannot add a parameter after a variadic one")
		}
		for _, field := range params.List {
			for _, name := range field.Names {
				if name.Name == op.Name {
					return nil, nil, editing.Errorf(editing.CodeAmbiguous, "%s already has a parameter %q", op.Symbol, op.Name)
				}
			}
		}
	}
	decl := op.Name + " " + strings.TrimSpace(op.Text)
	insert := decl
	if fd.Type.Params != nil && len(fd.Type.Params.List) > 0 {
		insert = ", " + decl
	}
	at := p.offset(fd.Type.Params.Closing)
	line, column := p.lineColumn(fd.Name.Pos())
	return []splice{{file: file, start: at, end: at, text: insert}},
		[]Symbol{{Symbol: op.Symbol, Kind: funcKind(fd), Path: file, Line: line, Column: column}}, nil
}

// removeParam removes one parameter from a function's signature.
func (p *pkg) removeParam(op Op) ([]splice, []Symbol, error) {
	fd, file, err := p.resolveFunc(op.Symbol)
	if err != nil {
		return nil, nil, err
	}
	params := fd.Type.Params
	if params == nil {
		return nil, nil, editing.Errorf(editing.CodeUnknownSymbol, "%s has no parameters", op.Symbol)
	}
	for fieldIndex, field := range params.List {
		for nameIndex, name := range field.Names {
			if name.Name != op.Name {
				continue
			}
			start, end := p.paramRemoval(params.List, fieldIndex, nameIndex)
			line, column := p.lineColumn(fd.Name.Pos())
			return []splice{{file: file, start: start, end: end}},
				[]Symbol{{Symbol: op.Symbol, Kind: "param", Path: file, Line: line, Column: column}}, nil
		}
	}
	return nil, nil, editing.Errorf(editing.CodeUnknownSymbol, "%s has no parameter %q", op.Symbol, op.Name)
}

// paramRemoval computes the byte range that removes one name from a parameter list, with exactly one
// separator, so the remaining parameters stay valid and their text is untouched.
func (p *pkg) paramRemoval(fields []*ast.Field, fieldIndex, nameIndex int) (int, int) {
	field := fields[fieldIndex]
	if len(field.Names) > 1 {
		if nameIndex < len(field.Names)-1 {
			return p.offset(field.Names[nameIndex].Pos()), p.offset(field.Names[nameIndex+1].Pos())
		}
		return p.offset(field.Names[nameIndex-1].End()), p.offset(field.Names[nameIndex].End())
	}
	if fieldIndex < len(fields)-1 {
		return p.offset(field.Pos()), p.offset(fields[fieldIndex+1].Pos())
	}
	if fieldIndex > 0 {
		return p.offset(fields[fieldIndex-1].End()), p.offset(field.End())
	}
	return p.offset(field.Pos()), p.offset(field.End())
}

// renameParam renames a parameter and exactly its uses.
func (p *pkg) renameParam(op Op) ([]splice, []Symbol, error) {
	if !token.IsIdentifier(op.NewName) {
		return nil, nil, editing.Errorf(editing.CodeInvalid, "%q is not an identifier", op.NewName)
	}
	fd, _, err := p.resolveFunc(op.Symbol)
	if err != nil {
		return nil, nil, err
	}
	obj := paramObject(fd, op.Name)
	if obj == nil {
		return nil, nil, editing.Errorf(editing.CodeUnknownSymbol, "%s has no parameter %q", op.Symbol, op.Name)
	}
	if op.Name == op.NewName {
		return nil, nil, nil
	}
	if err := p.refuseCapture(fd, obj, op.NewName); err != nil {
		return nil, nil, err
	}
	var splices []splice
	var symbols []Symbol
	for _, file := range p.sortedFiles() {
		walkIdents(file, func(_ ast.Node, id *ast.Ident) {
			if id.Obj != obj {
				return
			}
			line, column := p.lineColumn(id.Pos())
			splices = append(splices, splice{file: p.fileOf(id.Pos()), start: p.offset(id.Pos()), end: p.offset(id.End()), text: op.NewName})
			symbols = append(symbols, Symbol{Symbol: op.Symbol + "." + op.Name, Kind: "param", Path: p.fileOf(id.Pos()), Line: line, Column: column})
		})
	}
	return splices, symbols, nil
}

// paramObject returns the object a parameter name declares.
func paramObject(fd *ast.FuncDecl, name string) *ast.Object {
	if fd.Type.Params == nil {
		return nil
	}
	for _, field := range fd.Type.Params.List {
		for _, ident := range field.Names {
			if ident.Name == name {
				return ident.Obj
			}
		}
	}
	return nil
}

// refuseCapture refuses a rename whose new name already means something inside the function: the
// renamed parameter would capture that name.
func (p *pkg) refuseCapture(fd *ast.FuncDecl, obj *ast.Object, to string) error {
	found := ""
	walkIdents(fd, func(parent ast.Node, id *ast.Ident) {
		if found != "" || id.Name != to || id.Obj == obj {
			return
		}
		// A selector's field name and a label are not names the parameter could capture.
		if selector, ok := parent.(*ast.SelectorExpr); ok && selector.Sel == id {
			return
		}
		if _, ok := parent.(*ast.LabeledStmt); ok {
			return
		}
		line, column := p.lineColumn(id.Pos())
		found = fmt.Sprintf("%s:%d:%d", filepath.Base(p.fileOf(id.Pos())), line, column)
	})
	if found != "" {
		return editing.Errorf(editing.CodeAmbiguous, "rename would capture existing %q (%s)", to, found)
	}
	return nil
}

// rename renames a package-level object, a method, a field or a parameter.
func (p *pkg) rename(op Op) ([]splice, []Symbol, string, error) {
	if !token.IsIdentifier(op.NewName) {
		return nil, nil, "", editing.Errorf(editing.CodeInvalid, "%q is not an identifier", op.NewName)
	}
	if op.Symbol == "" {
		return nil, nil, "", editing.Errorf(editing.CodeInvalid, "rename needs symbol")
	}
	if fd, _, err := p.resolveFunc(op.Symbol); err == nil {
		if fd.Recv != nil {
			return p.renameSelector(op, fd, "method")
		}
		obj := fd.Name.Obj
		if obj == nil {
			return nil, nil, "", editing.Errorf(editing.CodeUnknownSymbol, "%s has no resolved declaration", op.Symbol)
		}
		return p.renameObject(op, obj)
	}
	if ts, _, err := p.resolveType(op.Symbol); err == nil {
		obj := ts.Name.Obj
		if obj == nil {
			return nil, nil, "", editing.Errorf(editing.CodeUnknownSymbol, "%s has no resolved declaration", op.Symbol)
		}
		return p.renameObject(op, obj)
	}
	_, name := splitSymbol(op.Symbol)
	obj := p.scope.Lookup(name)
	if obj == nil {
		return nil, nil, "", editing.Errorf(editing.CodeUnknownSymbol, "no package-level symbol %q", op.Symbol)
	}
	return p.renameObject(op, obj)
}

// renameObject renames a package-level object and every identifier that resolves to it, across files.
func (p *pkg) renameObject(op Op, obj *ast.Object) ([]splice, []Symbol, string, error) {
	if existing := p.scope.Lookup(op.NewName); existing != nil && existing != obj {
		line, column := p.lineColumn(existing.Pos())
		return nil, nil, "", editing.Errorf(editing.CodeAmbiguous, "package %s already declares %q (%s:%d:%d)",
			p.name, op.NewName, filepath.Base(p.fileOf(existing.Pos())), line, column)
	}
	if builtin := types.Universe.Lookup(op.NewName); builtin != nil {
		return nil, nil, "", editing.Errorf(editing.CodeAmbiguous, "rename would shadow universe declaration %q", builtin.Name())
	}
	info, err := p.typeInfo()
	if err != nil {
		return nil, nil, "", err
	}
	for id, used := range info.Uses {
		if id.Obj != obj {
			continue
		}
		var inner *types.Scope
		for _, scope := range info.Scopes {
			if scope.Contains(id.Pos()) && (inner == nil || scope.Pos() >= inner.Pos() && scope.End() <= inner.End()) {
				inner = scope
			}
		}
		if inner != nil {
			_, captured := inner.LookupParent(op.NewName, id.Pos())
			if captured != nil && captured != used {
				return nil, nil, "", editing.Errorf(editing.CodeAmbiguous, "rename would capture existing %q at %s", op.NewName, p.fset.Position(id.Pos()))
			}
		}
	}
	var splices []splice
	var symbols []Symbol
	for _, file := range p.sortedFiles() {
		walkIdents(file, func(_ ast.Node, id *ast.Ident) {
			if id.Obj != obj {
				return
			}
			line, column := p.lineColumn(id.Pos())
			splices = append(splices, splice{file: p.fileOf(id.Pos()), start: p.offset(id.Pos()), end: p.offset(id.End()), text: op.NewName})
			symbols = append(symbols, Symbol{Symbol: obj.Name, Kind: objectKind(obj), Path: p.fileOf(id.Pos()), Line: line, Column: column})
		})
	}
	if len(splices) == 0 {
		return nil, nil, "", editing.Errorf(editing.CodeUnknownSymbol, "no occurrence of %q resolved", op.Symbol)
	}
	return splices, symbols, fmt.Sprintf("renamed %d occurrence(s) of %s", len(splices), obj.Name), nil
}

// renameSelector resolves receiver identity; unresolved packages are refused rather
// than falling back to selector spelling.
func (p *pkg) renameSelector(op Op, fd *ast.FuncDecl, kind string) ([]splice, []Symbol, string, error) {
	info, err := p.typeInfo()
	if err != nil {
		return nil, nil, "", err
	}
	target, ok := info.Defs[fd.Name].(*types.Func)
	if !ok {
		return nil, nil, "", editing.Errorf(editing.CodeUnknownSymbol, "method identity is unresolved")
	}
	sig := target.Type().(*types.Signature)
	if existing, _, _ := types.LookupFieldOrMethod(sig.Recv().Type(), true, target.Pkg(), op.NewName); existing != nil && existing != target {
		return nil, nil, "", editing.Errorf(editing.CodeAmbiguous, "receiver already has member %q", op.NewName)
	}
	if err := rejectReverseMethodCapture(info, target, fd.Name.Name, op.NewName); err != nil {
		return nil, nil, "", err
	}
	for _, selection := range info.Selections {
		method, ok := selection.Obj().(*types.Func)
		if !ok || method.Origin() != target.Origin() {
			continue
		}
		if existing, index, _ := types.LookupFieldOrMethod(selection.Recv(), true, target.Pkg(), op.NewName); existing != nil || index != nil {
			return nil, nil, "", editing.Errorf(editing.CodeAmbiguous, "rename would rebind a promoted %s selection", kind)
		}
	}
	var splices []splice
	var symbols []Symbol
	for _, file := range p.sortedFiles() {
		walkIdents(file, func(_ ast.Node, id *ast.Ident) {
			obj := info.ObjectOf(id)
			method, ok := obj.(*types.Func)
			if !ok || method.Origin() != target.Origin() {
				return
			}
			line, column := p.lineColumn(id.Pos())
			splices = append(splices, splice{file: p.fileOf(id.Pos()), start: p.offset(id.Pos()), end: p.offset(id.End()), text: op.NewName})
			symbols = append(symbols, Symbol{Symbol: fd.Name.Name, Kind: kind, Path: p.fileOf(id.Pos()), Line: line, Column: column})
		})
	}
	return splices, symbols, fmt.Sprintf("renamed %d occurrence(s) of %s", len(splices), fd.Name.Name), nil
}
func rejectReverseMethodCapture(info *types.Info, target *types.Func, oldName, newName string) error {
	for _, object := range info.Defs {
		typeName, ok := object.(*types.TypeName)
		if !ok {
			continue
		}
		for _, candidate := range []types.Type{typeName.Type(), types.NewPointer(typeName.Type())} {
			old, _, _ := types.LookupFieldOrMethod(candidate, true, target.Pkg(), oldName)
			oldMethod, ok := old.(*types.Func)
			if !ok || oldMethod.Origin() != target.Origin() {
				continue
			}
			if replacement, index, _ := types.LookupFieldOrMethod(candidate, true, target.Pkg(), newName); replacement != nil || index != nil {
				return editing.Errorf(editing.CodeAmbiguous, "rename would rebind existing method selection %q", newName)
			}
		}
	}
	return nil
}
func (p *pkg) typeInfo() (*types.Info, error) {
	info := &types.Info{
		Defs: make(map[*ast.Ident]types.Object), Uses: make(map[*ast.Ident]types.Object),
		Scopes: make(map[ast.Node]*types.Scope), Selections: make(map[*ast.SelectorExpr]*types.Selection),
	}
	config := types.Config{Importer: importer.ForCompiler(p.fset, "source", nil)}
	if _, err := config.Check(p.name, p.fset, p.sortedFiles(), info); err != nil {
		return nil, editing.Errorf(editing.CodeType, "rename requires resolved types: %v", err)
	}
	return info, nil
}

// member declares the member name of a struct or interface type, or "" when it has none.
func memberName(expr ast.Expr, want string) string {
	var list *ast.FieldList
	switch typed := expr.(type) {
	case *ast.StructType:
		list = typed.Fields
	case *ast.InterfaceType:
		list = typed.Methods
	}
	if list == nil {
		return ""
	}
	for _, field := range list.List {
		if fieldName(field) == want {
			return want
		}
	}
	return ""
}

// member adds, removes or replaces one field or method of a struct or interface type.
func (p *pkg) member(op Op) ([]splice, []Symbol, error) {
	ts, file, err := p.resolveType(op.Symbol)
	if err != nil {
		return nil, nil, err
	}
	var list *ast.FieldList
	switch typed := ts.Type.(type) {
	case *ast.StructType:
		list = typed.Fields
	case *ast.InterfaceType:
		list = typed.Methods
	default:
		return nil, nil, editing.Errorf(editing.CodeInvalid, "%s is not a struct or interface type", op.Symbol)
	}
	line, column := p.lineColumn(ts.Name.Pos())
	symbol := Symbol{Symbol: op.Symbol, Kind: "type", Path: file, Line: line, Column: column}
	switch op.Kind {
	case OpAddMember:
		if strings.TrimSpace(op.Text) == "" {
			return nil, nil, editing.Errorf(editing.CodeInvalid, "add_member needs text (the declaration)")
		}
		if op.Name != "" && memberName(ts.Type, op.Name) != "" {
			return nil, nil, editing.Errorf(editing.CodeAmbiguous, "%s already has a member %q", op.Symbol, op.Name)
		}
		return appendMember(p, file, list, op.Text), []Symbol{symbol}, nil
	case OpRemoveMember, OpReplaceMember:
		for index, field := range list.List {
			if fieldName(field) != op.Name {
				continue
			}
			if op.Kind == OpReplaceMember {
				if strings.TrimSpace(op.Text) == "" {
					return nil, nil, editing.Errorf(editing.CodeInvalid, "replace_member needs text")
				}
				return []splice{{file: file, start: p.offset(field.Pos()), end: p.offset(field.End()), text: op.Text}}, []Symbol{symbol}, nil
			}
			start, end := p.paramRemoval(list.List, index, 0)
			return []splice{{file: file, start: start, end: end}}, []Symbol{symbol}, nil
		}
		return nil, nil, editing.Errorf(editing.CodeUnknownSymbol, "%s has no member %q", op.Symbol, op.Name)
	}
	return nil, nil, editing.Errorf(editing.CodeInvalid, "unknown member op %q", op.Kind)
}

// appendMember inserts a member declaration on its own line before the type's closing brace.
func appendMember(p *pkg, file string, list *ast.FieldList, text string) []splice {
	brace := p.offset(list.Closing)
	src := p.src[file]
	lineStart := bytes.LastIndexByte(src[:brace], '\n') + 1
	indent := string(src[lineStart:brace])
	if strings.TrimSpace(indent) != "" {
		return []splice{{file: file, start: brace, end: brace, text: "\n\t" + text + "\n"}}
	}
	return []splice{{file: file, start: lineStart, end: lineStart, text: indent + "\t" + text + "\n"}}
}

// fieldName is the name a struct field or interface method is addressed by.
func fieldName(field *ast.Field) string {
	if len(field.Names) > 0 {
		return field.Names[0].Name
	}
	return strings.TrimPrefix(exprString(field.Type), "*")
}

// addImport adds an import to the target file, into the existing block when there is one.
func (p *pkg) addImport(op Op) ([]splice, []Symbol, string, error) {
	if strings.TrimSpace(op.ImportPath) == "" {
		return nil, nil, "", editing.Errorf(editing.CodeInvalid, "add_import needs import_path")
	}
	file := p.files[p.target]
	for _, spec := range file.Imports {
		if unquote(spec.Path.Value) == op.ImportPath {
			return nil, nil, fmt.Sprintf("%s is already imported", op.ImportPath), nil
		}
	}
	text := strconv.Quote(op.ImportPath)
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT || !gen.Lparen.IsValid() {
			continue
		}
		at := p.offset(gen.Rparen)
		return []splice{{file: p.target, start: at, end: at, text: "\t" + text + "\n"}}, nil, fmt.Sprintf("added import %s", op.ImportPath), nil
	}
	at := p.offset(file.Name.End())
	return []splice{{file: p.target, start: at, end: at, text: "\n\nimport " + text}}, nil, fmt.Sprintf("added import %s", op.ImportPath), nil
}

// removeImport removes every import of a path from the target file.
func (p *pkg) removeImport(op Op) ([]splice, []Symbol, string, error) {
	if strings.TrimSpace(op.ImportPath) == "" {
		return nil, nil, "", editing.Errorf(editing.CodeInvalid, "remove_import needs import_path")
	}
	file := p.files[p.target]
	src := p.src[p.target]
	var splices []splice
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT {
			continue
		}
		for _, item := range gen.Specs {
			spec, ok := item.(*ast.ImportSpec)
			if !ok || unquote(spec.Path.Value) != op.ImportPath {
				continue
			}
			if !gen.Lparen.IsValid() {
				start, end := lineRange(src, p.offset(gen.Pos()), p.offset(gen.End()))
				splices = append(splices, splice{file: p.target, start: start, end: end})
				continue
			}
			if len(gen.Specs) == 1 {
				start, end := lineRange(src, p.offset(gen.Pos()), p.offset(gen.End()))
				splices = append(splices, splice{file: p.target, start: start, end: end})
				continue
			}
			start, end := lineRange(src, p.offset(spec.Pos()), p.offset(spec.End()))
			splices = append(splices, splice{file: p.target, start: start, end: end})
		}
	}
	if len(splices) == 0 {
		return nil, nil, "", editing.Errorf(editing.CodeNotFound, "%s does not import %s", filepath.Base(p.target), op.ImportPath)
	}
	return splices, nil, fmt.Sprintf("removed import %s", op.ImportPath), nil
}

// lineRange widens [start, end) to whole lines, so removing a declaration leaves no blank line where
// its first line was.
func lineRange(src []byte, start, end int) (int, int) {
	lineStart := bytes.LastIndexByte(src[:start], '\n') + 1
	if strings.TrimSpace(string(src[lineStart:start])) != "" {
		lineStart = start
	}
	lineEnd := end
	for lineEnd < len(src) && src[lineEnd] != '\n' {
		lineEnd++
	}
	if lineEnd < len(src) {
		lineEnd++
	}
	return lineStart, lineEnd
}

func unquote(literal string) string {
	if value, err := strconv.Unquote(literal); err == nil {
		return value
	}
	return strings.Trim(literal, `"`)
}

// walkIdents visits every identifier of a tree with its parent node. go/ast's resolver records the
// object each identifier refers to, which is what makes a rename scope-aware.
func walkIdents(root ast.Node, visit func(parent ast.Node, id *ast.Ident)) {
	stack := make([]ast.Node, 0, 32)
	ast.Inspect(root, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, node)
		if id, ok := node.(*ast.Ident); ok && len(stack) >= 2 {
			visit(stack[len(stack)-2], id)
		}
		return true
	})
}

// formatNode prints an expression the way a symbol path spells it.
func formatNode(b *bytes.Buffer, expr ast.Expr) error {
	return format.Node(b, token.NewFileSet(), expr)
}
