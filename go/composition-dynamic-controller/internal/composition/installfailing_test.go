package composition

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/krateo-platformops/composition-dynamic-controller/internal/conditionreason"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	"github.com/krateo-platformops/unstructured-runtime/pkg/controller"
	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func readyOf636(u *unstructured.Unstructured) metav1.Condition {
	for _, c := range unstructuredtools.GetConditions(u) {
		if c.Type == condition.TypeReady {
			return c
		}
	}
	return metav1.Condition{}
}

func storedPortal(t *testing.T, dyn *dynamicfake.FakeDynamicClient) *unstructured.Unstructured {
	t.Helper()
	u, err := dyn.Resource(srGVR).Namespace("krateo-system").Get(context.Background(), "portal", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// The whole life of a first install that keeps failing, each step starting from the object a RESTARTED
// controller reads: the first two failures are Creating, the third on is InstallFailing; what is written
// before and during a retry keeps it; an interrupted retry is counted as a retry and not as a failure;
// both numbers in the message are the stored ones at every write; and it clears.
func TestInstallFailing(t *testing.T) {
	ctx := context.Background()
	dyn := frFakeDyn(ndPortal(""))
	opts, log := srOpts(dyn), logging.NewNopLogger()
	now := time.Date(2026, 11, 23, 22, 0, 0, 0, time.UTC)
	check := func(step string, wantReason string, wantFailed, wantRetries int64, wantIn ...string) {
		t.Helper()
		u := storedPortal(t, dyn)
		r := readyOf636(u)
		failed := installFailureCount(u)
		retries, err := installRecoveryCount(u)
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != metav1.ConditionFalse || r.Reason != wantReason || failed != wantFailed || retries != wantRetries {
			t.Errorf("%s: Ready %s/%s failed %d retries %d; want False/%s %d %d", step, r.Status, r.Reason, failed, retries, wantReason, wantFailed, wantRetries)
		}
		if wantReason == conditionreason.ReasonInstallFailing {
			wantIn = append(wantIn, fmt.Sprintf("the first install has failed %d times (%d automatic retries so far); last error: ", failed, retries))
		}
		for _, w := range wantIn {
			if !strings.Contains(r.Message, w) {
				t.Errorf("%s: message %q lacks %q", step, r.Message, w)
			}
		}
	}
	fail := func(n int) {
		t.Helper()
		_, err := recordInstallFailure(ctx, storedPortal(t, dyn), fmt.Errorf("hook failed, attempt %d", n), now, opts, log)
		var f *installFailed
		if !errors.As(err, &f) || !f.recorded || err.Error() != fmt.Sprintf("hook failed, attempt %d", n) {
			t.Fatalf("attempt %d: %v", n, err)
		}
	}
	revision := 0
	retry := func(reason string) {
		t.Helper()
		revision++ // every attempt that starts writes one
		mg, err := recordInstallRecovery(ctx, storedPortal(t, dyn), revision, reason, now, opts, log)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := recordHelmOperation(ctx, mg, "install", now, opts, log); err != nil {
			t.Fatal(err)
		}
	}

	fail(1)
	check("1st failure", condition.ReasonCreating, 1, 0, "hook failed, attempt 1")
	retry("Failed: x")
	check("during the 1st retry", conditionreason.ReasonHelmOperationInProgress, 1, 1)
	fail(2)
	check("2nd failure", condition.ReasonCreating, 2, 1, "hook failed, attempt 2")
	retry("Interrupted: x") // interrupted: no failure
	check("during an interrupted retry", conditionreason.ReasonHelmOperationInProgress, 2, 2)
	retry("Failed: x")
	fail(3)
	check("3rd failure", conditionreason.ReasonInstallFailing, 3, 3, "hook failed, attempt 3")
	if lastErr, _, _ := unstructured.NestedString(storedPortal(t, dyn).Object, "status", "installFailures", "lastError"); lastErr != "hook failed, attempt 3" {
		t.Errorf("lastError %q", lastErr)
	}

	// Recording the next retry moves the count and the message that names it in one write.
	if _, err := recordInstallRecovery(ctx, storedPortal(t, dyn), revision+1, "Failed: x", now, opts, log); err != nil {
		t.Fatal(err)
	}
	check("the 4th retry is recorded", conditionreason.ReasonInstallFailing, 3, 4, "hook failed, attempt 3", "a retry is starting")
	if _, err := recordHelmOperation(ctx, storedPortal(t, dyn), "install", now, opts, log); err != nil {
		t.Fatal(err)
	}
	check("during the 4th retry", conditionreason.ReasonInstallFailing, 3, 4, "hook failed, attempt 3", "a retry is running: helm install in progress since 2026-11-23T22:00:00Z")
	// Control: another operation on the same composition is not a retry of the install.
	if _, err := recordHelmOperation(ctx, storedPortal(t, dyn), "uninstall", now, opts, log); err != nil {
		t.Fatal(err)
	}
	check("control: an uninstall", conditionreason.ReasonHelmOperationInProgress, 3, 4)
	// A retry that does not start: the reason stands.
	if err := failInstallAgain(ctx, storedPortal(t, dyn), errors.New("a hook Job is still there"), opts); !controller.IsReadyAssessed(err) {
		t.Errorf("err = %v, want ReadyAssessed", err)
	}
	check("a retry that did not start", conditionreason.ReasonInstallFailing, 3, 4, "the next retry did not start: a hook Job is still there")
	fail(4)
	check("4th failure", conditionreason.ReasonInstallFailing, 4, 4, "hook failed, attempt 4")

	mg := storedPortal(t, dyn)
	clearInstallFailures(mg)
	if _, err := updateStatus(ctx, mg, opts); err != nil {
		t.Fatal(err)
	}
	u := storedPortal(t, dyn)
	if n, _ := installRecoveryCount(u); installFailureCount(u) != 0 || n != 4 {
		t.Errorf("after a deploy: failed %d, retries %d; want 0 and 4: the retries are never reset", installFailureCount(u), n)
	}
	fail(1)
	check("the first failure after a deploy", condition.ReasonCreating, 1, 4)
}

// What is not a failed attempt, and what a write that fails does.
func TestRecordInstallFailure_NotCounted(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		err   error
		react func(*dynamicfake.FakeDynamicClient)
	}{
		{"nil", context.Background(), nil, nil},
		{"refused: the release moved on", context.Background(), errRetryRefused, nil},
		{"refused: another operation holds it", context.Background(), fmt.Errorf("x: %w", helmconfig.ErrOperationInProgress), nil},
		{"refused: the name is in use", context.Background(), errors.New("install failed: cannot re-use a name that is still in use"), nil},
		{"cancelled by a graceful stop", cancelled, errors.New("context canceled"), nil},
		{"the status write loses a race: not counted, and not claimed to be recorded", context.Background(), errors.New("hook failed"),
			failNthStatusWrite(1, apierrors.NewConflict(schema.GroupResource{Group: srGVR.Group, Resource: srGVR.Resource}, "portal", nil))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dyn := frFakeDyn(ndPortal(""))
			if tc.react != nil {
				tc.react(dyn)
			}
			mg, err := recordInstallFailure(tc.ctx, ndPortal(""), tc.err, time.Now(), srOpts(dyn), logging.NewNopLogger())
			if mg == nil || (tc.err != nil && !strings.Contains(err.Error(), tc.err.Error())) || (tc.err == nil && err != nil) {
				t.Fatalf("mg %v err %v", mg, err)
			}
			var f *installFailed
			if errors.As(err, &f) && f.recorded {
				t.Error("claims to be recorded")
			}
			if n := installFailureCount(storedPortal(t, dyn)); n != 0 {
				t.Errorf("stored count %d", n)
			}
		})
	}
	// A failure that was not recorded leaves Ready to failInstallAgain's finding.
	dyn := frFakeDyn(ndPortal(""))
	if err := failInstallAgain(context.Background(), ndPortal(""), &installFailed{err: errors.New("x")}, srOpts(dyn)); !controller.IsReadyAssessed(err) {
		t.Errorf("err = %v", err)
	}
	if r := readyOf636(storedPortal(t, dyn)); r.Reason != condition.ReasonUnavailable {
		t.Errorf("Ready %s", r.Reason)
	}
	// One that was: its own Ready stands, and is not written again.
	dyn = frFakeDyn(ndPortal(""))
	if err := failInstallAgain(context.Background(), ndPortal(""), fmt.Errorf("w: %w", &installFailed{err: errors.New("x"), recorded: true}), srOpts(dyn)); !controller.IsReadyAssessed(err) {
		t.Errorf("err = %v", err)
	}
	if r := readyOf636(storedPortal(t, dyn)); r.Reason != "" {
		t.Errorf("Ready was written again: %s", r.Reason)
	}
}

