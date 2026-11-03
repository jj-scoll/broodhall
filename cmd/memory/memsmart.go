package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	"github.com/smacker/go-tree-sitter/bash"
	"github.com/smacker/go-tree-sitter/golang"
	"github.com/smacker/go-tree-sitter/javascript"
	"github.com/smacker/go-tree-sitter/python"
	"github.com/smacker/go-tree-sitter/typescript/tsx"
	"github.com/smacker/go-tree-sitter/typescript/typescript"
)

// smart_* tools: local tree-sitter code navigation (no DB, no network). Argument names and
// output shape follow the smart_search / smart_outline / smart_unfold tool descriptions.
//
// Access rules (ruling R38): paths must resolve (symlinks
// followed) inside the workspace, CLAUDE_PROJECT_DIR else cwd. Files the secret guard
// protects (secretguard.go) are refused, so these tools cannot bypass the masked-read path.

const (
	smartMaxFileBytes = 1 << 20
	smartMaxFiles     = 5000
	smartDefaultHits  = 20
	smartUnfoldList   = 50
)

// smartSymbol is one outlined declaration. Rows are 0-based; docRow is the first row of
// the doc comment (== startRow when there is none).
type smartSymbol struct {
	name      string
	kind      string
	signature string
	doc       string
	exported  bool
	docRow    uint32
	startRow  uint32
	endRow    uint32
	children  []*smartSymbol
}

type smartFile struct {
	path       string
	language   string
	totalLines int
	imports    []string
	symbols    []*smartSymbol
}

type smartLang struct {
	name string
	lang func() *sitter.Language
	exts []string
}

var smartLangs = []smartLang{
	{"go", golang.GetLanguage, []string{".go"}},
	{"typescript", typescript.GetLanguage, []string{".ts", ".mts", ".cts"}},
	{"tsx", tsx.GetLanguage, []string{".tsx"}},
	{"javascript", javascript.GetLanguage, []string{".js", ".jsx", ".mjs", ".cjs"}},
	{"python", python.GetLanguage, []string{".py"}},
	{"bash", bash.GetLanguage, []string{".sh", ".bash"}},
}

func smartLangFor(path string) *smartLang {
	ext := strings.ToLower(filepath.Ext(path))
	for i := range smartLangs {
		for _, e := range smartLangs[i].exts {
			if e == ext {
				return &smartLangs[i]
			}
		}
	}
	return nil
}

func smartArg(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// smartParse outlines src. A nil result with nil error means the language is unsupported
// or the file has no symbols.
func smartParse(src []byte, path string) (*smartFile, error) {
	sl := smartLangFor(path)
	if sl == nil {
		return nil, nil
	}
	p := sitter.NewParser()
	defer p.Close()
	p.SetLanguage(sl.lang())
	tree, err := p.ParseCtx(context.Background(), nil, src)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	defer tree.Close()

	f := &smartFile{path: path, language: sl.name, totalLines: strings.Count(strings.TrimSuffix(string(src), "\n"), "\n") + 1}
	if len(src) == 0 {
		f.totalLines = 0
	}
	w := &smartWalker{src: src, f: f}
	root := tree.RootNode()
	switch sl.name {
	case "go":
		f.symbols = w.goSymbols(root)
	case "python":
		f.symbols = w.pySymbols(root, false)
	case "bash":
		f.symbols = w.bashSymbols(root)
	default:
		f.symbols = w.jsSymbols(root)
	}
	return f, nil
}

type smartWalker struct {
	src []byte
	f   *smartFile
}

func (w *smartWalker) text(n *sitter.Node) string {
	if n == nil {
		return ""
	}
	return n.Content(w.src)
}

func (w *smartWalker) field(n *sitter.Node, name string) string {
	return w.text(n.ChildByFieldName(name))
}

// header returns the source from n's start up to the start of its body, whitespace-collapsed.
func (w *smartWalker) header(n *sitter.Node, bodyField string) string {
	end := n.EndByte()
	if b := n.ChildByFieldName(bodyField); b != nil {
		end = b.StartByte()
	}
	s := string(w.src[n.StartByte():end])
	return strings.Join(strings.Fields(strings.TrimRight(strings.TrimSpace(s), "{")), " ")
}

func (w *smartWalker) firstLine(n *sitter.Node) string {
	s := w.text(n)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "{"))
}

