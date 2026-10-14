// Guard against a doc comment that documents one declaration while being ATTACHED to another.
//
// This is the defect that nearly shipped with core-provider#167. A new helper was inserted between
// isVersionMigrationHandover's doc comment and its func, which silently re-parented twenty lines of
// explanation — the whole argument for why a version bump is not a deletion — onto a function they
// said nothing about, and left the function they described undocumented. Nothing objected: gofmt
// reformats it happily, go vet has no opinion, every test passes. The only reason it was caught is
// that someone read the diff back. golint used to flag doc comments that do not begin with the name
// of what they document; nothing in the current toolchain does.
//
// The check is deliberately NARROW rather than a style rule. It does not require that every doc
// comment begin with its own declaration's name — plenty of honest prose does not, and three
// comments in this package alone legitimately start with a different word. It fires only on the
// signature of re-parenting:
//
//	a declaration's doc comment begins with the name of a DIFFERENT declaration in the same file,
//	AND that other declaration has no doc comment of its own.
//
// Both halves matter. The first says the comment names something other than what it is attached to;
// the second says the thing it names is sitting there undocumented, which is what re-parenting
// leaves behind. A comment that begins with another symbol's name while that symbol is separately
// documented is ordinary cross-reference prose ("Generate is called by..."), and is not flagged.
//
// It runs over the WHOLE repository, not just this package. The property is not specific to
// composition.go, and when this was first written it found two more instances of the same defect in
// core-provider (authnClientFor's doc attached to a var block; Generate's doc swallowed into the
// comment group above var readVerbs, some seventy lines from the function it describes) — which is
// the argument for the guard rather than a one-off correction.
//
// It lives in this package because this is where #167 happened and where the rest of that fix's
// source-level guards are (rbac_ordering_test.go). It is a test rather than a linter so it runs with
// `go test ./...` and needs no new tooling in CI.

package composition

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// repoRootFromPackage is this package's path relative to the repository root, inverted. Tests run
// with the package directory as the working directory, so the root is a fixed number of levels up.
const repoRootFromPackage = "../../../.."

// declInfo records where a top-level name is declared and whether it carries its own doc comment.
type declInfo struct {
	line   int
	hasDoc bool
}

// reparenting is one suspected misattached doc comment.
type reparenting struct {
	file       string // repository-relative
	attachedTo string // the declaration the comment is actually attached to
	attachLine int
	names      string // the declaration the comment appears to describe
	namesLine  int
}

// firstIdent returns the leading identifier of s, or "" if s does not start with one. Doc comments
// by convention open with the name of what they document, so this is the word to compare.
func firstIdent(s string) string {
	s = strings.TrimSpace(s)
	i := 0
	for i < len(s) {
		c := s[i]
		isLetter := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if isLetter || (i > 0 && isDigit) {
			i++
			continue
		}
		break
	}
	return s[:i]
}

// declaredNames returns the names a declaration introduces. A GenDecl block introduces all of them
// (`var ( a; b )` is documented by one comment covering the block), so any of them satisfies the
// comment.
func declaredNames(d ast.Decl) []string {
	switch dd := d.(type) {
	case *ast.FuncDecl:
		return []string{dd.Name.Name}
	case *ast.GenDecl:
		var out []string
		for _, s := range dd.Specs {
			switch ss := s.(type) {
			case *ast.TypeSpec:
				out = append(out, ss.Name.Name)
			case *ast.ValueSpec:
				for _, n := range ss.Names {
					out = append(out, n.Name)
				}
			}
		}
		return out
	}
	return nil
}

// docOf returns the doc comment attached to a declaration, or nil.
func docOf(d ast.Decl) *ast.CommentGroup {
	switch dd := d.(type) {
	case *ast.FuncDecl:
		return dd.Doc
	case *ast.GenDecl:
		return dd.Doc
	}
	return nil
}

