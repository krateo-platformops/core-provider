package composition

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

// #79. status.managed[].path is what a consumer GETs. A namespaced child whose rendered manifest
// omitted metadata.namespace used to get a CLUSTER-scoped path written, and anything following it
// got Go's router 404 — proved end to end on a customer cluster by the marketing session, one bad
// entry out of 93, with the namespaced form of the same path returning the object.
//
// The fix for that was upstream NORMALISATION; this function was left branching on whether the
// namespace string happened to be non-empty. Keying it on scope instead is what makes the path
// correct by construction rather than correct by an invariant twenty lines away.
//
// THE DISCRIMINATING ARM IS THE FOURTH ONE and it is why this was extracted from a closure. Inside
// populateManagedResources no input could distinguish the old branch from the new: the
// normalisation strips a cluster-scoped kind's namespace before the path is built, so both versions
// agreed on every reachable input. As a function, isNamespaced=false WITH a non-empty namespace is
// reachable — and that is exactly the state the old code got wrong.
func TestManagedPath(t *testing.T) {
	for _, tc := range []struct {
		name         string
		gvr          schema.GroupVersionResource
		isNamespaced bool
		namespace    string
		objName      string
		want         string
		why          string
	}{
		{
			name:         "namespaced, grouped",
			gvr:          schema.GroupVersionResource{Group: "ecr.services.k8s.aws", Version: "v1alpha1", Resource: "repositories"},
			isNamespaced: true, namespace: "krateo-system", objName: "repo",
			want: "/apis/ecr.services.k8s.aws/v1alpha1/namespaces/krateo-system/repositories/repo",
			why:  "the real kind from the customer's 93-entry inventory",
		},
		{
			name:         "namespaced, CORE group goes under /api not /apis",
			gvr:          schema.GroupVersionResource{Version: "v1", Resource: "secrets"},
			isNamespaced: true, namespace: "krateo-system", objName: "pki",
			want: "/api/v1/namespaces/krateo-system/secrets/pki",
			why:  "core-group resources are served from /api; /apis/v1/... does not exist",
		},
		{
			name:         "cluster-scoped, grouped",
			gvr:          schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"},
			isNamespaced: false, namespace: "", objName: "things.example.io",
			want: "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/things.example.io",
			why:  "no /namespaces/ segment for a cluster-scoped kind",
		},
		{
			// THE ARM THE EXTRACTION EXISTS FOR. Under the old code this returned the NAMESPACED
			// path, because it keyed on the namespace string being non-empty rather than on scope.
			// Unreachable while the logic lived in a closure behind the normalisation.
			name:         "cluster-scoped WITH a stray namespace must still be cluster-scoped",
			gvr:          schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "clusterroles"},
			isNamespaced: false, namespace: "krateo-system", objName: "reader",
			want: "/apis/rbac.authorization.k8s.io/v1/clusterroles/reader",
			why: "scope decides the path, not the presence of a namespace string. The OLD code " +
				"returned /namespaces/krateo-system/clusterroles/reader here, which 404s",
		},
		{
			// The inverse, for completeness: a namespaced kind must never lose its /namespaces/
			// segment, which is the original #79 defect.
			name:         "namespaced with an EMPTY namespace still takes the namespaced form",
			gvr:          schema.GroupVersionResource{Version: "v1", Resource: "configmaps"},
			isNamespaced: true, namespace: "", objName: "cfg",
			want: "/api/v1/namespaces//configmaps/cfg",
			why: "the path is malformed, but it is malformed in a way that is OBVIOUSLY wrong " +
				"rather than silently resolving to a cluster-scoped endpoint — the normalisation " +
				"upstream is what prevents this input, and this arm pins what happens if it ever " +
				"stops: a visible empty segment, not a 404 against the wrong endpoint",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := managedPath(tc.gvr, tc.isNamespaced, tc.namespace, tc.objName)
			if got != tc.want {
				t.Errorf("managedPath = %q\n  want %q\n  %s", got, tc.want, tc.why)
			}
		})
	}
}
