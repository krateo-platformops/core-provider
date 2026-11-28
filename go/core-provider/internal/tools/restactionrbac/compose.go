package restactionrbac

import "sort"

// ClusterWide names one row that stays in the definition's ClusterRole because no composition's
// extras can resolve its namespace: the namespace comes from a prior stage's output, which exists
// only while the RESTAction runs (#691). It carries the group, resource and verb only.
type ClusterWide struct {
	Group    string `json:"group"`
	Resource string `json:"resource"`
	Verb     string `json:"verb"`
}

// Composed is Generate's result for a whole definition, plus the rows it kept cluster-wide.
type Composed struct {
	Generated
	// ClusterWide lists, sorted and de-duplicated, the namespaced rows that stayed cluster-wide
	// because they were still namespaceUnknown for a composition.
	ClusterWide []ClusterWide
}

// Namespaces are the namespaces holding a Role, sorted.
func (g Generated) Namespaces() []string {
	out := make([]string, 0, len(g.Roles))
	for i := range g.Roles {
		out = append(out, g.Roles[i].Namespace)
	}
	return out
}

// Compose turns a definition's read-sets into group-bound RBAC (core-provider#691).
//
// base is the read-set resolved with the definition's static extras alone: only its cluster-scoped
// rows are used (they do not depend on a composition). Its namespaced and namespaceUnknown rows are
// ignored, because each composition's own read-set carries them with the namespace its extras give.
//
// perComposition holds one read-set per composition, resolved with apiref.CompositionExtras. A row
// with a concrete namespace becomes a rule of the Role in THAT namespace, whichever composition it
// came from: the Roles are the union of every composition's rows landing there. A row still
// namespaceUnknown is the residual: nothing can resolve it before the RESTAction reads, so it keeps a
// cluster-wide grant (Diego, 2026-11-28), and is named in ClusterWide.
//
// A concrete namespaced row never reaches the ClusterRole: only rows with no namespace at all, and
// the residual, are put there.
func Compose(base []Resource, perComposition [][]Resource, group, name string) Composed {
	var rows []Resource
	for _, r := range base {
		if r.NamespaceUnknown || r.Namespace != "" {
			continue
		}
		rows = append(rows, r)
	}
	seen := map[ClusterWide]bool{}
	for _, set := range perComposition {
		for _, r := range set {
			if r.NamespaceUnknown && grantable(r) {
				seen[ClusterWide{Group: r.Group, Resource: r.Resource, Verb: r.Verb}] = true
				r.NamespaceUnknown, r.Namespace = false, ""
			}
			rows = append(rows, r)
		}
	}
	out := Composed{Generated: Generate(rows, group, name)}
	for cw := range seen {
		out.ClusterWide = append(out.ClusterWide, cw)
	}
	sort.Slice(out.ClusterWide, func(i, j int) bool {
		a, b := out.ClusterWide[i], out.ClusterWide[j]
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		if a.Resource != b.Resource {
			return a.Resource < b.Resource
		}
		return a.Verb < b.Verb
	})
	return out
}
