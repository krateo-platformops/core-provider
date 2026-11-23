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

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clienttesting "k8s.io/client-go/testing"
)

// infoCounter counts Info lines.
type infoCounter struct {
	logging.Logger
	lines []string
}

func (l *infoCounter) Info(msg string, _ ...any) { l.lines = append(l.lines, msg) }

// The count is cumulative; a count that cannot be read stops the pass.
func TestRecordInstallRecovery(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 23, 21, 0, 0, 0, time.UTC)
	dyn := frFakeDyn(ndPortal(""))
	mg := ndPortal("")
	var err error
	for i := 0; i < 3; i++ {
		if mg, err = recordInstallRecovery(ctx, mg, "because", now.Add(time.Duration(i)*time.Minute), srOpts(dyn), logging.NewNopLogger()); err != nil {
			t.Fatal(err)
		}
	}
	stored, err := dyn.Resource(srGVR).Namespace("krateo-system").Get(ctx, "portal", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _, _ := unstructured.NestedMap(stored.Object, "status", "installRecovery")
	if got["kind"] != installRecoveryRetried || got["count"] != int64(3) || got["lastAt"] != "2026-11-23T21:02:00Z" || got["reason"] != "because" {
		t.Errorf("stored record = %v", got)
	}
	if marker, _, _ := unstructured.NestedString(stored.Object, "status", "marker"); marker != "x" {
		t.Errorf("the write lost another status field: marker %q", marker)
	}

	bad := ndPortal("")
	_ = unstructured.SetNestedField(bad.Object, "many", "status", "installRecovery", "count")
	if _, err := recordInstallRecovery(ctx, bad, "r", now, srOpts(dyn), logging.NewNopLogger()); err == nil {
		t.Error("a count that cannot be read was taken for zero")
	}
}

// A CRD that predates the field prunes it: the write succeeds without it. The recovery goes ahead, pass
// after pass, and the log says so once per composition.
func TestRecordInstallRecovery_PrunedByAnOlderCRD(t *testing.T) {
	ctx := context.Background()
	dyn := frFakeDyn(ndPortal(""))
	dyn.PrependReactor("update", "portals", func(a clienttesting.Action) (bool, runtime.Object, error) {
		u := a.(clienttesting.UpdateAction).GetObject().(*unstructured.Unstructured).DeepCopy()
		unstructured.RemoveNestedField(u.Object, "status", "installRecovery")
		return true, u, nil
	})
	log := &infoCounter{Logger: logging.NewNopLogger()}
	mg := ndPortal("")
	mg.SetUID(types.UID("pruned-" + t.Name()))
	var err error
	for i := 0; i < 3; i++ {
		if mg, err = recordInstallRecovery(ctx, mg, "r", time.Now(), srOpts(dyn), log); err != nil {
			t.Fatalf("pass %d: a pruned record failed the recovery: %v", i+1, err)
		}
	}
	if len(log.lines) != 1 || !strings.Contains(log.lines[0], "pruned") {
		t.Errorf("log lines %v, want one that says it was pruned", log.lines)
	}
	// Control: a record that persists says nothing.
	log.lines = nil
	if _, err := recordInstallRecovery(ctx, ndPortal(""), "r", time.Now(), srOpts(frFakeDyn(ndPortal(""))), log); err != nil || len(log.lines) != 0 {
		t.Errorf("control: err=%v lines=%v", err, log.lines)
	}
}

// The status a later pass writes keeps the record: setStatus, which rebuilds most of the status, and the
// operation record's own writes.
func TestInstallRecoverySurvivesLaterStatusWrites(t *testing.T) {
	ctx := context.Background()
	dyn := frFakeDyn(ndPortal(""))
	mg, err := recordInstallRecovery(ctx, ndPortal(""), "because", time.Now(), srOpts(dyn), logging.NewNopLogger())
	if err != nil {
		t.Fatal(err)
	}
	if mg, err = recordHelmOperation(ctx, mg, "install", time.Now(), srOpts(dyn), logging.NewNopLogger()); err != nil {
		t.Fatal(err)
	}
	mg = settleHelmOperationRecord(ctx, mg, nil, nil, srOpts(dyn), logging.NewNopLogger())
	h := &handler{}
	if err := h.setStatus(ctx, mg, &statusManagerOpts{force: true, digest: "d", releaseName: "r", releaseStatus: "deployed", releaseRevision: 2,
		recordManaged: true, conditionType: ConditionTypeCreating}); err != nil {
		t.Fatal(err)
	}
	if _, err := updateStatus(ctx, mg, srOpts(dyn)); err != nil {
		t.Fatal(err)
	}
	stored, err := dyn.Resource(srGVR).Namespace("krateo-system").Get(ctx, "portal", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if d, _, _ := unstructured.NestedString(stored.Object, "status", "digest"); d != "d" {
		t.Fatalf("control: setStatus's write did not land: digest %q", d)
	}
	if n, err := installRecoveryCount(stored); n != 1 || err != nil {
		t.Errorf("after setStatus and the operation record's writes, installRecovery.count = %d (%v)", n, err)
	}
}

// As TestHelmOperationKeysAreDeclaredInTheGeneratedStatusSchema: the keys the WRITER produces, against
// the closed schema core-provider generates composition CRDs from.
func TestInstallRecoveryKeysAreDeclaredInTheGeneratedStatusSchema(t *testing.T) {
	mg, err := recordInstallRecovery(context.Background(), ndPortal(""), "r", time.Now(), srOpts(frFakeDyn(ndPortal(""))), logging.NewNopLogger())
	if err != nil {
		t.Fatal(err)
	}
	rec, _, _ := unstructured.NestedMap(mg.Object, "status", "installRecovery")
	var written []string
	for k := range rec {
		written = append(written, k)
	}
	sort.Strings(written)
	want := append([]string(nil), installRecoveryKeys...)
	sort.Strings(want)
	if strings.Join(written, ",") != strings.Join(want, ",") {
		t.Fatalf("the writer produces %v but installRecoveryKeys says %v", written, want)
	}
	path := filepath.Join("..", "..", "..", "core-provider", "internal", "tools", "crd", "generation", "statics", "status.schema.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Properties map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	props := schema.Properties["installRecovery"].Properties
	if len(props) == 0 {
		t.Fatal("status.schema.json declares no installRecovery properties: the apiserver prunes every one of them")
	}
	for _, k := range written {
		if _, ok := props[k]; !ok {
			t.Errorf("status.installRecovery.%s is written but not declared in %s: the apiserver prunes it on every write", k, path)
		}
	}
}
