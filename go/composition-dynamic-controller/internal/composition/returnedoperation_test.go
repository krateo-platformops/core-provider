package composition

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	helmconfig "github.com/krateo-platformops/plumbing/helm"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// #497, Diego 2026-11-14: a lock left by an uninstall whose call RETURNED is cleared at once. One arm
// per input state; every arm but the first must keep the grace.
func TestReturnedOperation(t *testing.T) {
	podName, podNamespace = "fireworks-v1-controller", "krateo-system"
	t.Cleanup(func() { podName, podNamespace = "", "" })

	// The uninstall began at `updated` (helm's Info.Deleted); the revision was deployed hours before
	// (Info.LastDeployed). Distinct on purpose: measuring from the deploy is the #497 defect.
	updated := time.Date(2026, 11, 14, 9, 0, 10, 0, time.UTC)
	rel := &helmconfig.Release{Status: helmconfig.StatusUninstalling, Updated: updated.Add(-3 * time.Hour), Deleted: updated}
	started := updated.Add(-2 * time.Second)
	record := func(phase string, startedAt time.Time, returnedAt string) *unstructured.Unstructured {
		mg := &unstructured.Unstructured{Object: map[string]any{}}
		setHelmOperationRecord(mg, phase, startedAt)
		if returnedAt != "" {
			_ = unstructured.SetNestedField(mg.Object, returnedAt, "status", "helmOperation", "returnedAt")
		}
		return mg
	}
	ret := started.Add(5 * time.Second).Format(time.RFC3339Nano)

	marked := record("uninstall", started, "")
	markHelmOperationReturned(marked, started.Add(5*time.Second))

	for _, tc := range []struct {
		name string
		mg   *unstructured.Unstructured
		want bool
	}{
		{"this revision's uninstall, marked returned by markHelmOperationReturned", marked, true},
		{"no returnedAt: the call may still be running", record("uninstall", started, ""), false},
		{"another phase's record", record("rollback", started, ret), false},
		{"a stale record from an earlier operation", record("uninstall", updated.Add(-2*time.Hour), ret), false},
		{"a record written after the revision", record("uninstall", updated.Add(time.Second), ret), false},
		{"returnedAt precedes startedAt", record("uninstall", started, started.Add(-time.Second).Format(time.RFC3339Nano)), false},
		{"unreadable returnedAt", record("uninstall", started, "yesterday"), false},
		{"no record at all", &unstructured.Unstructured{Object: map[string]any{}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, why := returnedOperation(tc.mg, rel, "uninstall")
			if got != tc.want {
				t.Errorf("returned = %v (%s), want %v", got, why, tc.want)
			}
		})
	}

	if got, _ := returnedOperation(marked, nil, "uninstall"); got {
		t.Error("returned = true with no release")
	}
	// markHelmOperationReturned invents no record: with none there is nothing to mark.
	empty := &unstructured.Unstructured{Object: map[string]any{}}
	markHelmOperationReturned(empty, time.Now())
	if _, found, _ := unstructured.NestedMap(empty.Object, "status", "helmOperation"); found {
		t.Error("markHelmOperationReturned created a record where there was none")
	}
}

// The composition CRD's status schema is CLOSED under helmOperation, and core-provider generates it
// from statics/status.schema.json. A key this controller writes that the schema does not declare is
// pruned by the apiserver on every write, so whatever reads it back never engages, silently: the
// #497 fix would wait the grace exactly as before. The keys are taken from a record actually built by
// the writers, not from a hand list, and the list helmOperationRecordKeys must match them too.
func TestHelmOperationKeysAreDeclaredInTheGeneratedStatusSchema(t *testing.T) {
	podName, podNamespace = "p", "ns"
	t.Cleanup(func() { podName, podNamespace = "", "" })
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setHelmOperationRecord(mg, "uninstall", time.Now())
	markHelmOperationReturned(mg, time.Now())
	rec, _, _ := unstructured.NestedMap(mg.Object, "status", "helmOperation")
	var written []string
	for k := range rec {
		written = append(written, k)
	}
	sort.Strings(written)
	declaredList := append([]string(nil), helmOperationRecordKeys...)
	sort.Strings(declaredList)
	if strings.Join(written, ",") != strings.Join(declaredList, ",") {
		t.Fatalf("the writers produce %v but helmOperationRecordKeys says %v", written, declaredList)
	}

	path := filepath.Join("..", "..", "..", "core-provider", "internal", "tools", "crd", "generation", "statics", "status.schema.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading core-provider's status schema at %s: %v", path, err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	props := schema.Properties["helmOperation"].Properties
	if len(props) == 0 {
		t.Fatal("status.schema.json declares no helmOperation properties: the parse found nothing, which is not evidence the keys are declared")
	}
	for _, k := range written {
		if _, ok := props[k]; !ok {
			t.Errorf("status.helmOperation.%s is written by the controller but not declared in %s: the apiserver prunes it on every write", k, path)
		}
	}
}

// #497: an uninstalling revision is timed from helm's Info.Deleted, every other incomplete operation
// from Info.LastDeployed, and an uninstalling revision with no Deleted falls back to LastDeployed.
func TestOperationClock(t *testing.T) {
	deployed := time.Date(2026, 11, 14, 6, 0, 0, 0, time.UTC)
	uninstall := deployed.Add(3 * time.Hour)
	for _, tc := range []struct {
		name string
		rel  *helmconfig.Release
		want time.Time
	}{
		{"uninstalling: the uninstall's start", &helmconfig.Release{Status: helmconfig.StatusUninstalling, Updated: deployed, Deleted: uninstall}, uninstall},
		{"uninstalling with no Deleted: the deploy", &helmconfig.Release{Status: helmconfig.StatusUninstalling, Updated: deployed}, deployed},
		{"pending-upgrade: the pending revision's write", &helmconfig.Release{Status: helmconfig.StatusPendingUpgrade, Updated: deployed, Deleted: uninstall}, deployed},
	} {
		if got := operationClock(tc.rel); !got.Equal(tc.want) {
			t.Errorf("%s: operationClock = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// #461's orphan check on an UNINSTALL: before #497 it compared the record with the deploy time, so it
// could never match an uninstall (whose record is written hours after the deploy) and a stuck
// uninstall whose starter pod died always waited the full grace.
func TestOrphanedOperation_MatchesAnUninstallByItsStart(t *testing.T) {
	podName, podNamespace = "fireworks-v1-controller-new", "krateo-system"
	t.Cleanup(func() { podName, podNamespace = "", "" })
	uninstall := time.Date(2026, 11, 14, 9, 0, 10, 0, time.UTC)
	rel := &helmconfig.Release{Status: helmconfig.StatusUninstalling, Updated: uninstall.Add(-3 * time.Hour), Deleted: uninstall}
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	_ = unstructured.SetNestedMap(mg.Object, map[string]any{
		"phase": "uninstall", "startedAt": uninstall.Add(-2 * time.Second).Format(time.RFC3339Nano),
		"startedBy": "krateo-system/fireworks-v0-controller-old"}, "status", "helmOperation")
	gone := func(context.Context, string, string) error {
		return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "x")
	}
	if got, why := orphanedOperation(context.Background(), mg, rel, gone); !got {
		t.Errorf("orphaned = false (%s): an uninstall whose starter pod is gone must be recognised by its start, not its deploy", why)
	}
}
