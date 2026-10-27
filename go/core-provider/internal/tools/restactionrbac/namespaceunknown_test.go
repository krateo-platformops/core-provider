package restactionrbac

import (
	"encoding/json"
	"reflect"
	"testing"
)

// A /rbac 200 body carrying the flag snowplow ships as of 1.12.38.
const readSetWithNamespaceUnknown = `{
  "restaction": {"name": "pods-reader", "namespace": "demo-system"},
  "readSet": [
    {"group": "apps", "version": "v1", "resource": "deployments", "verb": "list", "namespaceUnknown": true},
    {"group": "", "version": "v1", "resource": "nodes", "verb": "list"}
  ]
}`

// The flag must survive decode. It was dropped before #164, which is the whole defect: an empty
// Namespace is byte-identical to a genuinely cluster-scoped row, so once the flag is gone nothing
// downstream can tell "namespaced, scope unknown" from "cluster-scoped".
func TestResource_NamespaceUnknownSurvivesDecode(t *testing.T) {
	var body response
	if err := json.Unmarshal([]byte(readSetWithNamespaceUnknown), &body); err != nil {
		t.Fatalf("decoding: %v", err)
	}
	if len(body.ReadSet) != 2 {
		t.Fatalf("decoded %d rows, want 2", len(body.ReadSet))
	}
	if !body.ReadSet[0].NamespaceUnknown {
		t.Error("namespaceUnknown was dropped at decode: a namespaced read is now indistinguishable " +
			"from a cluster-scoped one, which is exactly what #164 is about")
	}
	// Control arm: the genuinely cluster-scoped row must NOT come back flagged, or the assertion
	// above would pass for a struct that hardcodes true.
	if body.ReadSet[1].NamespaceUnknown {
		t.Error("a cluster-scoped row decoded as namespace-unknown")
	}
}

// The two fields this struct has historically dropped must stay modelled. Both were added after
// the fact; this fails if either is removed.
func TestReadSetFields_ModelsTheFieldsThatWereDroppedBefore(t *testing.T) {
	known := readSetFields()
	for _, f := range []string{"nonReadVerb", "namespaceUnknown", "group", "version", "resource", "namespace", "verb"} {
		if !known[f] {
			t.Errorf("read-set field %q is not modelled by Resource", f)
		}
	}
}

// The instrument #164 asks for: a snowplow field this build does not model must be NAMED, not
// silently discarded. Both historical gaps (nonReadVerb, namespaceUnknown) were found by reading
// code, because dropping an unknown JSON key is exactly what encoding/json is specified to do.
func TestUnknownReadSetFields_NamesWhatThisBuildWouldDrop(t *testing.T) {
	raw := `{"readSet": [
	  {"group": "apps", "version": "v1", "resource": "deployments", "verb": "list",
	   "subresource": "status", "fieldSelector": "spec.nodeName=x"}
	]}`
	got := unknownReadSetFields([]byte(raw))
	want := []string{"fieldSelector", "subresource"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// Control arm for the detector. A payload using only modelled fields must report nothing —
// otherwise the Info line fires on every call and is ignored within a day, which is the same
// silence it exists to break.
func TestUnknownReadSetFields_QuietWhenEverythingIsModelled(t *testing.T) {
	if got := unknownReadSetFields([]byte(readSetWithNamespaceUnknown)); len(got) != 0 {
		t.Fatalf("reported %v on a fully-modelled payload", got)
	}
}

// A body that is not valid JSON must not panic or invent fields; the typed decode reports the real
// error.
func TestUnknownReadSetFields_IgnoresUndecodableBodies(t *testing.T) {
	if got := unknownReadSetFields([]byte(`{"readSet": "not-an-array"`)); got != nil {
		t.Fatalf("reported %v on a malformed body", got)
	}
}

// The #164 defect itself, at the point it would bite.
//
// Generate maps Namespace == "" to a ClusterRole. A namespaced row whose namespace is merely
// unknown looks identical, so moving off GenerateClusterScoped (the Phase 3 refinement that
// generate.go's own comment describes) would have widened it to cluster-wide — arriving as an
// inherited side effect of a refactor rather than as a decision anyone made.
func TestGenerate_NamespaceUnknownIsDeferredNotClusterWidened(t *testing.T) {
	row := Resource{Group: "apps", Version: "v1", Resource: "deployments", Verb: "list"}

	// Control arm FIRST: the identical row without the flag is genuinely cluster-scoped and must
	// still produce a ClusterRole. Without this, the assertion below would pass for a Generate
	// that had simply stopped emitting ClusterRoles at all.
	clusterScoped := Generate([]Resource{row}, "krateo:cdc:test", "test")
	if clusterScoped.ClusterRole == nil {
		t.Fatal("control arm: a genuinely cluster-scoped read no longer produces a ClusterRole, " +
			"so this test can no longer tell widening from a broken generator")
	}
	if clusterScoped.Deferred.Count != 0 {
		t.Fatalf("control arm: an unflagged row was deferred (%d)", clusterScoped.Deferred.Count)
	}

	unknownNS := row
	unknownNS.NamespaceUnknown = true
	got := Generate([]Resource{unknownNS}, "krateo:cdc:test", "test")

	if got.ClusterRole != nil {
		t.Errorf("a namespaced read with an undetermined namespace was granted cluster-wide: %+v",
			got.ClusterRole.Rules)
	}
	if got.ClusterRoleBinding != nil {
		t.Error("a ClusterRoleBinding was minted for a row with no determined namespace")
	}
	if got.Deferred.Count != 1 {
		t.Errorf("Deferred.Count = %d, want 1 — the grant is owed, not refused", got.Deferred.Count)
	}
	// It is owed, not refused: conflating the two is what loses the distinction #164 is about.
	if got.Dropped.Count != 0 {
		t.Errorf("Dropped.Count = %d, want 0 — a deferred read is not a refused one", got.Dropped.Count)
	}
}

// Deferral must not swallow the gate that matters more. A non-read verb stays REFUSED even when
// its namespace is unknown — otherwise a userAccessFilter row could be re-granted later as an
// "owed" namespaced grant, which is the snowplow#179 inversion wearing a new hat.
func TestGenerate_NonReadVerbStaysDroppedEvenWhenNamespaceIsUnknown(t *testing.T) {
	row := Resource{
		Group: "apps", Version: "v1", Resource: "deployments", Verb: "create",
		NonReadVerb: true, NamespaceUnknown: true,
	}
	got := Generate([]Resource{row}, "krateo:cdc:test", "test")

	if got.Dropped.Count != 1 {
		t.Errorf("Dropped.Count = %d, want 1: a userAccessFilter verb must be refused outright", got.Dropped.Count)
	}
	if got.Deferred.Count != 0 {
		t.Errorf("Deferred.Count = %d, want 0: a non-read verb must never become an owed grant", got.Deferred.Count)
	}
	if got.ClusterRole != nil {
		t.Error("a non-read verb reached a ClusterRole")
	}
}
