package compositiondefinitions

import (
	"context"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func withManagementNamespace(t *testing.T) string {
	t.Helper()
	if ManagementNamespace == "" {
		ManagementNamespace = "krateo-system"
		t.Cleanup(func() { ManagementNamespace = "" })
	}
	return ManagementNamespace
}

var pagesGVR = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "pages"}

// #580: which Deployment runs a definition's controller, by input state.
func TestControllerPlacement(t *testing.T) {
	ns := withManagementNamespace(t)
	dedicated := deploy.ControllerDeploymentKey(ns, pagesGVR).Name

	member := poolCD("team-a", "page", 1, "v1-0-0", "pages")
	plain := poolCD("team-a", "page", 1, "v1-0-0", "")
	remote := poolCD("team-a", "page", 1, "v1-0-0", "pages")
	remote.Spec.Deploy = &compositiondefinitionsv1alpha1.DeploymentTarget{TargetRef: &compositiondefinitionsv1alpha1.TargetReference{Name: "spoke"}}

	for _, tc := range []struct {
		name   string
		runner *compositiondefinitionsv1alpha1.CompositionDefinition
		want   compositiondefinitionsv1alpha1.ControllerStatus
	}{
		{"a dedicated controller: its own <resource>-<version>-controller", &plain,
			compositiondefinitionsv1alpha1.ControllerStatus{Mode: compositiondefinitionsv1alpha1.ControllerModeDedicated, Namespace: ns, Deployment: dedicated}},
		{"a pool member: the pool's Deployment", &member,
			compositiondefinitionsv1alpha1.ControllerStatus{Mode: compositiondefinitionsv1alpha1.ControllerModePool, Namespace: ns, Deployment: deploy.PoolNames("pages").Workload, Pool: "pages"}},
		{"remote (a remote pool is refused at admission): dedicated, in the definition's namespace on the target", &remote,
			compositiondefinitionsv1alpha1.ControllerStatus{Mode: compositiondefinitionsv1alpha1.ControllerModeDedicated, Namespace: "team-a", Deployment: deploy.ControllerDeploymentKey("team-a", pagesGVR).Name}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := controllerPlacement(tc.runner, pagesGVR); got != tc.want {
				t.Errorf("placement = %+v\nwant        %+v", got, tc.want)
			}
		})
	}
}

// #580's defect, found before pool step 6: Ready was read from <resource>-<version>-controller for
// EVERY definition, so a pool member, whose dedicated Deployment cutover retires, read
// Ready=False/Creating "the controller Deployment was not found" for as long as it stayed pooled.
// The placement decides which Deployment is read; the control is the old read, against the same
// cluster.
func TestControllerConditions_ReadTheDeploymentThatRunsTheController(t *testing.T) {
	ns := withManagementNamespace(t)
	poolDep := rolledOut(1)
	poolDep.Name, poolDep.Namespace = deploy.PoolNames("pages").Workload, ns
	// The cluster after cutover: the pool's Deployment runs; the member's dedicated one is gone.
	e := &external{kube: clientfake.NewClientBuilder().WithObjects(poolDep).Build()}
	ctx := context.Background()
	member := poolCD("team-a", "page", 1, "v1-0-0", "pages")

	ready, _, ok, err := e.controllerConditions(ctx, &member, controllerPlacement(&member, pagesGVR))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ready.Reason != cdreason.Available.Condition().Reason {
		t.Errorf("a pool member whose pool runs: Ready = %s/%s %q, want Available", ready.Status, ready.Reason, ready.Message)
	}

	// CONTROL: the read before #580 (the dedicated name), against the same cluster.
	old := compositiondefinitionsv1alpha1.ControllerStatus{Namespace: ns, Deployment: deploy.ControllerDeploymentKey(ns, pagesGVR).Name}
	ready, _, _, _ = e.controllerConditions(ctx, &member, old)
	if ready.Reason != cdreason.Creating.Condition().Reason || !strings.Contains(ready.Message, "not found") {
		t.Errorf("control: the dedicated read should reproduce the defect (Creating, not found), got %s/%s %q", ready.Status, ready.Reason, ready.Message)
	}

	t.Run("a follower whose OWNER is pooled reads the pool too", func(t *testing.T) {
		owner := poolCD("team-a", "page", 1, "v1-0-0", "pages")
		follower := poolCD("team-b", "page-b", 2, "v1-0-0", "")
		p := controllerPlacement(&owner, pagesGVR)
		if p.Mode != compositiondefinitionsv1alpha1.ControllerModePool {
			t.Fatalf("owner placement %+v", p)
		}
		ready, _, _, err := e.controllerConditions(ctx, &follower, p)
		if err != nil || ready.Reason != cdreason.Available.Condition().Reason {
			t.Errorf("follower: Ready = %s/%s %q err=%v, want Available", ready.Status, ready.Reason, ready.Message, err)
		}
	})
}