// commentLine reduces a comment to its first meaningful line without markers.
func commentLine(c string) string {
	for _, ln := range strings.Split(c, "\n") {
		ln = strings.TrimSpace(ln)
		ln = strings.TrimPrefix(ln, "/**")
		ln = strings.TrimPrefix(ln, "/*")
		ln = strings.TrimPrefix(ln, "//")
		ln = strings.TrimPrefix(ln, "*")
		ln = strings.TrimSpace(strings.TrimSuffix(ln, "*/"))
		if ln != "" {
			return ln
		}
	}
	return ""
}

// docTracker collects the run of comment siblings directly above a declaration.
type docTracker struct {
	w     *smartWalker
	first *sitter.Node
	last  *sitter.Node
	lines []string
}

func (d *docTracker) add(c *sitter.Node) {
	if d.last != nil && c.StartPoint().Row > d.last.EndPoint().Row+1 {
		d.reset()
	}
	if d.first == nil {
		d.first = c
	}
	d.last = c
	d.lines = append(d.lines, commentLine(d.w.text(c)))
}

func (d *docTracker) reset() { d.first, d.last, d.lines = nil, nil, nil }

// take attaches the pending comment run to s when it ends on the row above n.
func (d *docTracker) take(n *sitter.Node, s *smartSymbol) {
	s.docRow = s.startRow
	if d.last != nil && d.last.EndPoint().Row+1 == n.StartPoint().Row {
		s.docRow = d.first.StartPoint().Row
		for _, l := range d.lines {
			if l != "" {
				s.doc = l
				break
			}
		}
	}
	d.reset()
}

func newSym(n *sitter.Node, name, kind, sig string) *smartSymbol {
	return &smartSymbol{name: name, kind: kind, signature: sig, startRow: n.StartPoint().Row, endRow: n.EndPoint().Row, docRow: n.StartPoint().Row}
}

var goRecvRe = regexp.MustCompile(`([A-Za-z_][A-Za-z0-9_]*)\s*(?:\[[^\]]*\])?\s*\)?\s*$`)

func (w *smartWalker) goSymbols(root *sitter.Node) []*smartSymbol {
	var out []*smartSymbol
	d := &docTracker{w: w}
	for i := 0; i < int(root.NamedChildCount()); i++ {
		n := root.NamedChild(i)
		var syms []*smartSymbol
		switch n.Type() {
		case "comment":
			d.add(n)
			continue
		case "import_declaration":
			w.f.imports = append(w.f.imports, strings.Join(strings.Fields(w.text(n)), " "))
		case "function_declaration":
			syms = append(syms, newSym(n, w.field(n, "name"), "function", w.header(n, "body")))
		case "method_declaration":
			name := w.field(n, "name")
			if m := goRecvRe.FindStringSubmatch(strings.TrimSpace(w.field(n, "receiver"))); m != nil {
				name = m[1] + "." + name
			}
			syms = append(syms, newSym(n, name, "method", w.header(n, "body")))
		case "type_declaration":
			for j := 0; j < int(n.NamedChildCount()); j++ {
				spec := n.NamedChild(j)
				if spec.Type() != "type_spec" && spec.Type() != "type_alias" {
					continue
				}
				kind := "type"
				if t := spec.ChildByFieldName("type"); t != nil {
					switch t.Type() {
					case "struct_type":
						kind = "struct"
					case "interface_type":
						kind = "interface"
					}
				}
				rng := spec
				if n.NamedChildCount() == 1 {
					rng = n
				}
				s := newSym(rng, w.field(spec, "name"), kind, w.firstLine(rng))
				syms = append(syms, s)
			}
		case "const_declaration", "var_declaration":
			kind := "const"
			if n.Type() == "var_declaration" {
				kind = "variable"
			}
			for j := 0; j < int(n.NamedChildCount()); j++ {
				spec := n.NamedChild(j)
				if spec.Type() != "const_spec" && spec.Type() != "var_spec" {
					continue
				}
				rng := spec
				if n.NamedChildCount() == 1 {
					rng = n
				}
				if nm := spec.ChildByFieldName("name"); nm != nil {
					syms = append(syms, newSym(rng, w.text(nm), kind, w.firstLine(rng)))
				}
			}
		}
		for _, s := range syms {
			d.take(n, s)
		}
		if len(syms) == 0 {
			d.reset()
		}
		out = append(out, syms...)
	}
	return out
}

