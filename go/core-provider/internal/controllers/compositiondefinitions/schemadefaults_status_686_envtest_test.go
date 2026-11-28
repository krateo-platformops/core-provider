//go:build envtest

package compositiondefinitions

import (
	"encoding/json"
	"sync/atomic"
	"testing"

	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/writtenby"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// #686: status.schemaDefaults on a real apiserver. The CRD is the generated one (crds/), so a field
// it did not declare would be pruned: the expectation is read from what the apiserver STORED, and
// passes that change nothing else must leave resourceVersion unchanged (the field is set on the
// in-memory object in the pass that writes the status anyway).
func TestEnvtest_686_SchemaDefaultsStatusCausesNoExtraWrites(t *testing.T) {
	e := start644(t)
	// start644's charts carry values.yaml "{}", so this schema default has no values.yaml key.
	e.charts.add(t, "defaults", "1.0.0", `{"type":"object","properties":{"flag":{"type":"boolean","default":true}}}`)
	d := e.define(t, "defaults", "defaults", nil)
	var updates atomic.Int32
	d.r = e.reconciler669(writtenby.New("2.13.137", "c0ffee137"), &updates)
	d.rest(6)

	got := d.cd().Status.SchemaDefaults
	if got == nil || got.Dropped != 0 || len(got.Differ) != 1 {
		t.Fatalf("status.schemaDefaults = %+v, want one entry", got)
	}
	if g := got.Differ[0]; g.Path != "flag" || g.Kind != v1alpha1.SchemaDefaultWithoutValuesKey || g.Values != nil || g.Schema == nil || string(g.Schema.Raw) != "true" {
		t.Fatalf("entry = %+v, want path flag, schema true, no values, kind schemaDefaultWithoutValuesKey", g)
	}
	// What the apiserver stored (not pruned, JSON values not strings).
	u, err := e.c.dyn.Resource(cdGVR644).Namespace(self642CDNS).Get(t.Context(), "defaults", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	sd, _ := u.Object["status"].(map[string]any)["schemaDefaults"].(map[string]any)
	b, _ := json.Marshal(sd)
	if want := `{"differ":[{"kind":"schemaDefaultWithoutValuesKey","path":"flag","schema":true}],"dropped":0}`; string(b) != want {
		t.Fatalf("stored status.schemaDefaults = %s, want %s", b, want)
	}

	rv := d.cd().ResourceVersion
	updates.Store(0)
	const passes = 3
	for i := 0; i < passes; i++ {
		if _, err := d.pass(); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d passes sent %d status updates; resourceVersion %s -> %s", passes, updates.Load(), rv, d.cd().ResourceVersion)
	if updates.Load() < passes {
		t.Fatalf("control: %d status updates in %d passes; the passes did not reach the status write, so an unchanged resourceVersion would prove nothing", updates.Load(), passes)
	}
	if got := d.cd().ResourceVersion; got != rv {
		t.Errorf("at rest the definition is still being written: resourceVersion %s -> %s", rv, got)
	}
}
