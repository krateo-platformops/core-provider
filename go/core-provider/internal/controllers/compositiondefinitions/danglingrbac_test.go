package compositiondefinitions

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	rbGVR  = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}
	crbGVR = schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterrolebindings"}
)

// sweepBinding is a RoleBinding in app-ns whose only subject is leakNS/<sa>.
func sweepBinding(name, sa string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "rbac.authorization.k8s.io/v1", "kind": "RoleBinding",
		"metadata": map[string]any{"name": name, "namespace": "app-ns"},
		"roleRef":  map[string]any{"apiGroup": "rbac.authorization.k8s.io", "kind": "Role", "name": name},
		"subjects": []any{map[string]any{"kind": "ServiceAccount", "name": sa, "namespace": leakNS}},
	}}
	return u
}

func sweepExternal(t *testing.T, crd *apiextensionsv1.CustomResourceDefinition, remove bool, objs ...runtime.Object) (*external, *dynamicfake.FakeDynamicClient) {
	t.Helper()
	b := clientfake.NewClientBuilder().WithScheme(leakScheme(t))
	if crd != nil {
		b = b.WithObjects(crd)
	}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), map[schema.GroupVersionResource]string{
		rbGVR: "RoleBindingList", crbGVR: "ClusterRoleBindingList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "roles"}:        "RoleList",
		{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"}: "ClusterRoleList",
		{Version: "v1", Resource: "serviceaccounts"}:                                  "ServiceAccountList",
	}, objs...)
	kc := b.Build()
	return &external{kube: kc, mgmt: kc, dynamic: dyn, removeDanglingRBAC: remove}, dyn
}

func bindingExists(t *testing.T, dyn *dynamicfake.FakeDynamicClient, name string) bool {
	t.Helper()
	_, err := dyn.Resource(rbGVR).Namespace("app-ns").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
	return err == nil
}

// #414. The versions that are NOT retired come from the live CRD: v0-3-484 is on it, so its
// binding is never a candidate even though its ServiceAccount is absent. v0-3-480 is off it.
func TestSweepDanglingControllerRBAC(t *testing.T) {
	ctx := context.Background()
	objs := func() []runtime.Object {
		return []runtime.Object{
			sweepBinding("retired", leakResource+"-v0-3-480"),
			sweepBinding("on-crd", leakResource+"-v0-3-484"),
		}
	}

	t.Run("dry run reports and deletes nothing", func(t *testing.T) {
		e, dyn := sweepExternal(t, leakCRD("v0-3-484", "vacuum"), false, objs()...)
		got, err := e.sweepDanglingControllerRBAC(ctx, leakGVR(), []string{leakNS}, false)
		if err != nil {
			t.Fatal(err)
		}
		if !got.DryRun || len(got.Removed) != 0 {
			t.Errorf("dry run: DryRun=%v Removed=%v, want true and none", got.DryRun, got.Removed)
		}
		if names := strings.Join(danglingNames(got.Found), ","); names != "RoleBinding app-ns/retired -> krateo-system/installers-v0-3-480" {
			t.Errorf("found %q, want only the retired version's binding", names)
		}
		if !bindingExists(t, dyn, "retired") {
			t.Error("a dry run deleted the binding")
		}
	})

	t.Run("removal enabled deletes only the retired version's binding", func(t *testing.T) {
		e, dyn := sweepExternal(t, leakCRD("v0-3-484", "vacuum"), true, objs()...)
		got, err := e.sweepDanglingControllerRBAC(ctx, leakGVR(), []string{leakNS}, false)
		if err != nil {
			t.Fatal(err)
		}
		if got.DryRun || len(got.Removed) == 0 {
			t.Errorf("DryRun=%v Removed=%v, want false and the binding", got.DryRun, got.Removed)
		}
		if bindingExists(t, dyn, "retired") {
			t.Error("the retired version's binding is still there")
		}
		if !bindingExists(t, dyn, "on-crd") {
			t.Error("a binding for a version on the CRD was deleted")
		}
	})

	t.Run("no CRD is an error, not every version retired", func(t *testing.T) {
		e, dyn := sweepExternal(t, nil, true, objs()...)
		got, err := e.sweepDanglingControllerRBAC(ctx, leakGVR(), []string{leakNS}, false)
		if err == nil {
			t.Errorf("no CRD returned no error (found %v)", danglingNames(got.Found))
		}
		if !bindingExists(t, dyn, "retired") || !bindingExists(t, dyn, "on-crd") {
			t.Error("a sweep without a CRD deleted something")
		}
	})
}