func (w *smartWalker) jsSymbols(root *sitter.Node) []*smartSymbol {
	var out []*smartSymbol
	d := &docTracker{w: w}
	for i := 0; i < int(root.NamedChildCount()); i++ {
		n := root.NamedChild(i)
		if n.Type() == "comment" {
			d.add(n)
			continue
		}
		if n.Type() == "import_statement" {
			w.f.imports = append(w.f.imports, strings.Join(strings.Fields(w.text(n)), " "))
			d.reset()
			continue
		}
		decl, exported := n, false
		if n.Type() == "export_statement" {
			exported = true
			if dn := n.ChildByFieldName("declaration"); dn != nil {
				decl = dn
			}
		}
		syms := w.jsDecl(n, decl)
		for _, s := range syms {
			s.exported = exported
			d.take(n, s)
		}
		if len(syms) == 0 {
			d.reset()
		}
		out = append(out, syms...)
	}
	return out
}

// jsDecl outlines one TS/JS declaration. rng supplies the line range (the export wrapper
// when present); decl is the wrapped declaration.
func (w *smartWalker) jsDecl(rng, decl *sitter.Node) []*smartSymbol {
	sym := func(name, kind, sig string) *smartSymbol {
		s := newSym(rng, name, kind, sig)
		return s
	}
	switch decl.Type() {
	case "function_declaration", "generator_function_declaration":
		return []*smartSymbol{sym(w.field(decl, "name"), "function", w.header(decl, "body"))}
	case "class_declaration", "abstract_class_declaration":
		s := sym(w.field(decl, "name"), "class", w.header(decl, "body"))
		if body := decl.ChildByFieldName("body"); body != nil {
			dt := &docTracker{w: w}
			for j := 0; j < int(body.NamedChildCount()); j++ {
				m := body.NamedChild(j)
				if m.Type() == "comment" {
					dt.add(m)
					continue
				}
				switch m.Type() {
				case "method_definition", "abstract_method_signature", "method_signature":
					c := newSym(m, w.field(m, "name"), "method", w.header(m, "body"))
					dt.take(m, c)
					s.children = append(s.children, c)
				default:
					dt.reset()
				}
			}
		}
		return []*smartSymbol{s}
	case "interface_declaration":
		return []*smartSymbol{sym(w.field(decl, "name"), "interface", w.firstLine(decl))}
	case "type_alias_declaration":
		return []*smartSymbol{sym(w.field(decl, "name"), "type", w.firstLine(decl))}
	case "enum_declaration":
		return []*smartSymbol{sym(w.field(decl, "name"), "enum", w.firstLine(decl))}
	case "lexical_declaration", "variable_declaration":
		var out []*smartSymbol
		for j := 0; j < int(decl.NamedChildCount()); j++ {
			v := decl.NamedChild(j)
			if v.Type() != "variable_declarator" {
				continue
			}
			kind := "const"
			if val := v.ChildByFieldName("value"); val != nil {
				switch val.Type() {
				case "arrow_function", "function", "function_expression", "generator_function":
					kind = "function"
				}
			}
			out = append(out, sym(w.field(v, "name"), kind, w.firstLine(decl)))
		}
		return out
	}
	return nil
}

func (w *smartWalker) pySymbols(parent *sitter.Node, inClass bool) []*smartSymbol {
	var out []*smartSymbol
	for i := 0; i < int(parent.NamedChildCount()); i++ {
		n := parent.NamedChild(i)
		def := n
		if n.Type() == "decorated_definition" {
			if dn := n.ChildByFieldName("definition"); dn != nil {
				def = dn
			}
		}
		switch def.Type() {
		case "import_statement", "import_from_statement":
			if !inClass {
				w.f.imports = append(w.f.imports, strings.Join(strings.Fields(w.text(n)), " "))
			}
		case "function_definition":
			kind := "function"
			if inClass {
				kind = "method"
			}
			s := newSym(n, w.field(def, "name"), kind, w.header(def, "body"))
			s.doc = w.pyDoc(def)
			out = append(out, s)
		case "class_definition":
			s := newSym(n, w.field(def, "name"), "class", w.header(def, "body"))
			s.doc = w.pyDoc(def)
			if body := def.ChildByFieldName("body"); body != nil {
				s.children = w.pySymbols(body, true)
			}
			out = append(out, s)
		}
	}
	return out
}

func (w *smartWalker) pyDoc(def *sitter.Node) string {
	body := def.ChildByFieldName("body")
	if body == nil || body.NamedChildCount() == 0 {
		return ""
	}
	first := body.NamedChild(0)
	if first.Type() != "expression_statement" || first.NamedChildCount() == 0 || first.NamedChild(0).Type() != "string" {
		return ""
	}
	s := strings.Trim(w.text(first.NamedChild(0)), `"' `)
	return commentLine(s)
}

