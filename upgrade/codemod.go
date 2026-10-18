package upgrade

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/mod/semver"
	"golang.org/x/tools/go/ast/astutil"
	"golang.org/x/tools/go/packages"

	"github.com/gombit-dev/gombit/internal/atomicfile"
)

// Codemod is a mechanical rewrite of an application's uses of one
// framework API, for a change whose replacement is deterministic: a rename
// or a move (Rename, MoveTo), or any other rewrite of a use (Rewrite).
//
// A codemod rewrites uses, found by type information exactly as
// ScanDeprecated finds them (the same object, never the same text), and
// never the application's own declarations: renaming an interface method
// the application implements still needs its implementations renamed.
type Codemod struct {
	// ID names it (kebab-case); a manifest change applies it as action
	// "codemod:<ID>".
	ID string
	// Description says what it rewrites, in one line.
	Description string
	// Since is the release that ships it. The compatibility policy keeps
	// it registered at least until RemovableFrom(Since), so an app that
	// skips releases still has it (docs/compatibility.md).
	Since string
	// Package is the import path of the framework package declaring the
	// API it rewrites.
	Package string
	// Symbol is that API in Package: Name for a function, type, constant
	// or variable, Type.Name for a method or a struct field that Type's own
	// declaration declares. A member Type only reaches is refused: one
	// promoted from an embedded type or interface, or Base's when Type is
	// defined from it (type Type Base) or an alias of it. It is the
	// declaring type's, and every use of it would be rewritten, however
	// reached.
	Symbol string

	// Rename renames each use to this name. With MoveTo, the empty Rename
	// keeps the name.
	Rename string
	// MoveTo is the import path of the package the replacement of a
	// package-level API lives in: pkg.Name becomes newpkg.Name (or
	// newpkg.Rename), the file importing newpkg if it does not, and no
	// longer importing pkg if nothing else uses it.
	MoveTo string

	// Rewrite rewrites one use, for a change Rename and MoveTo cannot
	// express (reordered arguments, a new parameter): it returns the
	// expression replacing u.Node(), or nil (u.Skip) to leave the use for
	// the developer. It runs bottom-up, so u.Call's arguments are already
	// rewritten; give new nodes the position of what they replace. The
	// replacement must not use the API it replaces, or a second run would
	// rewrite it again: RunCodemods refuses one that does.
	Rewrite func(u *Use) ast.Expr
}

// codemods are the codemods this framework implements, by ID. It ships
// none yet: each comes with the release whose change it rewrites.
var codemods = map[string]Codemod{}

// codemodActionPrefix prefixes the action name of a codemod.
const codemodActionPrefix = "codemod:"