func TestBoundedError(t *testing.T) {
	if got := boundedError(errors.New(" short \n")); got != "short" {
		t.Errorf("%q", got)
	}
	long := boundedError(errors.New(strings.Repeat("è", maxLastErrorBytes)))
	if len(long) > maxLastErrorBytes+3 || !strings.HasSuffix(long, "è...") {
		t.Errorf("%d bytes, ends %q", len(long), long[len(long)-8:])
	}
}

// The record of a failed attempt, and the operation's: while the never-deployed mark could not be written,
// the operation record is the only proof the install is this controller's, so it stays.
func TestAfterFailedInstall_KeepsWhatProvesTheInstall(t *testing.T) {
	started := time.Now()
	mine := rev(1, helmconfig.StatusFailed)
	mine.Updated = started.Add(time.Second)
	for _, tc := range []struct {
		name       string
		deny       bool
		wantRecord bool
	}{{"marked: the record goes", false, false}, {"the mark could not be written: the record stays", true, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dyn := frFakeDyn(ndPortal(""))
			if tc.deny {
				denyAnnotationPatch(dyn)
			}
			hc := &ownFailureClient{rel: mine, history: []*helmconfig.Release{mine}}
			_, err := afterFailedInstall(context.Background(), ndPortal(""), hc, "r", started, errors.New("hook failed"), srOpts(dyn), logging.NewNopLogger())
			if err == nil {
				t.Fatal("no error")
			}
			u := storedPortal(t, dyn)
			if ok, why := startedByController(u, "r", mine); !ok {
				t.Errorf("the install is no longer provably this controller's: %s", why)
			}
			if _, found, _ := unstructured.NestedMap(u.Object, "status", "helmOperation"); found != tc.wantRecord {
				t.Errorf("operation record stored %v, want %v", found, tc.wantRecord)
			}
			if installFailureCount(u) != 1 {
				t.Error("not counted")
			}
		})
	}
}