func (w *smartWalker) bashSymbols(root *sitter.Node) []*smartSymbol {
	var out []*smartSymbol
	d := &docTracker{w: w}
	for i := 0; i < int(root.NamedChildCount()); i++ {
		n := root.NamedChild(i)
		if n.Type() == "comment" {
			d.add(n)
			continue
		}
		if n.Type() == "function_definition" {
			name := w.field(n, "name")
			s := newSym(n, name, "function", name+"()")
			d.take(n, s)
			s.doc = strings.TrimSpace(strings.TrimLeft(s.doc, "# "))
			out = append(out, s)
			continue
		}
		d.reset()
	}
	return out
}

var smartIcons = map[string]string{
	"function": "ƒ", "method": "ƒ", "class": "◆", "interface": "◇", "type": "◇", "struct": "◆",
	"enum": "▣", "const": "●", "variable": "○",
}

func smartIcon(kind string) string {
	if i, ok := smartIcons[kind]; ok {
		return i
	}
	return "·"
}

func rowRange(s *smartSymbol) string {
	if s.startRow == s.endRow {
		return fmt.Sprintf("L%d", s.startRow+1)
	}
	return fmt.Sprintf("L%d-%d", s.startRow+1, s.endRow+1)
}

func writeSymbol(b *strings.Builder, s *smartSymbol, indent string) {
	exp := ""
	if s.exported {
		exp = " [exported]"
	}
	fmt.Fprintf(b, "%s%s %s%s (%s)\n", indent, smartIcon(s.kind), s.name, exp, rowRange(s))
	fmt.Fprintf(b, "%s  %s\n", indent, s.signature)
	if s.doc != "" {
		fmt.Fprintf(b, "%s  \U0001F4AC %s\n", indent, s.doc)
	}
	for _, c := range s.children {
		writeSymbol(b, c, indent+"  ")
	}
}

func (f *smartFile) fold(display string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "\U0001F4C1 %s (%s, %d lines)\n\n", display, f.language, f.totalLines)
	if len(f.imports) > 0 {
		fmt.Fprintf(&b, "  \U0001F4E6 Imports: %d statements\n", len(f.imports))
		for _, im := range f.imports[:min(10, len(f.imports))] {
			fmt.Fprintf(&b, "    %s\n", im)
		}
		if len(f.imports) > 10 {
			fmt.Fprintf(&b, "    ... +%d more\n", len(f.imports)-10)
		}
		b.WriteString("\n")
	}
	for _, s := range f.symbols {
		writeSymbol(&b, s, "  ")
	}
	return strings.TrimRight(b.String(), "\n")
}

// smartGate holds the workspace root and secret guard for one tool call.
type smartGate struct {
	root  string // symlink-resolved workspace
	guard *secretGuard
}

func newSmartGate() (*smartGate, error) {
	base := os.Getenv("CLAUDE_PROJECT_DIR")
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if base == "" {
		base = cwd
	}
	root, err := filepath.EvalSymlinks(base)
	if err != nil {
		return nil, fmt.Errorf("workspace %s: %w", base, err)
	}
	home, _ := os.UserHomeDir()
	return &smartGate{root: root, guard: newSecretGuard(home, os.Getenv("XDG_CONFIG_HOME"), cwd)}, nil
}

func (g *smartGate) inside(p string) bool {
	return p == g.root || strings.HasPrefix(p, g.root+string(filepath.Separator))
}

// resolve returns the symlink-resolved absolute path of arg (relative to the workspace),
// or an error text when it escapes the workspace or names a protected file.
func (g *smartGate) resolve(arg string) (string, error) {
	if strings.TrimSpace(arg) == "" {
		return "", errors.New("Invalid: file_path is required")
	}
	abs := arg
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(g.root, abs)
	}
	abs = filepath.Clean(abs)
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		real = abs // missing file: containment is still judged on the lexical path
	}
	if !g.inside(real) {
		return "", fmt.Errorf("Error: access denied: %q resolves outside the workspace (%s); smart_* tools only read files within the current project", arg, g.root)
	}
	if g.protectedPath(abs) || g.protectedPath(real) {
		return "", fmt.Errorf("Error: access denied: %q is a protected secret file; smart_* tools do not read it", arg)
	}
	return real, nil
}

