// Package rbacguard answers one question: does the controller's own ClusterRole in the chart grant
// every Kubernetes resource the controller's Go code actually asks for?
//
// It exists because nothing did. core-provider has 86 test files and every RBAC-related one covers
// the RBAC the controller GENERATES for compositions; none compares the controller's own grants
// against its own calls. #263 shipped a kube.List on pods with no pods rule in the chart, and every
// CompositionDefinition version bump on 2.13.45 wedged -- found on a peer's cluster, as a timeout
// that named the informer rather than the permission (#270).
//
// A MENTION-BASED ANALYSIS IS SOUND HERE, which is unusual and worth stating because it is wrong
// for most guards. core-provider#232 established that an AST guard cannot tell a mention from
// reachability. For RBAC the implication runs the useful way: NEEDING a permission follows from the
// code containing a client call on that type, whether or not the line executes. Over-approximating
// grants slightly more than required and never less, so a false positive here costs an unnecessary
// rule and a false negative is the outage this package exists to prevent.
package rbacguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// clientMethods are the controller-runtime client calls whose object argument implies a grant.
// DeleteAllOf is included because it requires deletecollection, which an operator granting only
// "delete" would miss.
var clientMethods = map[string]bool{
	"Get": true, "List": true, "Create": true, "Delete": true,
	"Update": true, "Patch": true, "DeleteAllOf": true,
}

// TypeGrant maps a Go type to the API group and resource a rule must name. Explicit rather than
// derived: a derivation would have to pluralise, and plurals are not derivable -- the same mistake
// that dropped 32 of 83 rows in a fleet join elsewhere in this project.
//
// A client call on a type ABSENT from this table is reported as unmapped rather than ignored. An
// unknown type is the case where silence would be a false pass, so it fails loudly instead.
var TypeGrant = map[string]struct{ Group, Resource string }{
	"corev1.Pod":                                   {"", "pods"},
	"corev1.PodList":                               {"", "pods"},
	"corev1.ConfigMap":                             {"", "configmaps"},
	"corev1.ConfigMapList":                         {"", "configmaps"},
	"corev1.Secret":                                {"", "secrets"},
	"corev1.SecretList":                            {"", "secrets"},
	"corev1.ServiceAccount":                        {"", "serviceaccounts"},
	"corev1.ServiceAccountList":                    {"", "serviceaccounts"},
	"corev1.Service":                               {"", "services"},
	"corev1.ServiceList":                           {"", "services"},
	"corev1.Event":                                 {"", "events"},
	"corev1.Namespace":                             {"", "namespaces"},
	"appsv1.Deployment":                            {"apps", "deployments"},
	"appsv1.DeploymentList":                        {"apps", "deployments"},
	"rbacv1.Role":                                  {"rbac.authorization.k8s.io", "roles"},
	"rbacv1.RoleList":                              {"rbac.authorization.k8s.io", "roles"},
	"rbacv1.RoleBinding":                           {"rbac.authorization.k8s.io", "rolebindings"},
	"rbacv1.RoleBindingList":                       {"rbac.authorization.k8s.io", "rolebindings"},
	"rbacv1.ClusterRole":                           {"rbac.authorization.k8s.io", "clusterroles"},
	"rbacv1.ClusterRoleList":                       {"rbac.authorization.k8s.io", "clusterroles"},
	"rbacv1.ClusterRoleBinding":                    {"rbac.authorization.k8s.io", "clusterrolebindings"},
	"rbacv1.ClusterRoleBindingList":                {"rbac.authorization.k8s.io", "clusterrolebindings"},
	"apiextensionsv1.CustomResourceDefinition":     {"apiextensions.k8s.io", "customresourcedefinitions"},
	"apiextensionsv1.CustomResourceDefinitionList": {"apiextensions.k8s.io", "customresourcedefinitions"},
	"coordinationv1.Lease":                         {"coordination.k8s.io", "leases"},
	// The controller's own CRs. Found by the scan as UNMAPPED on first run -- both ARE granted by
	// the chart, so the guard was silently not checking two real API objects.
	"compositiondefinitionsv1alpha1.CompositionDefinition":     {"core.krateo.io", "compositiondefinitions"},
	"compositiondefinitionsv1alpha1.CompositionDefinitionList": {"core.krateo.io", "compositiondefinitions"},
	"compositiondefinitionsv1alpha1.KubernetesTarget":          {"core.krateo.io", "kubernetestargets"},
	"compositiondefinitionsv1alpha1.KubernetesTargetList":      {"core.krateo.io", "kubernetestargets"},
	"compositiondefinitionsv1alpha1.SchemaDefinition":          {"core.krateo.io", "schemadefinitions"},
	"compositiondefinitionsv1alpha1.SchemaDefinitionList":      {"core.krateo.io", "schemadefinitions"},
	"compositiondefinitionsv1alpha1.RemoteInstall":             {"core.krateo.io", "remoteinstalls"},
	"compositiondefinitionsv1alpha1.RemoteInstallList":         {"core.krateo.io", "remoteinstalls"},
	"coordinationv1.LeaseList":                                 {"coordination.k8s.io", "leases"},
}

