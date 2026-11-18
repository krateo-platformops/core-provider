package deploy

import (
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
)

// gvrWithDeploymentName returns a GVR whose dedicated controller Deployment name is exactly n
// characters: "<plural>-v0-1-0-controller", padding the plural.
func gvrWithDeploymentName(n int) schema.GroupVersionResource {
	const fixed = len("-v0-1-0-controller")
	return schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-1-0", Resource: strings.Repeat("a", n-fixed)}
}

// #588, as a differential against the SHIPPED templates: the check's verdict must agree with what
// the two-stage render (helm, then core-provider) actually produces, at the 63-character boundary.
// At 63 the rendered selector label equals the Deployment's name; at 64 the template's `trunc 63`
// cuts it, and that silent cut is what the check refuses. The Service, rendered with metrics on,
// is 8 characters longer, so it crosses at a 55-character Deployment name.
func TestDerivedNameViolations_AgreeWithTheShippedTemplates(t *testing.T) {
	manifests := renderShippedChart(t, map[string]any{"cdc": map[string]any{"metrics": map[string]any{"enabled": true}}})
	depTemplate := mountedTemplate(t, manifests, "cdc-deployment.yaml", "deployment.yaml")
	svcTemplate := mountedTemplate(t, manifests, "cdc-service.yaml", "service.yaml")

	for _, tc := range []struct {
		depLen                int
		wantDep, wantSvc      bool // a violation expected
		renderedLabelTruncate bool
	}{
		{depLen: 55, wantDep: false, wantSvc: false},
		{depLen: 56, wantDep: false, wantSvc: true},
		{depLen: 63, wantDep: false, wantSvc: true},
		{depLen: 64, wantDep: true, wantSvc: true, renderedLabelTruncate: true},
	} {
		gvr := gvrWithDeploymentName(tc.depLen)
		nn := getCDCDeploymentNN(types.NamespacedName{Namespace: "krateo-system", Name: resourceNamer(gvr.Resource, gvr.Version)})
		if len(nn.Name) != tc.depLen {
			t.Fatalf("fixture: Deployment name %q is %d characters, want %d", nn.Name, len(nn.Name), tc.depLen)
		}

		// What the write would produce.
		var dep appsv1.Deployment
		if err := objects.CreateK8sObject(&dep, gvr, nn, depTemplate, "serviceAccountName", resourceNamer(gvr.Resource, gvr.Version), "api_ref_name", ""); err != nil {
			t.Fatalf("depLen %d: stage 2, Deployment: %v", tc.depLen, err)
		}
		var svc corev1.Service
		if err := objects.CreateK8sObject(&svc, gvr, nn, svcTemplate); err != nil {
			t.Fatalf("depLen %d: stage 2, Service: %v", tc.depLen, err)
		}
		label := dep.Spec.Selector.MatchLabels["app.kubernetes.io/name"]
		if truncated := label != dep.Name; truncated != tc.renderedLabelTruncate {
			t.Errorf("depLen %d: rendered selector label %q vs name %q: truncated=%v, want %v (the fixture no longer crosses the template's cut)", tc.depLen, label, dep.Name, truncated, tc.renderedLabelTruncate)
		}
		svcInvalid := len(validation.IsDNS1035Label(svc.Name)) > 0
		if svcInvalid != tc.wantSvc {
			t.Errorf("depLen %d: rendered Service %q (%d) invalid=%v, want %v", tc.depLen, svc.Name, len(svc.Name), svcInvalid, tc.wantSvc)
		}

		// What the check says.
		got, err := DerivedNameViolations(gvr, true, svcTemplate)
		if err != nil {
			t.Fatalf("depLen %d: %v", tc.depLen, err)
		}
		var gotDep, gotSvc bool
		for _, v := range got {
			gotDep = gotDep || v.Name == dep.Name
			gotSvc = gotSvc || v.Name == svc.Name
		}
		if gotDep != tc.wantDep || gotSvc != tc.wantSvc {
			t.Errorf("depLen %d: violations %v; want Deployment=%v Service=%v", tc.depLen, got, tc.wantDep, tc.wantSvc)
		}
	}
}

func TestDerivedNameViolations(t *testing.T) {
	long := gvrWithDeploymentName(64)

	t.Run("metrics off: no Service template, so the Service is not checked", func(t *testing.T) {
		got, err := DerivedNameViolations(gvrWithDeploymentName(60), true, "testdata/no-such-service.yaml")
		if err != nil || len(got) != 0 {
			t.Fatalf("DerivedNameViolations = %v, %v; want none", got, err)
		}
	})

	t.Run("a pool member: the dedicated names do not apply", func(t *testing.T) {
		got, err := DerivedNameViolations(long, false, "testdata/service.yaml")
		if err != nil || len(got) != 0 {
			t.Fatalf("DerivedNameViolations = %v, %v; want none for a pool member", got, err)
		}
	})

	t.Run("the same GVR dedicated: refused (the control for the pool arm)", func(t *testing.T) {
		got, err := DerivedNameViolations(long, true, "testdata/service.yaml")
		if err != nil || len(got) == 0 {
			t.Fatalf("DerivedNameViolations = %v, %v; want violations", got, err)
		}
	})

	for _, version := range []string{"v1-0-0-RC1", "v1-0-0+build"} {
		// Not reachable through Observe (crdgen normalizes the version first); a guard for a caller
		// that passes an un-normalized one.
		t.Run("un-normalized CRD version "+version+": refused, pool member or not", func(t *testing.T) {
			gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: version, Resource: "widgets"}
			got, err := DerivedNameViolations(gvr, false, "")
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Name != version {
				t.Fatalf("DerivedNameViolations = %v; want exactly the CRD version", got)
			}
			if !strings.Contains(got[0].String(), "CRD version") {
				t.Errorf("message %q does not say which name", got[0])
			}
		})
	}

	t.Run("aruba's content-addressed version fits with a 15-character plural", func(t *testing.T) {
		gvr := schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v0-0-0-probe-35fc2441c975", Resource: "nocreaterollups"}
		got, err := DerivedNameViolations(gvr, true, "testdata/service.yaml")
		if err != nil || len(got) != 0 {
			t.Fatalf("DerivedNameViolations = %v, %v; want none (the 20-byte cap refused this)", got, err)
		}
	})
}