func (g *smartGate) protectedPath(p string) bool {
	file, _ := g.guard.protected(p)
	return file
}

func smartLoad(args map[string]any) (*smartFile, []byte, string, error) {
	gate, err := newSmartGate()
	if err != nil {
		return nil, nil, "", err
	}
	abs, err := gate.resolve(smartArg(args, "file_path"))
	if err != nil {
		return nil, nil, "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read %s: %w", abs, err)
	}
	if fi.IsDir() || fi.Size() > smartMaxFileBytes {
		return nil, nil, "", fmt.Errorf("%s is a directory or larger than %d bytes", abs, smartMaxFileBytes)
	}
	src, err := os.ReadFile(abs)
	if err != nil {
		return nil, nil, "", fmt.Errorf("read %s: %w", abs, err)
	}
	f, err := smartParse(src, abs)
	return f, src, abs, err
}

// smartOutline returns the folded structural view of one file.
func smartOutline(args map[string]any) (string, error) {
	f, _, abs, err := smartLoad(args)
	if err != nil {
		return "", err
	}
	if f == nil || len(f.symbols) == 0 {
		return fmt.Sprintf("Could not parse %s. File may use an unsupported language or be empty.", smartArg(args, "file_path")), nil
	}
	return f.fold(abs), nil
}

func findSymbol(syms []*smartSymbol, name string, qualified bool, prefix string) *smartSymbol {
	for _, s := range syms {
		full := s.name
		if prefix != "" {
			full = prefix + "." + s.name
		}
		cand := s.name
		if qualified {
			cand = full
		}
		if cand == name {
			return s
		}
		if r := findSymbol(s.children, name, qualified, full); r != nil {
			return r
		}
	}
	return nil
}

func flattenSymbols(syms []*smartSymbol, prefix string, fn func(full string, s *smartSymbol)) {
	for _, s := range syms {
		full := s.name
		if prefix != "" {
			full = prefix + "." + s.name
		}
		fn(full, s)
		flattenSymbols(s.children, full, fn)
	}
}

// smartUnfold returns the full source of one symbol, doc comment included.
func smartUnfold(args map[string]any) (string, error) {
	name := smartArg(args, "symbol_name")
	if name == "" {
		return "", errors.New("Invalid: symbol_name is required")
	}
	f, src, abs, err := smartLoad(args)
	if err != nil {
		return "", err
	}
	if f == nil || len(f.symbols) == 0 {
		return fmt.Sprintf("Could not parse %s. File may be unsupported or empty.", smartArg(args, "file_path")), nil
	}
	s := findSymbol(f.symbols, name, true, "")
	if s == nil {
		s = findSymbol(f.symbols, name, false, "")
	}
	if s == nil && f.language == "go" {
		flattenSymbols(f.symbols, "", func(full string, c *smartSymbol) {
			if s == nil && c.kind == "method" && c.name[strings.LastIndexByte(c.name, '.')+1:] == name {
				s = c
			}
		})
	}
	if s == nil {
		var b strings.Builder
		fmt.Fprintf(&b, "Symbol %q not found in %s.\n", name, smartArg(args, "file_path"))
		n := 0
		flattenSymbols(f.symbols, "", func(full string, c *smartSymbol) {
			if n < smartUnfoldList {
				fmt.Fprintf(&b, "  - %s (%s)\n", full, c.kind)
			}
			n++
		})
		if n > smartUnfoldList {
			b.WriteString("  ... more symbols omitted; use smart_search to narrow the lookup.\n")
		}
		return strings.TrimRight(b.String(), "\n"), nil
	}
	lines := strings.Split(string(src), "\n")
	end := min(int(s.endRow)+1, len(lines))
	body := strings.Join(lines[s.docRow:end], "\n")
	return fmt.Sprintf("// \U0001F4CD %s L%d-%d\n\n%s", abs, s.docRow+1, s.endRow+1, body), nil
}

var smartSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true,
	"__pycache__": true, ".venv": true, "venv": true, "target": true, ".next": true,
}

