package deploy

import (
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// renderCDCDeployment renders the CDC Deployment fixture the way deploy.Deploy does, with the
// given per-Kind controller values appended as template pairs. No cluster required.
func renderCDCDeployment(t *testing.T, extra ...any) appsv1.Deployment {
	t.Helper()
	dep := appsv1.Deployment{}
	gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-1-0", Resource: "benchapps"}
	nn := types.NamespacedName{Namespace: "bench-ns-01", Name: "benchapps-v0-1-0-controller"}
	values := append(deploymentIdentityValues("sa", ""), extra...) // a later pair wins
	if err := objects.CreateK8sObject(&dep, gvr, nn, "testdata/deploy.yaml", values...); err != nil {
		t.Fatalf("render CDC deployment: %v", err)
	}
	return dep
}

func cdcArgs(t *testing.T, dep appsv1.Deployment) []string {
	t.Helper()
	if len(dep.Spec.Template.Spec.Containers) == 0 {
		t.Fatal("rendered deployment has no containers")
	}
	return dep.Spec.Template.Spec.Containers[0].Args
}

func hasArgPrefix(args []string, prefix string) bool {
	for _, a := range args {
		if strings.HasPrefix(a, prefix) {
			return true
		}
	}
	return false
}

// A CompositionDefinition with no spec.controller must render NO -workers / -resync-interval args
// at all, so the CDC applies its own defaults and existing definitions stay byte-identical (and do
// not restart on upgrade).
func TestControllerConfig_OmittedWhenUnset(t *testing.T) {
	args := cdcArgs(t, renderCDCDeployment(t))
	assert.False(t, hasArgPrefix(args, "-workers="), "unset workers must emit no -workers arg: %v", args)
	assert.False(t, hasArgPrefix(args, "-resync-interval="), "unset resync must emit no -resync-interval arg: %v", args)
}

// spec.controller values (as deploy.Deploy threads them: Workers as int32, ResyncInterval as a Go
// duration string) must appear as args on the one Kind, leaving every other CDC untouched.
func TestControllerConfig_OverridesPerKind(t *testing.T) {
	args := cdcArgs(t, renderCDCDeployment(t, "workers", int32(64), "resyncInterval", "1h0m0s"))
	assert.Contains(t, args, "-workers=64")
	assert.Contains(t, args, "-resync-interval=1h0m0s")
}

// Only the field that is set is emitted (workers without resync, and vice versa).
func TestControllerConfig_PartialOnlyEmitsSet(t *testing.T) {
	args := cdcArgs(t, renderCDCDeployment(t, "workers", int32(8)))
	assert.Contains(t, args, "-workers=8")
	assert.False(t, hasArgPrefix(args, "-resync-interval="), "resync unset must emit no arg: %v", args)
}

// spec.controller.resources (threaded by deploy.Deploy as a JSON string) must land as the CDC
// container's resources; unset must render empty resources (byte-identical to the prior template).
func TestControllerConfig_Resources(t *testing.T) {
	rj := `{"requests":{"memory":"512Mi","cpu":"250m"},"limits":{"memory":"1Gi"}}`
	res := renderCDCDeployment(t, "resources", rj).Spec.Template.Spec.Containers[0].Resources
	assert.Equal(t, "512Mi", res.Requests.Memory().String())
	assert.Equal(t, "250m", res.Requests.Cpu().String())
	assert.Equal(t, "1Gi", res.Limits.Memory().String())

	empty := renderCDCDeployment(t).Spec.Template.Spec.Containers[0].Resources
	assert.Empty(t, empty.Requests, "unset resources must render empty")
	assert.Empty(t, empty.Limits, "unset resources must render empty")
}