// A child whose first install keeps failing is FAILED in its parent, named by its reason only. CONTROLS:
// the same child Creating is converging; a True with the same reason string is not this arm.
func TestRollup_636_InstallFailing(t *testing.T) {
	for _, tc := range []struct {
		status, reason string
		want           childState
		wantDetail     string
	}{
		{"False", "InstallFailing", childFailed, "InstallFailing"},
		{"False", "Creating", childConverging, ""},
		{"False", "HelmOperationInProgress", childConverging, ""},
		{"True", "InstallFailing", childHealthy, ""},
	} {
		if ev := krateoEval(childComposition("c", tc.status, tc.reason)); ev.state != tc.want || ev.detail != tc.wantDetail {
			t.Errorf("Ready=%s/%s: state %d detail %q; want %d %q", tc.status, tc.reason, ev.state, ev.detail, tc.want, tc.wantDetail)
		}
	}
	ref := func(name string) map[string]any {
		return map[string]any{"apiVersion": "composition.krateo.io/v0-1-0", "resource": "portals", "name": name, "namespace": "ns"}
	}
	child := childComposition("failing", "False", "InstallFailing")
	conds, _, _ := unstructured.NestedSlice(child.Object, "status", "conditions")
	conds[0].(map[string]any)["message"] = "the first install has failed 7 times (9 automatic retries so far)"
	_ = unstructured.SetNestedSlice(child.Object, conds, "status", "conditions")
	mg := &unstructured.Unstructured{Object: map[string]any{}}
	setManaged(mg, ref("failing"), ref("ok"))
	h := &handler{}
	v := h.rollupManagedChildren(context.Background(), fakeDynWith(child, childComposition("ok", "True", "Available")), mg)
	if v.ready || v.reason != "Unavailable" || len(v.failing) != 1 || v.failing[0] != "portals/ns/failing (InstallFailing)" || strings.Contains(v.message, "automatic retries") {
		t.Errorf("ready=%v reason=%s failing=%v message=%q", v.ready, v.reason, v.failing, v.message)
	}
	if v = h.rollupManagedChildren(context.Background(), fakeDynWith(childComposition("failing", "True", "Available"), childComposition("ok", "True", "Available")), mg); !v.ready {
		t.Errorf("after the child recovers: ready=%v reason=%s", v.ready, v.reason)
	}
}

// The reason string is a cross-repo contract, as the others in reasoncontract_test.go (installer's and
// frontend's gates match it).
func TestInstallFailingReasonStringIsStable(t *testing.T) {
	if conditionreason.ReasonInstallFailing != "InstallFailing" || conditionreason.InstallFailing.Condition().Status != metav1.ConditionFalse {
		t.Errorf("%q %s", conditionreason.ReasonInstallFailing, conditionreason.InstallFailing.Condition().Status)
	}
}

// As TestInstallRecoveryKeysAreDeclaredInTheGeneratedStatusSchema.
func TestInstallFailureKeysAreDeclaredInTheGeneratedStatusSchema(t *testing.T) {
	mg, _ := recordInstallFailure(context.Background(), ndPortal(""), errors.New("x"), time.Now(), srOpts(frFakeDyn(ndPortal(""))), logging.NewNopLogger())
	rec, _, _ := unstructured.NestedMap(mg.Object, "status", "installFailures")
	var written []string
	for k := range rec {
		written = append(written, k)
	}
	sort.Strings(written)
	want := append([]string(nil), installFailureKeys...)
	sort.Strings(want)
	if strings.Join(written, ",") != strings.Join(want, ",") {
		t.Fatalf("the writer produces %v but installFailureKeys says %v", written, want)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "core-provider", "internal", "tools", "crd", "generation", "statics", "status.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	var sch struct {
		Properties map[string]struct {
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(raw, &sch); err != nil {
		t.Fatal(err)
	}
	props := sch.Properties["installFailures"].Properties
	for _, k := range written {
		if _, ok := props[k]; !ok {
			t.Errorf("status.installFailures.%s is written but not declared: the apiserver prunes it, and the count never reaches the threshold", k)
		}
	}
}
