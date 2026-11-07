//go:build integration
// +build integration

package compositiondefinitions

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/e2e-framework/klient/k8s/resources"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
)

// #327, against a real apiserver with the shipped templates: a per-version controller left behind
// for a version the CRD does not carry is retired by the next Update of its definition, the way
// 2.13.45's aborted retirements left installers-v0-3-480-controller on krateo-057.
//
// The plant uses the names the templates render for a version (resource-version for the
// ServiceAccount, plus -controller for the Deployment, plus -configmap for the ConfigMap), so what is
// asserted gone is what Undeploy targets. Replicas 0: a leaked controller's presence is the defect,
// and no pod means no image pull and nothing for the drain wait to wait on.
const (
	reclaimLeakVersion = "v1-9-9" // never a fireworksapps version
	reclaimLeakSA      = "fireworksapps-" + reclaimLeakVersion
	reclaimLeakDeploy  = reclaimLeakSA + "-controller"
	reclaimLeakCM      = reclaimLeakSA + "-configmap"
)

func assessReclaimLeaked(ctx context.Context, t *testing.T, _ *envconf.Config, r *resources.Resources) context.Context {
	// PRECONDITION, checked rather than assumed: the plant must be for a version the CRD does not
	// carry, or the reclaim correctly keeps it and this step would test the refusal instead.
	var crd apiextensionsv1.CustomResourceDefinition
	if err := r.Get(ctx, rehomeCRD, "", &crd); err != nil {
		t.Fatalf("reading %s: %v", rehomeCRD, err)
	}
	for _, v := range crd.Spec.Versions {
		if v.Name == reclaimLeakVersion {
			t.Fatalf("setup: %s carries %s, so a controller for it is not a leak", rehomeCRD, reclaimLeakVersion)
		}
	}

	labels := map[string]string{"app.kubernetes.io/name": reclaimLeakDeploy}
	zero := int32(0)
	plant := []client.Object{
		&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: reclaimLeakSA, Namespace: namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: reclaimLeakCM, Namespace: namespace}},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: reclaimLeakDeploy, Namespace: namespace, Labels: labels},
			Spec: appsv1.DeploymentSpec{
				Replicas: &zero,
				Selector: &metav1.LabelSelector{MatchLabels: labels},
				Template: corev1.PodTemplateSpec{
					ObjectMeta: metav1.ObjectMeta{Labels: labels},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "registry.k8s.io/pause:3.10"}}},
				},
			},
		},
	}
	for _, o := range plant {
		if err := r.Create(ctx, o); err != nil {
			t.Fatalf("planting %T %s: %v", o, o.GetName(), err)
		}
	}

	// Update runs only when something changed, so change something real: one more worker re-renders
	// the current controller's Deployment, which is what a roll does to every definition.
	var cd v1alpha1.CompositionDefinition
	if err := r.Get(ctx, rehomeCDName, namespace, &cd); err != nil {
		t.Fatal(err)
	}
	before := cd.Spec.Controller.DeepCopy()
	workers := int32(2)
	if cd.Spec.Controller == nil {
		cd.Spec.Controller = &v1alpha1.ControllerConfig{}
	}
	if w := cd.Spec.Controller.Workers; w != nil && *w < 512 {
		workers = *w + 1
	}
	cd.Spec.Controller.Workers = &workers
	if err := r.Update(ctx, &cd); err != nil {
		t.Fatalf("changing workers to trigger an Update: %v", err)
	}

	gone := func(o client.Object) bool {
		err := r.Get(ctx, o.GetName(), o.GetNamespace(), o)
		return apierrors.IsNotFound(err)
	}
	rehomeEventually(t, 3*time.Minute, "the leaked v1-9-9 controller, its ServiceAccount and ConfigMap to be retired", func() bool {
		return gone(&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: reclaimLeakDeploy, Namespace: namespace}}) &&
			gone(&corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: reclaimLeakSA, Namespace: namespace}}) &&
			gone(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: reclaimLeakCM, Namespace: namespace}})
	})

	// The current controller is what a wrong predicate would destroy: it must still be there, and
	// so must its identity.
	if !rehomeControllerIn(ctx, t, r, namespace) {
		t.Fatalf("the CURRENT %s controller in %s was removed by the reclaim", rehomeVersion, namespace)
	}
	var sa corev1.ServiceAccount
	if err := r.Get(ctx, rehomeShared, namespace, &sa); err != nil {
		t.Fatalf("the current controller's ServiceAccount %s: %v", rehomeShared, err)
	}
	if vs := servedVersionNames(&crd); !slices.Contains(vs, rehomeVersion) {
		t.Fatalf("setup: the CRD no longer serves %s (%v)", rehomeVersion, vs)
	}

	// Leave the definition as later steps expect it.
	if err := r.Get(ctx, rehomeCDName, namespace, &cd); err != nil {
		t.Fatal(err)
	}
	cd.Spec.Controller = before
	if err := r.Update(ctx, &cd); err != nil {
		t.Fatalf("restoring spec.controller: %v", err)
	}
	return ctx
}

func servedVersionNames(crd *apiextensionsv1.CustomResourceDefinition) []string {
	var out []string
	for _, v := range crd.Spec.Versions {
		if v.Served {
			out = append(out, v.Name)
		}
	}
	return out
}
