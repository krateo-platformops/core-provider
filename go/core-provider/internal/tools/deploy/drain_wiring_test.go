package deploy

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/krateo-platformops/core-provider/internal/tools/objects"
)

// THE WIRING TEST, and it exists because the mutant proved it had to.
//
// drain_test.go covers waitForControllerDrained thoroughly — six arms including a failed LIST and
// an empty selector. A compiling mutant that deletes the CALL from Undeploy passes every one of
// them, because they exercise the helper and say nothing about whether it is reached.
//
// That is the third time this shape has come up: #258's paused condition was covered at the
// mechanism and uncovered at the call site, and plumbing's stale-mapping retry existed only in
// Install while Upgrade/Uninstall/Rollback went without. A helper nobody calls is not a fix.
//
// So this drives the real Undeploy, with a pod that never terminates, and asserts it REFUSES to
// reach the identity deletions.
func TestUndeploy_DoesNotRevokeTheIdentityWhileAPodSurvives(t *testing.T) {
	const ns = keepSharedOldNS

	// Render the Deployment exactly as Undeploy does, so the pod we seed carries the labels its
	// selector will actually look for. Hardcoding a label here would make the test pass or fail on
	// the fixture rather than on the behaviour, and would rot silently if the template changed.
	nn := getCDCDeploymentNN(types.NamespacedName{
		Namespace: ns,
		Name:      resourceNamer(guardGVR.Resource, guardGVR.Version),
	})
	var rendered appsv1.Deployment
	if err := objects.CreateK8sObject(&rendered, guardGVR, nn, "testdata/deploy.yaml",
		"serviceAccountName", "irrelevant-for-the-selector",
		"composition_controller_sa_name", "irrelevant-for-the-selector",
		"api_ref_name", ""); err != nil {
		t.Fatalf("rendering the controller Deployment: %v", err)
	}
	if rendered.Spec.Selector == nil || len(rendered.Spec.Selector.MatchLabels) == 0 {
		t.Fatalf("PREMISE: the rendered Deployment has no selector, so this test cannot seed a " +
			"matching pod and would assert nothing")
	}

	surviving := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "retiring-controller-pod",
		Namespace: ns,
		Labels:    rendered.Spec.Selector.MatchLabels,
	}}

	dyn := keepSharedCluster()
	err := Undeploy(context.Background(), nil, UndeployOptions{
		KubeClient: clientfake.NewClientBuilder().WithObjects(surviving).Build(),
		// A short bound: this test proves the CALL is wired into Undeploy, not what the bound is.
		// drain_test.go's BoundFollowsThePodsGracePeriod covers the derived value.
		DrainTimeout:           2 * time.Second,
		DynamicClient:          dyn,
		GVR:                    guardGVR,
		Namespace:              ns,
		SkipCRD:                true,
		RBACFolderPath:         "testdata",
		DeploymentTemplatePath: "testdata/deploy.yaml",
		ConfigmapTemplatePath:  "testdata/cm.yaml",
		JsonSchemaTemplatePath: "testdata/configmap_jsonschema.yaml",
		ServiceTemplatePath:    "testdata/service.yaml",
	})

	if err == nil {
		t.Fatal("Undeploy completed while a controller pod was still running: it has therefore " +
			"deleted that pod's ServiceAccount and RBAC out from under a live reconcile, which is " +
			"#263 and the origin of #261")
	}
	if !strings.Contains(err.Error(), "drain") {
		t.Errorf("Undeploy failed with %q; expected the drain refusal, so this test may be passing "+
			"for an unrelated reason and asserting nothing about the wait", err)
	}
}

// CONTROL. The same Undeploy with NO surviving pod must complete — otherwise the test above would
// pass against a change that simply broke Undeploy, and every real retirement would now fail.
func TestUndeploy_CompletesWhenNoPodSurvives(t *testing.T) {
	dyn := keepSharedCluster()
	err := Undeploy(context.Background(), nil, UndeployOptions{
		KubeClient:             clientfake.NewClientBuilder().Build(),
		DynamicClient:          dyn,
		GVR:                    guardGVR,
		Namespace:              keepSharedOldNS,
		SkipCRD:                true,
		RBACFolderPath:         "testdata",
		DeploymentTemplatePath: "testdata/deploy.yaml",
		ConfigmapTemplatePath:  "testdata/cm.yaml",
		JsonSchemaTemplatePath: "testdata/configmap_jsonschema.yaml",
		ServiceTemplatePath:    "testdata/service.yaml",
	})
	if err != nil {
		t.Fatalf("control: a quiescent retirement must still complete, or #263's fix has broken "+
			"every version migration: %v", err)
	}
}