// Codemods are the codemods this framework implements, by ID.
func Codemods() []Codemod {
	out := make([]Codemod, 0, len(codemods))
	for _, c := range codemods {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ErrCodemodIncomplete: a codemod applied as an automatic change wrote
// what it could, but did not cover the whole application: uses it left for
// the developer, or files the build configuration excludes that mention
// its API (rerun under their GOOS, GOARCH or tags). The change is not done.
var ErrCodemodIncomplete = errors.New("upgrade: the codemod did not cover the whole application")

// action is the codemod as an automatic change. Check reports whether the
// app may need it: uses to rewrite, or files the build excludes that
// mention its API. Apply writes the rewrite, and fails
// (ErrCodemodIncomplete) when uses remain that it could not rewrite or
// excluded files mention the API: either way the change is not complete.
func (c Codemod) action() Action {
	return Action{
		Description: fmt.Sprintf("%s (gombit upgrade fix --only %s)", c.Description, c.ID),
		Apply: func(workDir string) (bool, error) {
			fix, err := RunCodemods(context.Background(), workDir, []Codemod{c})
			if err != nil {
				return false, err
			}
			written, err := fix.Write()
			changed := len(written) > 0
			if err != nil {
				return changed, err
			}
			var gaps []string
			if len(fix.Left) > 0 {
				gaps = append(gaps, fmt.Sprintf("%d use(s) left to change by hand", len(fix.Left)))
			}
			if len(fix.Unchecked) > 0 {
				gaps = append(gaps, fmt.Sprintf("not checked, excluded by this build: %s (rerun under their GOOS, GOARCH or tags)", strings.Join(fix.Unchecked, ", ")))
			}
			if len(gaps) > 0 {
				return changed, fmt.Errorf("%w: codemod %s: %s; gombit upgrade fix --only %s lists them", ErrCodemodIncomplete, c.ID, strings.Join(gaps, "; "), c.ID)
			}
			return changed, nil
		},
		Check: func(workDir string, _ Baseline) (bool, error) {
			return hasUses(context.Background(), workDir, c)
		},
	}
}

func (c Codemod) validate() error {
	if !changeID.MatchString(c.ID) {
		return fmt.Errorf("upgrade: codemod id %q: want kebab-case (a-z, 0-9, -)", c.ID)
	}
	if strings.TrimSpace(c.Description) == "" || strings.Contains(c.Description, "\n") {
		return fmt.Errorf("upgrade: codemod %s: description must be one non-empty line", c.ID)
	}
	if c.Since != "" && (!semver.IsValid(c.Since) || semver.Canonical(c.Since) != c.Since) {
		return fmt.Errorf("upgrade: codemod %s: since %q, want a release (vX.Y.Z)", c.ID, c.Since)
	}
	if !isFrameworkPath(c.Package) {
		return fmt.Errorf("upgrade: codemod %s: package %q is not a framework package", c.ID, c.Package)
	}
	parts := strings.Split(c.Symbol, ".")
	if len(parts) > 2 || !allIdents(parts) {
		return fmt.Errorf("upgrade: codemod %s: symbol %q, want Name or Type.Name", c.ID, c.Symbol)
	}
	builtin := c.Rename != "" || c.MoveTo != ""
	switch {
	case builtin && c.Rewrite != nil:
		return fmt.Errorf("upgrade: codemod %s: Rewrite, or Rename and MoveTo, not both", c.ID)
	case !builtin && c.Rewrite == nil:
		return fmt.Errorf("upgrade: codemod %s: nothing to do (no Rename, MoveTo or Rewrite)", c.ID)
	case c.Rename != "" && !token.IsIdentifier(c.Rename):
		return fmt.Errorf("upgrade: codemod %s: rename to %q, not an identifier", c.ID, c.Rename)
	case c.MoveTo != "" && len(parts) == 2:
		return fmt.Errorf("upgrade: codemod %s: only a package-level API moves (MoveTo); %s is a member", c.ID, c.Symbol)
	case c.MoveTo != "" && !isFrameworkPath(c.MoveTo):
		return fmt.Errorf("upgrade: codemod %s: moves to %q, not a framework package", c.ID, c.MoveTo)
	case c.MoveTo == c.Package && (c.Rename == "" || c.Rename == parts[len(parts)-1]):
		return fmt.Errorf("upgrade: codemod %s: renames %s to itself", c.ID, c.Symbol)
	case c.MoveTo == "" && c.Rename == parts[len(parts)-1]:
		return fmt.Errorf("upgrade: codemod %s: renames %s to itself", c.ID, c.Symbol)
	}
	return nil
}

func allIdents(names []string) bool {
	for _, n := range names {
		if !token.IsIdentifier(n) {
			return false
		}
	}
	return true
}

// Use is one use of a codemod's API, handed to its Rewrite.
type Use struct {
	// Ident is the identifier denoting the API.
	Ident *ast.Ident
	// Expr is the use: pkg.Name; x.Name (a method, a field, a method
	// expression); or a bare Name (a dot import, a composite literal key).
	Expr ast.Expr
	// Call is the call, or conversion, whose function Expr is, possibly
	// parenthesized and instantiated ((f)(x), f[T](x)); nil when Expr is
	// not called. CallFun builds a new call's Fun keeping those wrappers.
	Call *ast.CallExpr
	// Path is the enclosing nodes, Ident first and the file last.
	Path []ast.Node
	// Info is the type information of the use's package, for the source
	// as it was before any rewrite (new nodes have none).
	Info *types.Info

	file   *fileRewrite
	reason string
	// For a member: the type it is selected from (a composite literal's,
	// for a key), and whether a pointer method is callable on it, read
	// before any rewrite (a rewritten operand has no type information).
	recv        types.Type
	addressable bool
	key         bool // a composite literal key: only a direct field
}

// callOf is the call whose function is u.Expr, possibly parenthesized
// ((f)(x)) and instantiated (f[T](x), f[K, V](x)), or nil.
func callOf(u *Use) *ast.CallExpr {
	_, generic := u.Info.Instances[u.Ident]
	fun := u.Expr
	for i := 0; i+1 < len(u.Path); i++ {
		if u.Path[i] != fun {
			continue
		}
		switch n := u.Path[i+1].(type) {
		case *ast.ParenExpr:
			fun = n
		case *ast.IndexExpr: // an instantiation, not an index into a value
			if !generic || n.X != fun {
				return nil
			}
			fun = n
		case *ast.IndexListExpr:
			if n.X != fun {
				return nil
			}
			fun = n
		case *ast.CallExpr:
			if n.Fun == fun {
				return n
			}
			return nil
		default:
			return nil
		}
	}
	return nil
}

// CallFun is u.Call's function with Expr replaced by e, keeping the
// parentheses and type arguments around it: the Fun of a Rewrite's new
// call. Without a call, it is e.
func (u *Use) CallFun(e ast.Expr) ast.Expr {
	if u.Call == nil {
		return e
	}
	return rewrap(u.Call.Fun, u.Expr, e)
}

// rewrap is fun with old (inside its parentheses and instantiations)
// replaced by e, the wrappers copied.
func rewrap(fun, old, e ast.Expr) ast.Expr {
	switch f := fun.(type) {
	case *ast.ParenExpr:
		if fun != old {
			c := *f
			c.X = rewrap(f.X, old, e)
			return &c
		}
	case *ast.IndexExpr:
		c := *f
		c.X = rewrap(f.X, old, e)
		return &c
	case *ast.IndexListExpr:
		c := *f
		c.X = rewrap(f.X, old, e)
		return &c
	}
	return e
}

// Node is what a Rewrite replaces: Call when Expr is called, else Expr.
func (u *Use) Node() ast.Expr {
	if u.Call != nil {
		return u.Call
	}
	return u.Expr
}

// Skip leaves the use for the developer, saying why: a Rewrite returns
// u.Skip(reason) for a use it cannot rewrite.
func (u *Use) Skip(reason string) ast.Expr {
	u.reason = reason
	return nil
}

// Import is the name the use's file refers to the package at path by,
// adding its import when the file has none; false when the package does
// not load or that name is shadowed at the use (nothing could refer to it
// there).
func (u *Use) Import(path string) (string, bool) {
	name, ok := u.file.importName(path)
	if !ok {
		return "", false
	}
	if scope := u.file.pkg.Types.Scope().Innermost(u.Ident.Pos()); scope != nil {
		if _, obj := scope.LookupParent(name, u.Ident.Pos()); obj != nil {
			if pn, isPkg := obj.(*types.PkgName); !isPkg || pn.Imported().Path() != path {
				return "", false
			}
		}
	}
	return name, true
}

// Fix is the rewrite the codemods propose for an application: nothing is
// written until Write.
type Fix struct {
	// Files are the files it changes, by path.
	Files []FileFix `json:"files"`
	// Left are the uses no codemod could rewrite: the developer changes
	// them.
	Left []LeftUse `json:"left"`
	// Unchecked are the application's Go files the build configuration
	// excludes (build constraints) that may use a codemod's API (they
	// mention its name): they are not type-checked, so not rewritten. They
	// are found by walking the tree as ./... does, so a directory whose
	// every file is excluded is covered too. Rerun with their GOOS, GOARCH
	// or -tags (GOFLAGS) to cover them.
	Unchecked []string `json:"unchecked"`
}

// FileFix is one file's rewrite.
type FileFix struct {
	// Path is the file, relative to the application directory, with
	// forward slashes.
	Path string `json:"path"`
	// Before and After are its content before and after the rewrite.
	Before []byte `json:"-"`
	After  []byte `json:"-"`

	abs string
}

// LeftUse is a use a codemod could not rewrite, and why. Its position is
// in the file as the rewrite leaves it (as after Write or git apply).
type LeftUse struct {
	File    string `json:"file"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Codemod string `json:"codemod"`
	Symbol  string `json:"symbol"`
	Reason  string `json:"reason"`
}

// Patch is the rewrite as a unified diff, relative to the application
// directory: `git apply` (or `patch -p1`) applies it there.
func (f *Fix) Patch() string {
	var b strings.Builder
	for _, file := range f.Files {
		b.WriteString(unifiedDiff(file.Path, file.Before, file.After))
	}
	return b.String()
}

// ErrChangedSinceRun: a file the codemods rewrote changed after they read
// it, so writing their rewrite would discard that change. Nothing was
// written; run them again.
var ErrChangedSinceRun = errors.New("upgrade: a file changed since the codemods read it")

// Write writes the rewrite, each file atomically (whole or not at all), and
// returns the paths it wrote. It first checks that every file still holds
// exactly the bytes the codemods parsed, and writes nothing otherwise
// (ErrChangedSinceRun). That check is not a lock: Write does not
// coordinate with other writers, so a change made between it and a file's
// rename is lost. A failure writing one file leaves the ones before it
// written: the returned paths say which.
func (f *Fix) Write() ([]string, error) {
	modes := make([]os.FileMode, len(f.Files))
	for i, file := range f.Files {
		info, err := os.Stat(file.abs)
		if err != nil {
			return nil, fmt.Errorf("upgrade: %w", err)
		}
		current, err := os.ReadFile(file.abs) // #nosec G304 -- a file of the application's packages, as go/packages listed it
		if err != nil {
			return nil, fmt.Errorf("upgrade: %w", err)
		}
		if !bytes.Equal(current, file.Before) {
			return nil, fmt.Errorf("%w: %s", ErrChangedSinceRun, file.Path)
		}
		modes[i] = info.Mode().Perm()
	}
	var written []string
	for i, file := range f.Files {
		if err := atomicfile.Write(file.abs, file.After, modes[i]); err != nil {
			if atomicfile.Committed(err) {
				written = append(written, file.Path)
			}
			return written, fmt.Errorf("upgrade: write %s: %w", file.Path, err)
		}
		written = append(written, file.Path)
	}
	return written, nil
}

// RunCodemods runs mods on the application in workDir (all its packages,
// tests included) and returns the rewrite they propose, writing nothing.
//
// It loads and type-checks the application and the framework in one graph,
// as ScanDeprecated does (ErrDoesNotLoad when that fails), and rewrites the
// syntax tree of each use, never its text. A use it cannot rewrite safely
// (the new name would not resolve to the new API there, a generated or
// cgo-processed file, an embedded field whose name would change) is left
// for the developer, unchanged. Changed files are printed with go/format,
// so gofmt-formatted, keeping CRLF line endings. The codemods run in
// passes, each on the application as the last one left it (so a codemod
// may rewrite another's output), until a pass changes nothing: that pass
// type-checks the rewritten application, and shows that running the
// codemods on it changes nothing. A rewrite that does not type-check, or
// does not settle, is an error, and nothing is proposed.
func RunCodemods(ctx context.Context, workDir string, mods []Codemod) (*Fix, error) {
	seen := map[string]string{}
	for _, c := range mods {
		if err := c.validate(); err != nil {
			return nil, err
		}
		key := c.Package + "." + c.Symbol
		if other, dup := seen[key]; dup {
			return nil, fmt.Errorf("upgrade: codemods %s and %s both rewrite %s", other, c.ID, key)
		}
		seen[key] = c.ID
	}
	root, err := filepath.Abs(workDir)
	if err != nil {
		return nil, fmt.Errorf("upgrade: %w", err)
	}
	cfg, err := scanConfig(ctx, workDir)
	if err != nil {
		return nil, err
	}
	r := &rewriter{ctx: ctx, root: root, loaded: map[string]*types.Package{}}
	src := &sources{root: root, current: map[string][]byte{}}
	cfg.ParseFile = src.parseFile
	var unseen []string
	// One codemod's output can be another's input (a rename of a rename),
	// so the codemods run until a pass changes nothing. That last pass, on
	// the rewritten application (an overlay), is also the proof that the
	// rewrite type-checks and that running the codemods on it changes
	// nothing.
	for pass := 0; ; pass++ {
		if pass > len(mods) {
			return nil, fmt.Errorf("upgrade: the codemods do not settle after %d passes (a rewrite brings back what one rewrites, a codemod bug); none is proposed", pass)
		}
		cfg.Overlay = maps.Clone(src.current)
		src.parsed = map[string][]byte{}
		pkgs, err := loadPackages(cfg, "./...")
		if err != nil {
			if pass > 0 {
				return nil, fmt.Errorf("upgrade: the codemods' rewrite does not type-check, so none is proposed (a codemod bug): %w", err)
			}
			return nil, err
		}
		if err := src.settle(pass); err != nil {
			return nil, err
		}
		if pass == 0 {
			if unseen, err = unchecked(root, pkgs, mods); err != nil {
				return nil, err
			}
			loadedHookForTest()
		}
		r.graph = frameworkPackages(pkgs)
		targets, err := r.targets(mods, true)
		if err != nil {
			return nil, err
		}
		left := []LeftUse{}
		changed := false
		for _, af := range appFiles(pkgs) {
			after, replaced, l, err := r.file(af, targets, src)
			if err != nil {
				return nil, err
			}
			left = append(left, l...)
			// A pass that replaced anything is followed by another, even
			// when the output is byte-identical (a Rewrite rebuilding its
			// target from fresh nodes): only a pass with nothing to replace
			// ends the run, so such a rewrite never settles.
			changed = changed || replaced
			if after != nil {
				src.current[af.name] = after
			}
		}
		if !changed {
			return src.fix(root, left, unseen), nil
		}
	}
}

// loadedHookForTest runs after the first load (a test changes a file
// there).
var loadedHookForTest = func() {}

// sources are the exact bytes of the application's files the passes
// parsed (go/packages hands them to Config.ParseFile, once per file, from
// the disk or the overlay): never a second read, so a syntax tree and the
// content it is rewritten against are always the same version.
type sources struct {
	root string
	mu   sync.Mutex
	// parsed is this pass's: by file, inside root.
	parsed map[string][]byte
	// original is the first pass's: the content a rewrite starts from,
	// and that Write requires the disk to still hold.
	original map[string][]byte
	// current is the latest rewrite of each file a pass rewrote (the next
	// pass's overlay).
	current map[string][]byte
}

// parseFile is the loader's ParseFile (its default parse), recording the
// bytes of each file of the application.
func (s *sources) parseFile(fset *token.FileSet, filename string, src []byte) (*ast.File, error) {
	if _, inside := relTo(s.root, filename); inside {
		s.mu.Lock()
		s.parsed[filename] = src
		s.mu.Unlock()
	}
	return parser.ParseFile(fset, filename, src, parser.AllErrors|parser.ParseComments)
}

// settle records the first pass's bytes, and refuses a later pass that
// read a file other than the first pass did (not an overlay of a rewrite):
// it changed during the run.
func (s *sources) settle(pass int) error {
	if pass == 0 {
		s.original = s.parsed
		return nil
	}
	for name, data := range s.parsed {
		if _, rewritten := s.current[name]; rewritten {
			continue
		}
		if before, ok := s.original[name]; !ok || !bytes.Equal(before, data) {
			rel, _ := relTo(s.root, name)
			return fmt.Errorf("%w: %s (while they ran)", ErrChangedSinceRun, rel)
		}
	}
	return nil
}

// read is the content of the file go/packages parsed as af.
func (s *sources) read(af appFile, rel string) ([]byte, error) {
	data, ok := s.parsed[af.name]
	if !ok {
		return nil, fmt.Errorf("upgrade: %s: no source recorded for it", rel)
	}
	return data, nil
}

// fix is the rewrite: every file whose content the passes changed.
func (s *sources) fix(root string, left []LeftUse, unseen []string) *Fix {
	fix := &Fix{Files: []FileFix{}, Left: left, Unchecked: unseen}
	for name, after := range s.current {
		// A pass never restores a file's original: it held a use, which
		// the next pass would rewrite again.
		rel, _ := relTo(root, name)
		fix.Files = append(fix.Files, FileFix{Path: rel, Before: s.original[name], After: after, abs: name})
	}
	sort.Slice(fix.Files, func(i, j int) bool { return fix.Files[i].Path < fix.Files[j].Path })
	sort.Slice(fix.Left, func(i, j int) bool {
		a, b := fix.Left[i], fix.Left[j]
		if a.File != b.File {
			return a.File < b.File
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.Column < b.Column
	})
	return fix
}

// hasUses reports whether the application in workDir uses c's API, or has
// files its build configuration excludes that mention it (they may use
// it): one load, no rewrite.
func hasUses(ctx context.Context, workDir string, c Codemod) (bool, error) {
	if err := c.validate(); err != nil {
		return false, err
	}
	cfg, err := scanConfig(ctx, workDir)
	if err != nil {
		return false, err
	}
	pkgs, err := loadPackages(cfg, "./...")
	if err != nil {
		return false, err
	}
	r := &rewriter{graph: frameworkPackages(pkgs)}
	targets, err := r.targets([]Codemod{c}, false)
	if err != nil {
		return false, err
	}
	for _, af := range appFiles(pkgs) {
		if len(af.uses(targets)) > 0 {
			return true, nil
		}
	}
	root, err := filepath.Abs(workDir)
	if err != nil {
		return false, fmt.Errorf("upgrade: %w", err)
	}
	unseen, err := unchecked(root, pkgs, []Codemod{c})
	return len(unseen) > 0, err
}

// frameworkPackages are the framework packages in the graph, by path.
func frameworkPackages(pkgs []*packages.Package) map[string]*packages.Package {
	out := map[string]*packages.Package{}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		if isFrameworkPath(p.PkgPath) && p.Types != nil && out[p.PkgPath] == nil {
			out[p.PkgPath] = p
		}
	})
	return out
}

// appFile is one source file of the application, in the first package
// (variant) that compiles it: a package and its test variant share their
// files, and see the same framework objects in them.
type appFile struct {
	pkg  *packages.Package
	file *ast.File
	name string // the file parsed: for a cgo-processed one, not the source
	// variants are every package compiling the file (a package and its
	// test variant): a name the file adds must be free in all of them.
	variants []*types.Package
}

func appFiles(pkgs []*packages.Package) []appFile {
	var out []appFile
	seen := map[string]int{}
	for _, p := range pkgs {
		if p.Module != nil && p.Module.Path == FrameworkModulePath {
			continue
		}
		for _, f := range p.Syntax {
			name := p.Fset.File(f.FileStart).Name()
			if i, ok := seen[name]; ok {
				out[i].variants = append(out[i].variants, p.Types)
				continue
			}
			seen[name] = len(out)
			out = append(out, appFile{pkg: p, file: f, name: name, variants: []*types.Package{p.Types}})
		}
	}
	return out
}

// uses are the identifiers of af that denote a target, in source order.
func (af appFile) uses(targets map[types.Object]*target) []*ast.Ident {
	var ids []*ast.Ident
	for id, obj := range af.pkg.TypesInfo.Uses {
		if id.Pos() < af.file.FileStart || id.Pos() >= af.file.FileEnd {
			continue
		}
		if _, ok := targets[origin(obj)]; ok {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i].Pos() < ids[j].Pos() })
	return ids
}

// unchecked are the application's Go files the load did not type-check
// (excluded by build constraints) that mention a codemod's API by name:
// they may use it, unseen. They are found by walking the directory tree as
// the go command matches ./... (skipping testdata, vendor, names starting
// with "." or "_", and nested modules the load does not cover), not from
// the loaded packages: a directory whose every file is excluded yields no
// package at all, and its files would go unseen.
func unchecked(root string, pkgs []*packages.Package, mods []Codemod) ([]string, error) {
	names := map[string]bool{}
	for _, c := range mods {
		parts := strings.Split(c.Symbol, ".")
		names[parts[len(parts)-1]] = true
	}
	checked := map[string]bool{}
	modules := map[string]bool{} // module roots the load covers
	for _, p := range pkgs {
		if p.Module != nil && p.Module.Path == FrameworkModulePath {
			continue
		}
		for _, f := range p.GoFiles {
			checked[filepath.Clean(f)] = true
		}
		if p.Module != nil && p.Module.Dir != "" {
			modules[filepath.Clean(p.Module.Dir)] = true
		}
	}
	out := []string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() {
			if path == root {
				return nil
			}
			if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || name == "testdata" || name == "vendor" {
				return filepath.SkipDir
			}
			if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil && !modules[filepath.Clean(path)] {
				return filepath.SkipDir // another module
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "_") || checked[filepath.Clean(path)] {
			return nil
		}
		rel, _ := relTo(root, path)
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			out = append(out, rel) // cannot tell: say so
			return nil
		}
		mentions := false
		ast.Inspect(f, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && names[id.Name] {
				mentions = true
			}
			return !mentions
		})
		if mentions {
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("upgrade: list the application's files: %w", err)
	}
	sort.Strings(out)
	return out, nil
}

// relTo is file relative to root, with forward slashes, and whether it is
// inside root.
func relTo(root, file string) (string, bool) {
	rel, err := filepath.Rel(root, file)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(file), false
	}
	return filepath.ToSlash(rel), true
}

// rewriter runs codemods over an application's files.
type rewriter struct {
	ctx    context.Context
	root   string
	graph  map[string]*packages.Package // the framework packages loaded with the app
	loaded map[string]*types.Package    // others, loaded on demand (nil: does not load)
	owners map[*packages.Package]map[types.Object]string
}

// target is a codemod's API, resolved in the loaded graph.
type target struct {
	mod    *Codemod
	obj    types.Object
	member bool // a method or a field
	// A rename or a move: the API replacing it, or why it is unavailable.
	newPkg  string
	newName string
	newObj  types.Object
	missing string
}

// targets resolves each codemod's API in the graph, and with withNew the
// API a rename or a move replaces it with. One whose package the app does
// not reach, or that this framework version does not declare, has no uses
// to rewrite. A member target must be declared by the type its Symbol
// names: uses are matched by object, so a member Type only promotes would
// rewrite every use of the declaring type's member, wherever it is
// reached from. The replacement may be promoted (it only has to resolve).
func (r *rewriter) targets(mods []Codemod, withNew bool) (map[types.Object]*target, error) {
	out := map[types.Object]*target{}
	for i := range mods {
		c := &mods[i]
		p := r.graph[c.Package]
		if p == nil {
			continue
		}
		obj := lookupSymbol(p.Types, c.Symbol)
		if obj == nil {
			continue
		}
		if typ, _, isMember := strings.Cut(c.Symbol, "."); isMember {
			if r.owners == nil {
				r.owners = map[*packages.Package]map[types.Object]string{}
			}
			if r.owners[p] == nil {
				r.owners[p] = owners(p)
			}
			if owner := r.owners[p][obj]; owner != typ {
				by := "an unnamed type"
				if owner != "" {
					by = c.Package + "." + owner
				}
				return nil, fmt.Errorf("upgrade: codemod %s: %s.%s is declared by %s, not %s (promoted through it, or %s is defined from or an alias of that type); name the type that declares it", c.ID, c.Package, c.Symbol, by, typ, typ)
			}
		}
		t := &target{mod: c, obj: obj}
		parts := strings.Split(c.Symbol, ".")
		t.member = len(parts) == 2
		if c.Rewrite == nil && withNew {
			t.newPkg, t.newName = c.Package, parts[len(parts)-1]
			if c.MoveTo != "" {
				t.newPkg = c.MoveTo
			}
			if c.Rename != "" {
				t.newName = c.Rename
			}
			newSymbol := t.newName
			if t.member {
				newSymbol = parts[0] + "." + t.newName
			}
			if np := r.types(t.newPkg); np != nil {
				t.newObj = lookupSymbol(np, newSymbol)
			}
			if t.newObj == nil {
				t.missing = fmt.Sprintf("%s.%s does not exist in the framework version the app builds against: upgrade it first (gombit upgrade --to)", t.newPkg, newSymbol)
			}
		}
		out[obj] = t
	}
	return out, nil
}

// lookupSymbol is Name or Type.Name in pkg, or nil: for Type.Name, the
// member a selector Type.Name reaches (declared by Type, or not).
func lookupSymbol(pkg *types.Package, symbol string) types.Object {
	parts := strings.Split(symbol, ".")
	obj := pkg.Scope().Lookup(parts[0])
	if obj == nil || len(parts) == 1 {
		return obj
	}
	tn, ok := obj.(*types.TypeName)
	if !ok {
		return nil
	}
	member, _, _ := types.LookupFieldOrMethod(tn.Type(), true, pkg, parts[1])
	if member == nil {
		return nil
	}
	return origin(member)
}

// owners maps each member p declares (a struct field, named or embedded;
// an interface method; a method) to the name of the type whose declaration
// declares it, from the syntax: a field or an interface method to its
// enclosing type spec, a method to its receiver's type. A type defined from
// another (type Copy Base) or an alias of it (type A = Base) declares
// nothing (its spec has no struct or interface of its own): Base's members
// stay Base's.
func owners(p *packages.Package) map[types.Object]string {
	out := map[types.Object]string{}
	def := func(id *ast.Ident, owner string) {
		if id == nil {
			return
		}
		if obj := p.TypesInfo.Defs[id]; obj != nil {
			out[obj] = owner
		}
	}
	for _, f := range p.Syntax {
		for _, decl := range f.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil || len(decl.Recv.List) == 0 {
					continue
				}
				recv := decl.Recv.List[0].Type
				if star, ok := recv.(*ast.StarExpr); ok {
					recv = star.X
				}
				if id := embeddedName(recv); id != nil {
					def(decl.Name, id.Name)
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					ts, ok := spec.(*ast.TypeSpec)
					if !ok {
						continue
					}
					typ := ast.Unparen(ts.Type) // type T (struct{...}): parentheses change nothing
					var fields *ast.FieldList
					_, isStruct := typ.(*ast.StructType)
					switch t := typ.(type) {
					case *ast.StructType:
						fields = t.Fields
					case *ast.InterfaceType:
						fields = t.Methods
					default:
						continue
					}
					for _, field := range fields.List {
						if len(field.Names) == 0 {
							if isStruct { // an embedded field; an embedded interface declares nothing
								def(embeddedName(field.Type), ts.Name.Name)
							}
							continue
						}
						for _, name := range field.Names {
							def(name, ts.Name.Name)
						}
					}
				}
			}
		}
	}
	return out
}

// types is the framework package at path, from the graph or loaded on its
// own; nil when it does not load.
func (r *rewriter) types(path string) *types.Package {
	if p := r.graph[path]; p != nil {
		return p.Types
	}
	if t, done := r.loaded[path]; done {
		return t
	}
	r.loaded[path] = nil
	pkgs, err := packages.Load(&packages.Config{Context: r.ctx, Dir: r.root, Mode: packages.NeedName | packages.NeedTypes}, path)
	if err == nil && len(pkgs) == 1 && len(pkgs[0].Errors) == 0 && pkgs[0].Types != nil {
		r.loaded[path] = pkgs[0].Types
	}
	return r.loaded[path]
}

// fileRewrite is the rewrite of one file in progress.
type fileRewrite struct {
	r        *rewriter
	pkg      *packages.Package
	variants []*types.Package
	file     *ast.File
	added    map[string]string // imports added: path -> name
	// parenthesized are the file's import declarations parenthesized
	// before the rewrite.
	parenthesized map[*ast.GenDecl]bool
}

// file rewrites af's uses of targets: its new content (nil when the bytes
// are unchanged), whether any use was replaced, and the uses left.
func (r *rewriter) file(af appFile, targets map[types.Object]*target, src *sources) (after []byte, replaced bool, left []LeftUse, err error) {
	ids := af.uses(targets)
	if len(ids) == 0 {
		return nil, false, nil, nil
	}
	p, f := af.pkg, af.file
	fr := &fileRewrite{r: r, pkg: p, variants: af.variants, file: f, added: map[string]string{}, parenthesized: map[*ast.GenDecl]bool{}}
	for _, decl := range f.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.IMPORT && gen.Lparen.IsValid() {
			fr.parenthesized[gen] = true
		}
	}
	rel, inside := relTo(r.root, af.name)
	notEditable := ""
	switch {
	case !inside:
		notEditable = "outside the application directory"
	case !slices.Contains(p.GoFiles, af.name):
		notEditable = "in a cgo file (go/packages sees its cgo-processed form)"
	case ast.IsGenerated(f):
		notEditable = "in a generated file: regenerate it instead"
	}
	leave := func(t *target, id *ast.Ident, reason string) {
		at := p.Fset.Position(id.Pos())
		file := rel
		if at.Filename != af.name { // a //line directive
			file, _ = relTo(r.root, at.Filename)
		}
		left = append(left, LeftUse{File: file, Line: at.Line, Column: at.Column, Codemod: t.mod.ID, Symbol: t.mod.Package + "." + t.mod.Symbol, Reason: reason})
	}
	nodes := map[ast.Node]*Use{}
	owner := map[*Use]*target{}
	for _, id := range ids {
		t := targets[origin(p.TypesInfo.Uses[id])]
		if notEditable != "" {
			leave(t, id, notEditable)
			continue
		}
		path, _ := astutil.PathEnclosingInterval(f, id.Pos(), id.End())
		if len(path) == 0 || path[0] != id {
			leave(t, id, "not found in the syntax tree")
			continue
		}
		u := &Use{Ident: id, Expr: id, Path: path, Info: p.TypesInfo, file: fr}
		if sel, ok := path[1].(*ast.SelectorExpr); ok && sel.Sel == id {
			u.Expr = sel
		}
		u.Call = callOf(u)
		if t.member {
			u.recv, u.addressable = memberOperand(u)
			_, u.key = u.Expr.(*ast.Ident)
		}
		nodes[u.Node()] = u
		owner[u] = t
	}
	if len(nodes) == 0 {
		return nil, false, left, nil
	}
	changed := false
	var rewriteErr error
	func() {
		defer func() {
			if v := recover(); v != nil { // a Rewrite returned a node that cannot go there
				rewriteErr = fmt.Errorf("upgrade: rewriting %s: %v", rel, v)
			}
		}()
		astutil.Apply(f, nil, func(c *astutil.Cursor) bool {
			u := nodes[c.Node()]
			if u == nil || rewriteErr != nil {
				return rewriteErr == nil
			}
			t := owner[u]
			var repl ast.Expr
			if t.mod.Rewrite != nil {
				repl = t.mod.Rewrite(u)
			} else {
				repl = t.rename(u)
			}
			if repl == nil {
				reason := u.reason
				if reason == "" {
					reason = "the codemod does not rewrite this use"
				}
				leave(t, u.Ident, reason)
				return true
			}
			if usesTarget(repl, p.TypesInfo, t.obj) {
				rewriteErr = fmt.Errorf("upgrade: codemod %s: its rewrite of %s still uses it", t.mod.ID, t.mod.Symbol)
				return false
			}
			c.Replace(repl)
			changed = true
			return true
		})
	}()
	if rewriteErr != nil {
		return nil, false, nil, rewriteErr
	}
	if !changed {
		return nil, false, left, nil
	}
	fr.dropUnusedImports()
	var out bytes.Buffer
	if err := format.Node(&out, p.Fset, f); err != nil {
		return nil, false, nil, fmt.Errorf("upgrade: format %s: %w", rel, err)
	}
	before, err := src.read(af, rel)
	if err != nil {
		return nil, false, nil, err
	}
	after = out.Bytes()
	if n := bytes.Count(before, []byte("\n")); n > 0 && bytes.Count(before, []byte("\r\n")) == n {
		after = bytes.ReplaceAll(after, []byte("\n"), []byte("\r\n"))
	}
	if bytes.Equal(before, after) {
		return nil, true, left, nil
	}
	return after, true, left, nil
}

// usesTarget reports whether e holds an identifier of the original source
// that denotes obj.
func usesTarget(e ast.Expr, info *types.Info, obj types.Object) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && origin(info.Uses[id]) == obj {
			found = true
		}
		return !found
	})
	return found
}

// rename is the built-in Rewrite of a rename or a move: the use naming
// the new API instead, when that name resolves to it there.
func (t *target) rename(u *Use) ast.Expr {
	if t.missing != "" {
		return u.Skip(t.missing)
	}
	var repl ast.Expr
	switch e := u.Expr.(type) {
	case *ast.SelectorExpr:
		if t.member {
			if !t.resolves(u.recv, u.addressable, false) {
				return u.Skip(fmt.Sprintf("%s does not resolve to %s.%s here", t.newName, t.newPkg, t.mod.Symbol))
			}
			repl = &ast.SelectorExpr{X: e.X, Sel: &ast.Ident{NamePos: e.Sel.NamePos, Name: t.newName}}
			break
		}
		if t.embeddedRename(u) {
			return u.Skip("an embedded field: renaming its type renames the field, and the app's uses of it")
		}
		name, ok := u.Import(t.newPkg)
		if !ok {
			return u.Skip(fmt.Sprintf("the name of %s is shadowed here", t.newPkg))
		}
		x := &ast.Ident{NamePos: e.X.Pos(), Name: name}
		repl = &ast.SelectorExpr{X: x, Sel: &ast.Ident{NamePos: e.Sel.NamePos, Name: t.newName}}
	case *ast.Ident:
		if t.member { // a composite literal key
			if !t.resolves(u.recv, true, true) {
				return u.Skip(fmt.Sprintf("%s does not resolve to %s.%s here", t.newName, t.newPkg, t.mod.Symbol))
			}
			repl = &ast.Ident{NamePos: e.NamePos, Name: t.newName}
			break
		}
		// A dot import: the same package keeps it bare; another qualifies.
		if t.embeddedRename(u) {
			return u.Skip("an embedded field: renaming its type renames the field, and the app's uses of it")
		}
		if t.newPkg == t.obj.Pkg().Path() {
			scope := u.file.pkg.Types.Scope().Innermost(e.Pos())
			if scope == nil {
				return u.Skip("no scope for the use")
			}
			if _, obj := scope.LookupParent(t.newName, e.Pos()); obj != t.newObj {
				return u.Skip(fmt.Sprintf("%s does not resolve to %s.%s here", t.newName, t.newPkg, t.newName))
			}
			repl = &ast.Ident{NamePos: e.NamePos, Name: t.newName}
			break
		}
		name, ok := u.Import(t.newPkg)
		if !ok {
			return u.Skip(fmt.Sprintf("the name of %s is shadowed here", t.newPkg))
		}
		repl = &ast.SelectorExpr{X: &ast.Ident{NamePos: e.NamePos, Name: name}, Sel: &ast.Ident{NamePos: e.NamePos, Name: t.newName}}
	default:
		return u.Skip("not a name")
	}
	if u.Call != nil {
		call := *u.Call
		call.Fun = u.CallFun(repl)
		return &call
	}
	return repl
}

// memberOperand is the type a member use is selected from, and whether a
// pointer method is callable on it: a method value needs an addressable
// (or pointer) operand for one, a method expression names its receiver
// type. For a composite literal key, the literal's type.
func memberOperand(u *Use) (types.Type, bool) {
	switch e := u.Expr.(type) {
	case *ast.SelectorExpr:
		addressable := true
		if s := u.Info.Selections[e]; s != nil && s.Kind() != types.FieldVal {
			addressable = s.Kind() == types.MethodVal && (u.Info.Types[e.X].Addressable() || isPointer(s.Recv()))
		}
		return u.Info.TypeOf(e.X), addressable
	case *ast.Ident:
		if len(u.Path) > 2 {
			if kv, ok := u.Path[1].(*ast.KeyValueExpr); ok && kv.Key == e {
				if lit, ok := u.Path[2].(*ast.CompositeLit); ok {
					return u.Info.TypeOf(lit), true
				}
			}
		}
	}
	return nil, false
}

// resolves reports whether t's new name, selected from a value (or a type)
// of type recv, is t's new API: not a shadowing member, callable on an
// operand as addressable as the one selected from, and, for a composite
// literal key (direct), a field of recv itself, not a promoted one.
func (t *target) resolves(recv types.Type, addressable, direct bool) bool {
	if recv == nil {
		return false
	}
	got, index, _ := types.LookupFieldOrMethod(recv, addressable, t.obj.Pkg(), t.newName)
	return got != nil && origin(got) == t.newObj && (!direct || len(index) == 1)
}

func isPointer(t types.Type) bool {
	_, ok := t.Underlying().(*types.Pointer)
	return ok
}

// embeddedRename reports whether u is the type of an embedded field and
// the rewrite would change its name (which is the type's).
func (t *target) embeddedRename(u *Use) bool {
	_, isType := t.obj.(*types.TypeName)
	return isType && t.newName != t.obj.Name() && u.Info.Defs[u.Ident] != nil
}

// importName is the name the file refers to the package at path by, adding
// the import (named so as not to clash with any name in the file's scope)
// when it has none; false when the package does not load.
func (fr *fileRewrite) importName(path string) (string, bool) {
	for _, spec := range fr.file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil || p != path {
			continue
		}
		if spec.Name != nil {
			if spec.Name.Name == "_" || spec.Name.Name == "." {
				continue
			}
			return spec.Name.Name, true
		}
		if pn, ok := fr.pkg.TypesInfo.Implicits[spec].(*types.PkgName); ok {
			return pn.Name(), true
		}
	}
	if name, ok := fr.added[path]; ok {
		return name, true
	}
	pkg := fr.r.types(path)
	if pkg == nil {
		return "", false
	}
	name := pkg.Name()
	for i := 2; fr.taken(name); i++ {
		name = pkg.Name() + strconv.Itoa(i)
	}
	local := name
	if local == pkg.Name() {
		local = ""
	}
	astutil.AddNamedImport(fr.pkg.Fset, fr.file, local, path)
	fr.added[path] = name
	return name, true
}

// taken reports whether name is already declared in the file's scope: an
// import, a package-level name (of any variant: a test file's too), or a
// predeclared one.
func (fr *fileRewrite) taken(name string) bool {
	for _, spec := range fr.file.Imports {
		if fr.localName(spec) == name {
			return true
		}
	}
	for _, n := range fr.added {
		if n == name {
			return true
		}
	}
	for _, v := range fr.variants {
		if v.Scope().Lookup(name) != nil {
			return true
		}
	}
	return types.Universe.Lookup(name) != nil
}

// localName is the name an import declares in its file ("" for a blank or
// dot import, or one added by the rewrite).
func (fr *fileRewrite) localName(spec *ast.ImportSpec) string {
	if spec.Name != nil {
		if spec.Name.Name == "_" || spec.Name.Name == "." {
			return ""
		}
		return spec.Name.Name
	}
	if pn, ok := fr.pkg.TypesInfo.Implicits[spec].(*types.PkgName); ok {
		return pn.Name()
	}
	path, _ := strconv.Unquote(spec.Path.Value)
	return fr.added[path]
}

// dropUnusedImports removes the imports the rewrite left unused (the file
// type-checked, so every import was used before it). A named or plain
// import is used by a selector whose X is its package name (the original's,
// or a new identifier of that name); a dot import by an original
// identifier denoting one of its package's objects unqualified, or by any
// new bare identifier its package declares.
func (fr *fileRewrite) dropUnusedImports() {
	info := fr.pkg.TypesInfo
	named := map[string]bool{}
	dotted := map[string]bool{}
	qualified := map[*ast.Ident]bool{}
	ast.Inspect(fr.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.ImportSpec:
			return false
		case *ast.SelectorExpr: // visited before its X and Sel
			if x, ok := n.X.(*ast.Ident); ok {
				if pn, ok := info.Uses[x].(*types.PkgName); ok {
					named[pn.Name()], qualified[n.Sel] = true, true
				} else if isNewIdent(info, x) {
					named[x.Name], qualified[n.Sel] = true, true
				}
			}
		case *ast.Ident:
			if !qualified[n] {
				fr.dotUse(n, dotted)
			}
		}
		return true
	})
	for _, spec := range append([]*ast.ImportSpec(nil), fr.file.Imports...) {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || path == "C" {
			continue
		}
		var used bool
		var name string
		switch {
		case spec.Name != nil && spec.Name.Name == "_":
			continue
		case spec.Name != nil && spec.Name.Name == ".":
			used, name = dotted[path], "."
			if !used {
				if pkg := fr.r.types(path); pkg == nil || fr.anyBareNew(pkg) {
					continue // cannot tell: keep it
				}
			}
		default:
			name = fr.localName(spec)
			used = named[name]
			if spec.Name == nil {
				name = ""
			}
		}
		if !used {
			astutil.DeleteNamedImport(fr.pkg.Fset, fr.file, name, path)
		}
	}
	// An import declaration the rewrite parenthesized (adding an import)
	// and left with one spec reads as it did: import "path".
	for _, decl := range fr.file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.IMPORT || !gen.Lparen.IsValid() || len(gen.Specs) != 1 || fr.parenthesized[gen] {
			continue
		}
		if spec := gen.Specs[0].(*ast.ImportSpec); spec.Doc == nil && spec.Comment == nil {
			gen.Lparen, gen.Rparen = token.NoPos, token.NoPos
		}
	}
}

// dotUse records, for an identifier of the original source denoting a
// package-level object of another package unqualified, that package's
// dot import as used.
func (fr *fileRewrite) dotUse(id *ast.Ident, dotted map[string]bool) {
	if obj := fr.pkg.TypesInfo.Uses[id]; obj != nil && obj.Pkg() != nil && obj.Pkg() != fr.pkg.Types && obj.Parent() == obj.Pkg().Scope() {
		dotted[obj.Pkg().Path()] = true
	}
}

// anyBareNew reports whether the file has a new (rewritten) bare
// identifier that pkg declares: it may be a use through a dot import.
func (fr *fileRewrite) anyBareNew(pkg *types.Package) bool {
	info := fr.pkg.TypesInfo
	found := false
	sels := map[*ast.Ident]bool{} // a selector's Sel is never bare
	ast.Inspect(fr.file, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.SelectorExpr:
			sels[n.Sel] = true
		case *ast.Ident:
			found = !sels[n] && isNewIdent(info, n) && pkg.Scope().Lookup(n.Name) != nil
		}
		return !found
	})
	return found
}

// isNewIdent reports whether id is not of the original source (the type
// checker recorded nothing for it).
func isNewIdent(info *types.Info, id *ast.Ident) bool {
	_, use := info.Uses[id]
	_, def := info.Defs[id]
	return !use && !def
}