// findReparentedDocs reports doc comments in f that name a different, undocumented declaration of
// the same file. fset must be the one f was parsed with.
func findReparentedDocs(fset *token.FileSet, name string, f *ast.File) []reparenting {
	// A name is "documented" if ANY declaration of it carries a doc comment. Methods with the same
	// name on different receivers share an entry; counting one as documented is the conservative
	// direction, since this check exists to report a MISSING comment.
	index := map[string]*declInfo{}
	for _, d := range f.Decls {
		doc := docOf(d) != nil
		for _, n := range declaredNames(d) {
			if cur, ok := index[n]; ok {
				cur.hasDoc = cur.hasDoc || doc
				continue
			}
			index[n] = &declInfo{line: fset.Position(d.Pos()).Line, hasDoc: doc}
		}
	}

	var out []reparenting
	for _, d := range f.Decls {
		doc := docOf(d)
		if doc == nil {
			continue
		}
		own := declaredNames(d)
		if len(own) == 0 {
			continue
		}

		lead := firstIdent(doc.Text())
		if lead == "" {
			continue
		}
		if slices.Contains(own, lead) {
			continue
		}
		other, ok := index[lead]
		if !ok || other.hasDoc {
			continue
		}

		out = append(out, reparenting{
			file:       name,
			attachedTo: own[0],
			attachLine: fset.Position(d.Pos()).Line,
			names:      lead,
			namesLine:  other.line,
		})
	}
	return out
}

func TestNoDocCommentDocumentsAnotherDeclaration(t *testing.T) {
	root, err := filepath.Abs(repoRootFromPackage)
	if err != nil {
		t.Fatalf("resolving repository root: %v", err)
	}
	// Assert the root is what it claims to be rather than scanning whatever happens to be up
	// there. A walk that silently covers nothing is the failure mode this guard must not have.
	for _, marker := range []string{
		filepath.Join("go", "core-provider", "go.mod"),
		filepath.Join("go", "composition-dynamic-controller", "go.mod"),
		filepath.Join("go", "chart-inspector", "go.mod"),
	} {
		if _, err := os.Stat(filepath.Join(root, marker)); err != nil {
			t.Fatalf("%s does not look like the core-provider repository root (missing %s): %v",
				root, marker, err)
		}
	}

	fset := token.NewFileSet()
	var findings []reparenting
	scanned := 0

	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "testdata":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		f, parseErr := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if parseErr != nil {
			t.Errorf("parsing %s: %v", rel, parseErr)
			return nil
		}
		scanned++
		findings = append(findings, findReparentedDocs(fset, rel, f)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatalf("no Go files found under %s; the walk proved nothing", root)
	}

	for _, f := range findings {
		t.Errorf("%s:%d: the doc comment on %s begins with %q, and %s (line %d) has no doc comment "+
			"of its own — the comment looks like it belongs to %s and was separated from it. Move it "+
			"back, or reword the opening if it is genuinely prose about %s. (See core-provider#167 "+
			"for the bug this guards.)",
			f.file, f.attachLine, f.attachedTo, f.names, f.names, f.namesLine, f.names, f.names)
	}
}

// The repository walk above can only prove the repository is clean today. These two cases prove the
// checker would object to the #167 shape and stay quiet on the prose that resembles it — without
// them a green run is a claim rather than a result.
func TestReparentingCheckerFiresOnTheBugItGuards(t *testing.T) {
	// The #167 shape exactly: a helper inserted between a doc comment and the function it
	// describes, leaving that function undocumented.
	const planted = `package p

// target reports whether this is a handover rather than a deletion.
//
// Twenty more lines of reasoning that say nothing at all about the function below.
func inserted() {}

func target() bool { return true }
`

	findings := parseAndCheck(t, "planted.go", planted)
	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 finding on the planted re-parenting, got %d: %+v", len(findings), findings)
	}
	if findings[0].attachedTo != "inserted" || findings[0].names != "target" {
		t.Errorf("finding identifies the wrong declarations: attached to %q, names %q; want inserted/target",
			findings[0].attachedTo, findings[0].names)
	}
}

func TestReparentingCheckerIgnoresLegitimateCrossReferences(t *testing.T) {
	const prose = `package p

// target reports whether this is a handover.
func target() bool { return true }

// target is consulted first; this runs only if it says no.
func other() {}

// helper is documented here, and the var block below is documented separately.
func helper() {}

// limits are the bounds helper enforces.
var limits = []int{1, 2}
`

	if findings := parseAndCheck(t, "prose.go", prose); len(findings) != 0 {
		t.Errorf("checker fired on legitimate cross-reference prose: %+v", findings)
	}
}

func parseAndCheck(t *testing.T, name, src string) []reparenting {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parsing %s: %v", name, err)
	}
	return findReparentedDocs(fset, name, f)
}
