package resources

import (
	"testing"

	"github.com/krateo-platformops/chart-inspector/internal/handlers/resources"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
)

// core-provider#311. The RBAC derivation sees a dry-run install's API calls, narrowed against the
// release Manifest. A dry run executes no hooks, and helm keeps hooks OUT of Manifest by
// construction, so no grant was ever generated for a hook object.
//
// The reporting cluster's controlled comparison: ONE template rendering FOUR objects —
// ServiceAccount, Role, RoleBinding, Job — of which only the Job carried `helm.sh/hook: pre-delete`.
// Three of four got grants. The hook did not, and `helm uninstall` could never succeed because helm
// must DELETE the previous hook object under before-hook-creation.

// The hook as helm hands it over: its own slice, its own manifest, phase in Events.
const teardownHook = `apiVersion: batch/v1
kind: Job
metadata:
  name: lab-ha-worker-blade06-teardown
  namespace: openstack
spec:
  template:
    spec:
      containers:
        - name: drain
          image: ironic-drain:1
`

func hooks() []helmconfig.Hook {
	return []helmconfig.Hook{{
		Name:     "teardown",
		Kind:     "Job",
		Path:     "templates/teardown-undeploy.yaml",
		Manifest: teardownHook,
		Events:   []string{"pre-delete"},
	}}
}

func TestHookObjects_GrantsTheHookWithTheManagementVerbSet(t *testing.T) {
	res, keys, unresolved := hookObjects(hooks(), "default", fakePluralizer{})

	if len(unresolved) != 0 {
		t.Fatalf("unresolved = %v, want none for a plain batch/v1 Job", unresolved)
	}
	if len(res) != 1 {
		t.Fatalf("got %d resources, want 1: without an entry there is no grant and the uninstall "+
			"can never succeed", len(res))
	}
	r := res[0]
	if r.Group != "batch" || r.Resource != "jobs" || r.Name != "lab-ha-worker-blade06-teardown" {
		t.Errorf("resource = %+v, want batch/jobs named after the hook Job", r)
	}
	// THE NAMESPACE COMES FROM THE MANIFEST, not the default. Kind alone carries neither apiVersion
	// nor namespace, which is why the projection had to carry Manifest at all.
	if r.Namespace != "openstack" {
		t.Errorf("namespace = %q, want openstack (from the hook's own manifest)", r.Namespace)
	}
	// THE VERB SET IS THE WHOLE FIX. `delete` is what was missing: before-hook-creation makes helm
	// delete the previous hook object, and without it the uninstall wedges forever.
	if len(r.Verbs) != 1 || r.Verbs[0] != "*" {
		t.Errorf("verbs = %v, want [\"*\"]. A read-only grant is exactly the state that wedged "+
			"teardown: the hook could be seen and not deleted", r.Verbs)
	}

	// Both namespace spellings recorded, for the same reason renderedObjects records both.
	if len(keys) != 2 {
		t.Errorf("got %d keys, want 2 (namespaced and cluster-scoped spellings)", len(keys))
	}
}

