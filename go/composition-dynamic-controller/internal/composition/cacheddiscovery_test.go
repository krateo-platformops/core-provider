package composition

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"k8s.io/client-go/rest"
)

// #202. Every helm.NewClient that omits the shared discovery cache falls to plumbing's uncached
// RESTClientGetter, whose ToRESTMapper performs a FULL API discovery — every group, version and
// resource in the cluster. Observe builds two clients and runs on every resync, so on krateo-057 a
// controller managing nothing spent ~38s of every 60s tick and allocated 5,190 GB over 16h, 45.3%
// of it in NewDiscoveryRESTMapper.
//
// The failure mode is SILENT: a client that builds its own mapper behaves identically, only
// slower. Nothing errors and nothing is logged. So the call sites need a guard, not just the
// helper — this is the shape that survived the first mutation pass on #248 (the decision covered,
// the call site not) and on #232 before it.

// THE WIRING. Every helm.NewClient call in this package must pass h.helmCacheOpts(). A syntactic
// claim — "these two appear together" — which is what a source walk can actually prove, unlike a
// reachability claim.
func TestEveryHelmClientPassesTheSharedDiscoveryCache(t *testing.T) {
	// Parse EVERY non-test file in the package, not just composition.go. The original guard named
	// one file, and a helm.NewClient in existence.go therefore escaped the invariant entirely —
	// the guard's SCOPE was the defect, not its logic. A file-scoped guard answers "is this
	// invariant held in the file I named", which is not the claim it appears to make.
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parsing the package: %v", err)
	}
	var files []*ast.File
	for _, pkg := range pkgs {
		for _, f := range pkg.Files {
			files = append(files, f)
		}
	}
	if len(files) < 2 {
		t.Fatalf("parsed only %d non-test files; this guard is not reading the package and proves "+
			"nothing", len(files))
	}

	var total, withCache int
	var missing []string
	inspect := func(file *ast.File) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "NewClient" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "helm" {
				return true
			}
			total++
			// The cache options are prepended via append(h.helmCacheOpts(), ...), so look for that
			// call anywhere inside this NewClient expression.
			found := false
			ast.Inspect(call, func(m ast.Node) bool {
				inner, ok := m.(*ast.CallExpr)
				if !ok {
					return true
				}
				if s, ok := inner.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "helmCacheOpts" {
					found = true
				}
				return true
			})
			if found {
				withCache++
			} else {
				missing = append(missing, fset.Position(call.Pos()).String())
			}
			return true
		})
	}
	for _, f := range files {
		inspect(f)
	}

	if total == 0 {
		t.Fatal("found no helm.NewClient calls in composition.go — this guard is not looking at " +
			"the right file and proves nothing")
	}
	if withCache != total {
		t.Errorf("%d of %d helm.NewClient calls pass h.helmCacheOpts(); missing at %v. Each one "+
			"that does not performs a FULL API discovery per call, silently (#202).",
			withCache, total, missing)
	}
}

// Degrade-only: no kubeconfig (tests, or a discovery failure) must yield no cache and no options,
// so every call site keeps the pre-#202 behaviour rather than erroring. plumbing refuses a nil
// *CachedClients by design, so omitting the option entirely is how a caller says "I have none".
func TestHelmCacheOpts_DegradesWhenThereIsNoKubeconfig(t *testing.T) {
	if cc := newHelmCachedClients(nil); cc != nil {
		t.Errorf("newHelmCachedClients(nil) = %v, want nil", cc)
	}
	h := &handler{}
	if opts := h.helmCacheOpts(); len(opts) != 0 {
		t.Errorf("helmCacheOpts() returned %d options with no cache; passing a nil "+
			"*CachedClients would be refused by plumbing and fail every reconcile", len(opts))
	}
}

// And the positive case: with a config, the cache is built once and offered as exactly one option.
// Discovery is lazy inside DeferredDiscoveryRESTMapper, so this does not contact an apiserver.
func TestHelmCacheOpts_OffersTheCacheWhenPresent(t *testing.T) {
	cc := newHelmCachedClients(&rest.Config{Host: "https://api.example.invalid:6443"})
	if cc == nil {
		t.Fatal("newHelmCachedClients returned nil for a usable config; every helm client would " +
			"then build its own discovery mapper")
	}
	h := &handler{cachedClients: cc}
	if opts := h.helmCacheOpts(); len(opts) != 1 {
		t.Errorf("helmCacheOpts() returned %d options, want exactly 1", len(opts))
	}
}