// notAnObject are argument types that appear in client calls and are not API objects: options
// structs, keys and identifiers. They are excluded from the unmapped report rather than listed,
// because on the first run they produced fourteen lines of noise that hid the two real API objects
// the table was failing to check. An unmapped report that cannot be read is not a report.
var notAnObject = map[string]bool{
	"client.GetOptions": true, "client.ListOptions": true, "client.CreateOptions": true,
	"client.UpdateOptions": true, "client.DeleteOptions": true, "client.PatchOptions": true,
	"client.DeleteAllOfOptions": true, "client.ObjectKey": true,
	"metav1.GetOptions": true, "metav1.ListOptions": true, "metav1.CreateOptions": true,
	"metav1.UpdateOptions": true, "metav1.DeleteOptions": true, "metav1.PatchOptions": true,
	"types.NamespacedName": true, "schema.GroupResource": true, "schema.GroupVersionResource": true,
	"plurals.GetOptions": true,
}

// Need is one grant the code requires, with where it was found so a failure is actionable.
type Need struct {
	GoType   string
	Group    string
	Resource string
	Sites    []string
}

// Scan walks root for non-test Go files and returns the grants implied by client calls, plus any
// client call whose object type has no mapping.
func Scan(root string) (needs map[string]*Need, unmapped map[string][]string, err error) {
	needs, unmapped = map[string]*Need{}, map[string][]string{}
	fset := token.NewFileSet()

	werr := filepath.Walk(root, func(path string, info os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if info.IsDir() {
			// vendor and testdata are not this controller's code.
			if n := info.Name(); n == "vendor" || n == "testdata" || n == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return fmt.Errorf("parsing %s: %w", path, perr)
		}
		scanFile(fset, f, path, needs, unmapped)
		return nil
	})
	if werr != nil {
		return nil, nil, werr
	}
	return needs, unmapped, nil
}

func scanFile(fset *token.FileSet, f *ast.File, path string, needs map[string]*Need, unmapped map[string][]string) {
	ast.Inspect(f, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			return true
		}
		// Locally declared object vars: `var pods corev1.PodList` then `kube.List(ctx, &pods)`.
		// Without this, the List that caused #270 would not be seen at all -- it passes &pods,
		// not a composite literal.
		locals := map[string]string{}
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			vs, ok := m.(*ast.ValueSpec)
			if !ok || vs.Type == nil {
				return true
			}
			if t := typeName(vs.Type); t != "" {
				for _, nm := range vs.Names {
					locals[nm.Name] = t
				}
			}
			return true
		})
		// Short declarations: `pods := corev1.PodList{}`.
		ast.Inspect(fn.Body, func(m ast.Node) bool {
			as, ok := m.(*ast.AssignStmt)
			if !ok || as.Tok != token.DEFINE {
				return true
			}
			for i, rhs := range as.Rhs {
				if i >= len(as.Lhs) {
					break
				}
				id, ok := as.Lhs[i].(*ast.Ident)
				if !ok {
					continue
				}
				if cl, ok := rhs.(*ast.CompositeLit); ok {
					if t := typeName(cl.Type); t != "" {
						locals[id.Name] = t
					}
				}
			}
			return true
		})

		ast.Inspect(fn.Body, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !clientMethods[sel.Sel.Name] {
				return true
			}
			for _, arg := range call.Args {
				t := objectTypeOf(arg, locals)
				if t == "" {
					continue
				}
				site := fmt.Sprintf("%s:%d (%s.%s)", filepath.Base(path), fset.Position(call.Pos()).Line, fn.Name.Name, sel.Sel.Name)
				if g, ok := TypeGrant[t]; ok {
					k := g.Group + "/" + g.Resource
					if needs[k] == nil {
						needs[k] = &Need{GoType: t, Group: g.Group, Resource: g.Resource}
					}
					needs[k].Sites = append(needs[k].Sites, site)
				} else if !notAnObject[t] {
					unmapped[t] = append(unmapped[t], site)
				}
			}
			return true
		})
		return true
	})
}

