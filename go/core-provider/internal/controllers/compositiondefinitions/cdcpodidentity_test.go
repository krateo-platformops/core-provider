package compositiondefinitions

import (
	"path/filepath"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// core-provider#461 (c): every cdc pod knows its own name and namespace (POD_NAME / POD_NAMESPACE,
// downward API), which it records as the starter of each helm operation so the next controller can
// clear a dead pod's lock at once. The env block used to be rendered only when goMemLimit or
// node-local OTLP was on; the identity must be there in every configuration, including neither.
func TestCdcPodKnowsItsIdentity(t *testing.T) {
	for name, extra := range map[string]map[string]any{
		"defaults": nil,
		// renderChartAssetsWith replaces a whole top-level key, so the cdc block repeats the image the
		// helper sets (registry "", repository, tag) and only adds goMemLimit "".
		"otel off and no GOMEMLIMIT (no env block before)": {"otel": map[string]any{"enabled": false}, "cdc": map[string]any{
			"image":      map[string]any{"registry": "", "repository": "composition-dynamic-controller", "tag": "unit", "pullPolicy": "IfNotPresent"},
			"metrics":    map[string]any{"enabled": true},
			"goMemLimit": "",
		}},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if _, err := renderChartAssetsWith(root, "krateo-system", "composition-dynamic-controller:unit", extra); err != nil {
				t.Fatal(err)
			}
			var dep appsv1.Deployment
			if err := objects.CreateK8sObject(&dep, schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-0-0", Resource: "fireworksapps"},
				types.NamespacedName{Namespace: "krateo-system", Name: "fireworksapps-v1-0-0-controller"},
				filepath.Join(root, "cdc-deployment", filepath.Base(CDCtemplateDeploymentPath)),
				"serviceAccountName", "fireworksapps-v1-0-0"); err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"POD_NAME": "metadata.name", "POD_NAMESPACE": "metadata.namespace"}
			for _, e := range dep.Spec.Template.Spec.Containers[0].Env {
				if path, ok := want[e.Name]; ok && e.ValueFrom != nil && e.ValueFrom.FieldRef != nil && e.ValueFrom.FieldRef.FieldPath == path {
					delete(want, e.Name)
				}
			}
			if len(want) != 0 {
				t.Errorf("cdc env lacks %v from the downward API: %+v", want, dep.Spec.Template.Spec.Containers[0].Env)
			}
		})
	}
}