// THE TRAP, and the reason the keys exist at all.
//
// narrowLookupVerbs demotes any superset entry whose key it cannot find in the owned set — and a
// hook is absent from Manifest BY DEFINITION, so it is absent from renderedObjects' owned set.
// Appending the hook entries without also recording them as owned would see them narrowed to
// get/list/watch immediately, reintroducing the exact bug with extra steps.
func TestHookObjects_SurviveNarrowingOnlyBecauseTheyAreRecordedAsOwned(t *testing.T) {
	rendered, err := renderedObjects(manifest, "demo", fakePluralizer{})
	if err != nil {
		t.Fatalf("renderedObjects: %v", err)
	}
	hookRes, hookKeys, _ := hookObjects(hooks(), "default", fakePluralizer{})

	// ARM 1 — the defect: appended but NOT recorded as owned.
	got, narrowed := narrowLookupVerbs(append([]resources.Resource(nil), hookRes...), rendered)
	if narrowed != 1 {
		t.Fatalf("CONTROL: an unowned hook entry should be narrowed (narrowed=%d). If it is not, "+
			"this test cannot show that recording the keys is what protects it", narrowed)
	}
	if len(got[0].Verbs) == 1 && got[0].Verbs[0] == "*" {
		t.Error("CONTROL: the unowned entry kept the superset, so the arm below proves nothing")
	}

	// ARM 2 — the fix: recorded as owned first, then narrowed.
	for _, k := range hookKeys {
		rendered.owned[k] = struct{}{}
	}
	got, narrowed = narrowLookupVerbs(append([]resources.Resource(nil), hookRes...), rendered)
	if narrowed != 0 {
		t.Errorf("a hook recorded as chart-owned was still narrowed (narrowed=%d)", narrowed)
	}
	if len(got) != 1 || len(got[0].Verbs) != 1 || got[0].Verbs[0] != "*" {
		t.Errorf("verbs after narrowing = %v, want [\"*\"] preserved", got[0].Verbs)
	}
}

// Events are DELIBERATELY ignored. An object needs its permissions regardless of which phase fires
// it, and before-hook-creation makes helm delete a previous hook object whatever its phase.
// Filtering by event would grant a pre-delete hook nothing until a delete was already underway,
// which is the failure being fixed.
func TestHookObjects_PhaseIsIrrelevant(t *testing.T) {
	for _, events := range [][]string{
		{"pre-delete"}, {"post-install"}, {"pre-upgrade", "pre-rollback"}, nil,
	} {
		h := hooks()
		h[0].Events = events
		res, _, unresolved := hookObjects(h, "default", fakePluralizer{})
		if len(unresolved) != 0 || len(res) != 1 {
			t.Errorf("events=%v produced res=%d unresolved=%v; the phase must not change whether a "+
				"hook object is granted", events, len(res), unresolved)
		}
	}
}

// An unresolvable kind is REPORTED rather than swallowed, so an operator learns at install time
// instead of at teardown. This is the loud failure the rejected alternative (refusing any chart
// with uncoverable hooks) was reaching for — without rejecting charts that work today because an
// ordinary resource happens to need the same kind.
func TestHookObjects_UnresolvableKindIsReportedNotSilent(t *testing.T) {
	h := []helmconfig.Hook{{
		Name: "custom", Kind: "Widget", Path: "templates/widget-hook.yaml",
		Manifest: "apiVersion: example.com/v1\nkind: Widget\nmetadata:\n  name: w1\n",
		Events:   []string{"post-install"},
	}}
	res, keys, unresolved := hookObjects(h, "default", fakePluralizer{unresolvable: map[string]bool{"Widget": true}})

	if len(unresolved) != 1 {
		t.Fatalf("unresolved = %v, want exactly 1 report naming the hook", unresolved)
	}
	// The report must name the TEMPLATE FILE, or an operator cannot act on it.
	if !contains(unresolved[0], "templates/widget-hook.yaml") {
		t.Errorf("report %q does not name the template path; an operator told \"a hook has no "+
			"RBAC\" needs to know which file", unresolved[0])
	}
	if len(res) != 0 || len(keys) != 0 {
		t.Errorf("an unresolvable hook must contribute no entries (res=%d keys=%d): granting a "+
			"resource we cannot name is worse than reporting that we cannot name it", len(res), len(keys))
	}
}

// A chart with no hooks must behave exactly as before — no entries, no reports, no noise.
func TestHookObjects_NoHooksIsNoChange(t *testing.T) {
	res, keys, unresolved := hookObjects(nil, "default", fakePluralizer{})
	if len(res) != 0 || len(keys) != 0 || len(unresolved) != 0 {
		t.Errorf("a release with no hooks produced res=%d keys=%d unresolved=%v", len(res), len(keys), unresolved)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