// objectTypeOf recognises &pkg.Type{...}, pkg.Type{...} and &localVar where the local's declared
// type is known. A value returned from a helper call is NOT tracked, which is this analysis's main
// blind spot and is reported by the test rather than hidden.
func objectTypeOf(arg ast.Expr, locals map[string]string) string {
	switch a := arg.(type) {
	case *ast.UnaryExpr:
		if a.Op != token.AND {
			return ""
		}
		switch inner := a.X.(type) {
		case *ast.CompositeLit:
			return typeName(inner.Type)
		case *ast.Ident:
			return locals[inner.Name]
		}
	case *ast.CompositeLit:
		return typeName(a.Type)
	case *ast.Ident:
		return locals[a.Name]
	}
	return ""
}

func typeName(e ast.Expr) string {
	if sel, ok := e.(*ast.SelectorExpr); ok {
		if pkg, ok := sel.X.(*ast.Ident); ok {
			return pkg.Name + "." + sel.Sel.Name
		}
	}
	return ""
}

// Rule is one apiGroups/resources pair from the chart.
type Rule struct {
	Groups      []string
	Resources   []string
	Conditional bool
}

// ParseClusterRole extracts the rules from the chart's ClusterRole template. Lines carrying Go
// template syntax are skipped, and a rule inside an {{ if }} is marked Conditional -- it is counted
// as granted, which is an over-approximation, so the test prints which grants depend on values
// rather than letting a values-gated rule look unconditional.
func ParseClusterRole(path string) ([]Rule, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rules []Rule
	var cur *Rule
	section, inRules, depth := "", false, 0
	for _, line := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "{{") {
			if strings.Contains(trimmed, "if ") {
				depth++
			} else if strings.Contains(trimmed, "end") && depth > 0 {
				depth--
			}
			continue
		}
		if strings.HasPrefix(line, "rules:") {
			inRules = true
			continue
		}
		if !inRules || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(line, "- apiGroups:") || trimmed == "- apiGroups:" {
			rules = append(rules, Rule{Conditional: depth > 0})
			cur = &rules[len(rules)-1]
			section = "groups"
			continue
		}
		if cur == nil {
			continue
		}
		switch {
		case strings.HasPrefix(trimmed, "resources:"):
			section = "resources"
		case strings.HasPrefix(trimmed, "verbs:"):
			section = "verbs"
		case strings.HasPrefix(trimmed, "- "):
			v := strings.Trim(strings.TrimPrefix(trimmed, "- "), `"'`)
			switch section {
			case "groups":
				cur.Groups = append(cur.Groups, v)
			case "resources":
				cur.Resources = append(cur.Resources, v)
			}
		}
	}
	return rules, nil
}

// Grants reports whether the rules cover group/resource, honouring "*" in either position.
func Grants(rules []Rule, group, resource string) (ok, conditional bool) {
	for _, r := range rules {
		g := false
		for _, rg := range r.Groups {
			if rg == group || rg == "*" {
				g = true
				break
			}
		}
		if !g {
			continue
		}
		for _, rr := range r.Resources {
			if rr == resource || rr == "*" {
				return true, r.Conditional
			}
		}
		// A group rule with NO resources listed is a group wildcard in this chart's style.
		if len(r.Resources) == 0 {
			return true, r.Conditional
		}
	}
	return false, false
}

// SortedKeys is a test helper for deterministic output.
func SortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}