// #414 follow-up. Every sweep logs exactly one line naming its outcome and how many it found, so a
// census diff can tell "evaluated, nothing found" from "not evaluated". The first version was silent
// on a dry run that found nothing, which is the most common state on a cluster.
func TestDanglingSweepLogLineCoversEveryOutcome(t *testing.T) {
	one := []deploy.DanglingBinding{{Kind: "RoleBinding", Namespace: "app-ns", Name: "b", ServiceAccount: "krateo-system/installers-v1"}}
	boom := errors.New("boom")
	cases := []struct {
		name        string
		sweep       danglingSweep
		err         error
		wantOutcome string
		wantFound   int
		wantErr     bool
	}{
		{"dry run, nothing found", danglingSweep{DryRun: true}, nil, danglingOutcomeDryRun, 0, false},
		{"dry run, found", danglingSweep{DryRun: true, Found: one}, nil, danglingOutcomeDryRun, 1, false},
		{"search failed", danglingSweep{}, boom, danglingOutcomeError, 0, true},
		{"removal on, nothing found", danglingSweep{}, nil, danglingOutcomeNone, 0, false},
		{"removal on, removed", danglingSweep{Found: one, Removed: []string{"x"}}, nil, danglingOutcomeRemoved, 1, false},
		{"removal on, partly failed", danglingSweep{Found: one}, boom, danglingOutcomeRemoved, 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			msg, kv := danglingSweepLogLine(danglingPhaseUpdate, "installers", c.sweep, c.err)
			if !strings.Contains(msg, "#414") {
				t.Errorf("msg %q does not carry #414", msg)
			}
			fields := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				fields[kv[i].(string)] = kv[i+1]
			}
			if fields["outcome"] != c.wantOutcome || fields["found"] != c.wantFound || fields["resource"] != "installers" {
				t.Errorf("outcome=%v found=%v resource=%v, want %s %d installers", fields["outcome"], fields["found"], fields["resource"], c.wantOutcome, c.wantFound)
			}
			if _, has := fields["error"]; has != c.wantErr {
				t.Errorf("error field present=%v, want %v", has, c.wantErr)
			}
			if fields["phase"] != danglingPhaseUpdate {
				t.Errorf("phase=%v, want %s", fields["phase"], danglingPhaseUpdate)
			}
			// One shape for readers: kept, removed and bindings are on EVERY line, and never null.
			for _, k := range []string{"kept", "removed", "bindings"} {
				v, has := fields[k]
				if !has {
					t.Errorf("%s is missing from the %s line; every line must carry it", k, c.wantOutcome)
					continue
				}
				if v.([]string) == nil {
					t.Errorf("%s is a nil slice; it must log as [], not null", k)
				}
			}
		})
	}
}

// #414, Delete path. When the definition's Delete removed the Kind's CRD, every version is retired
// by that act, so the sweep runs WITHOUT a CRD and finds the bindings of every version, the one that
// was current included. Without kindRemoved, a missing CRD is still an error (the arm above).
func TestSweepDanglingControllerRBACOnKindRemoval(t *testing.T) {
	ctx := context.Background()
	e, dyn := sweepExternal(t, nil, true,
		sweepBinding("retired", leakResource+"-v0-3-480"),
		sweepBinding("was-current", leakResource+"-v0-3-484"),
	)
	got, err := e.sweepDanglingControllerRBAC(ctx, leakGVR(), []string{leakNS}, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Found) != 2 || len(got.Removed) == 0 {
		t.Errorf("found %v removed %v, want both bindings found and removed", danglingNames(got.Found), got.Removed)
	}
	for _, name := range []string{"retired", "was-current"} {
		if bindingExists(t, dyn, name) {
			t.Errorf("binding %s survived the Kind's removal", name)
		}
	}
}

// Diego, 2026-11-10: with removal on, a failed sweep holds the Delete (it is the last chance); a dry
// run never does.
func TestDeleteSweepError(t *testing.T) {
	boom := errors.New("boom")
	if err := deleteSweepError(danglingSweep{}, nil); err != nil {
		t.Errorf("no failure held the delete: %v", err)
	}
	if err := deleteSweepError(danglingSweep{DryRun: true}, boom); err != nil {
		t.Errorf("a dry run held the delete: %v", err)
	}
	if err := deleteSweepError(danglingSweep{}, boom); !errors.Is(err, boom) {
		t.Errorf("with removal on, a failed sweep must hold the delete; got %v", err)
	}
}

// sweepOnDelete wires Delete's skipCRD into the sweep. CRD removed by this Delete: no CRD is needed
// and every version's binding goes. CRD kept (another definition needs the Kind): the live CRD
// decides, so the version still on it keeps its binding.
func TestSweepOnDelete(t *testing.T) {
	ctx := context.Background()
	objs := func() []runtime.Object {
		return []runtime.Object{
			sweepBinding("retired", leakResource+"-v0-3-480"),
			sweepBinding("on-crd", leakResource+"-v0-3-484"),
		}
	}
	t.Run("this Delete removed the CRD", func(t *testing.T) {
		e, dyn := sweepExternal(t, nil, true, objs()...)
		if err := e.sweepOnDelete(ctx, leakGVR(), []string{leakNS}, false, logging.NewNopLogger()); err != nil {
			t.Fatal(err)
		}
		if bindingExists(t, dyn, "retired") || bindingExists(t, dyn, "on-crd") {
			t.Error("with the Kind removed, every version's binding must go")
		}
	})
	t.Run("another definition keeps the CRD", func(t *testing.T) {
		e, dyn := sweepExternal(t, leakCRD("v0-3-484", "vacuum"), true, objs()...)
		if err := e.sweepOnDelete(ctx, leakGVR(), []string{leakNS}, true, logging.NewNopLogger()); err != nil {
			t.Fatal(err)
		}
		if bindingExists(t, dyn, "retired") || !bindingExists(t, dyn, "on-crd") {
			t.Error("with the CRD kept, only the version off it may go")
		}
	})
	t.Run("CRD kept but unreadable holds the delete when removal is on", func(t *testing.T) {
		e, _ := sweepExternal(t, nil, true, objs()...)
		if err := e.sweepOnDelete(ctx, leakGVR(), []string{leakNS}, true, logging.NewNopLogger()); err == nil {
			t.Error("a sweep that could not run must hold the deletion")
		}
	})
}