// smartSearch finds symbols whose name matches query across a directory tree, and lists
// other files whose path or content matches.
func smartSearch(args map[string]any) (string, error) {
	query := smartArg(args, "query")
	if query == "" {
		return "", errors.New("Invalid: query is required")
	}
	gate, err := newSmartGate()
	if err != nil {
		return "", err
	}
	root := smartArg(args, "path")
	if root == "" {
		root = "."
	}
	if root, err = gate.resolve(root); err != nil {
		return "", err
	}
	maxResults := smartDefaultHits
	if v, ok := args["max_results"].(float64); ok && v > 0 {
		maxResults = int(v)
	}
	pattern := smartArg(args, "file_pattern")
	q := strings.ToLower(query)

	type hit struct {
		full string
		sym  *smartSymbol
		file *smartFile
		rel  string
	}
	var hits []hit
	var symFiles, contentFiles []*smartFile
	rels := map[*smartFile]string{}
	scanned, totalSyms, pathOnly := 0, 0, 0

	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && (smartSkipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") && d.Name() != ".") {
				return fs.SkipDir
			}
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		if smartLangFor(p) == nil || (pattern != "" && !strings.Contains(rel, pattern)) || scanned >= smartMaxFiles {
			return nil
		}
		real, err := filepath.EvalSymlinks(p)
		if err != nil || !gate.inside(real) || gate.protectedPath(p) || gate.protectedPath(real) {
			return nil
		}
		if fi, err := os.Stat(real); err != nil || fi.Size() > smartMaxFileBytes {
			return nil
		}
		src, err := os.ReadFile(real)
		if err != nil {
			return nil
		}
		f, err := smartParse(src, p)
		if err != nil || f == nil {
			return nil
		}
		scanned++
		rels[f] = rel
		matched := false
		flattenSymbols(f.symbols, "", func(full string, s *smartSymbol) {
			totalSyms++
			if strings.Contains(strings.ToLower(full), q) {
				matched = true
				hits = append(hits, hit{full, s, f, rel})
			}
		})
		switch {
		case matched:
			symFiles = append(symFiles, f)
		case strings.Contains(strings.ToLower(rel), q):
			pathOnly++
			contentFiles = append(contentFiles, f)
		case strings.Contains(strings.ToLower(string(src)), q):
			contentFiles = append(contentFiles, f)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("walk %s: %w", root, err)
	}

	sort.SliceStable(hits, func(i, j int) bool { return hits[i].rel < hits[j].rel })
	symMatches := len(hits)
	if len(hits) > maxResults {
		hits = hits[:maxResults]
	}
	shown := map[*smartFile]bool{}
	for _, h := range hits {
		shown[h.file] = true
	}
	var folded []*smartFile
	for _, f := range symFiles {
		if shown[f] {
			folded = append(folded, f)
		}
	}
	sort.Slice(folded, func(i, j int) bool { return rels[folded[i]] < rels[folded[j]] })
	sort.Slice(contentFiles, func(i, j int) bool { return rels[contentFiles[i]] < rels[contentFiles[j]] })
	if len(contentFiles) > maxResults {
		contentFiles = contentFiles[:maxResults]
	}

	var foldText []string
	tokens := 0
	for _, f := range folded {
		t := f.fold(rels[f])
		foldText = append(foldText, t)
		tokens += (len(t) + 3) / 4
	}

	var b strings.Builder
	fmt.Fprintf(&b, "\U0001F50D Smart Search: %q\n", query)
	fmt.Fprintf(&b, "   Scanned %d files, found %d symbols\n", scanned, totalSyms)
	fmt.Fprintf(&b, "   %d symbol matches; %d matched files (~%d tokens for folded view); %d files matched by path only\n\n",
		symMatches, len(symFiles), tokens, pathOnly)
	if len(hits) > 0 {
		b.WriteString("── Matching Symbols ──\n\n")
		for _, h := range hits {
			fmt.Fprintf(&b, "  %s %s (%s:%d)\n    %s\n", h.sym.kind, h.full, h.rel, h.sym.startRow+1, h.sym.signature)
			if h.sym.doc != "" {
				fmt.Fprintf(&b, "    \U0001F4AC %s\n", h.sym.doc)
			}
			b.WriteString("\n")
		}
	}
	if len(foldText) > 0 {
		b.WriteString("── Folded File Views ──\n\n")
		for _, t := range foldText {
			b.WriteString(t + "\n\n")
		}
	}
	if len(contentFiles) > 0 {
		b.WriteString("── Matching Files ──\n\n")
		for _, f := range contentFiles {
			fmt.Fprintf(&b, "  %s (%s, %d lines, ~%d tokens folded) — smart_outline to expand\n",
				rels[f], f.language, f.totalLines, (len(f.fold(rels[f]))+3)/4)
		}
		b.WriteString("\n")
	}
	b.WriteString("── Actions ──\n  To see full implementation: use smart_unfold with file path and symbol name")
	return b.String(), nil
}
