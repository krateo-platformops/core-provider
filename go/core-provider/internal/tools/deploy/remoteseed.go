package deploy

import (
	"context"
	_ "embed"
	"fmt"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// compositiondefinitions CRD, shipped as an asset so it can be projected onto a remote target
// where core-provider does NOT run (and therefore never installs its own CRDs).
//
//go:embed assets/compositiondefinitions-crd.yaml
var compositionDefinitionCRDBytes []byte

var compositionDefinitionGVR = schema.GroupVersionResource{
	Group:    "core.krateo.io",
	Version:  "v1alpha1",
	Resource: "compositiondefinitions",
}

// RemoteSeedOptions is what a spoke needs so a projected cdc can resolve a package locally. The
// composition coordinates (APIVersion/Kind/Resource) are the GENERATED composition's (e.g.
// composition.krateo.io/v0-1-0, DemoApp, demoapps) — the same values RefreshCompositionDefinitionStatus
// records on the hub CR.
type RemoteSeedOptions struct {
	Namespace  string
	CDName     string
	Chart      *compositiondefinitionsv1alpha1.ChartInfo
	PackageURL string
	APIVersion string
	Kind       string
	Resource   string
}

// SeedRemoteTarget makes a spoke self-sufficient for a projected cdc (Path A, Inc 1). The projected
// cdc resolves a package by LISTING compositiondefinitions.core.krateo.io in its OWN cluster and
// reading status.packageUrl (cdc internal/tools/archive/getter.go). core-provider does not run on
// the spoke, so nothing puts that CRD or a CR there — this does:
//
//	(B) ensure the target namespace,
//	(C) install the compositiondefinitions CRD, then create a status-only "shadow"
//	    CompositionDefinition carrying exactly what the getter reads.
//
// Idempotent; applied through the spoke-swapped clients. The shadow is bait for the getter — no
// controller reconciles it on the spoke, so its status persists. Deliberately NOT part of the
// composition deploy digest (it is target infrastructure, not per-composition state).
func SeedRemoteTarget(ctx context.Context, kube client.Client, dyn dynamic.Interface, opts RemoteSeedOptions) error {
	// B — target namespace. TypeMeta is set explicitly: kubecli.Apply reads the GVK off the object,
	// and a struct-literal typed object carries none.
	ns := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: opts.Namespace},
	}
	if _, err := kubecli.Apply(ctx, dyn, namespaceGVR, ns, kubecli.ApplyOptions{}); err != nil {
		return fmt.Errorf("ensuring target namespace %q: %w", opts.Namespace, err)
	}

	// C.1 — the compositiondefinitions CRD, then wait until the API is served.
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(compositionDefinitionCRDBytes, crd); err != nil {
		return fmt.Errorf("decoding embedded compositiondefinitions CRD: %w", err)
	}
	if _, err := kubecli.Apply(ctx, dyn, crdGVR, crd, kubecli.ApplyOptions{}); err != nil {
		return fmt.Errorf("installing compositiondefinitions CRD on target: %w", err)
	}
	if err := waitCRDServed(ctx, dyn, compositionDefinitionGVR, 30*time.Second); err != nil {
		return fmt.Errorf("waiting for compositiondefinitions CRD to be served on target: %w", err)
	}

	// C.2 — the shadow CompositionDefinition: spec mirrors the chart, status carries what the
	// getter reads (packageUrl + the composition apiVersion/kind/resource).
	chartMap, err := runtime.DefaultUnstructuredConverter.ToUnstructured(opts.Chart)
	if err != nil {
		return fmt.Errorf("encoding chart spec for shadow CompositionDefinition: %w", err)
	}

	res := dyn.Resource(compositionDefinitionGVR).Namespace(opts.Namespace)
	live, err := res.Get(ctx, opts.CDName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		shadow := &unstructured.Unstructured{}
		shadow.SetAPIVersion("core.krateo.io/v1alpha1")
		shadow.SetKind("CompositionDefinition")
		shadow.SetNamespace(opts.Namespace)
		shadow.SetName(opts.CDName)
		if err := unstructured.SetNestedMap(shadow.Object, chartMap, "spec", "chart"); err != nil {
			return fmt.Errorf("building shadow CompositionDefinition spec: %w", err)
		}
		if live, err = res.Create(ctx, shadow, metav1.CreateOptions{}); err != nil {
			return fmt.Errorf("creating shadow CompositionDefinition: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("reading shadow CompositionDefinition: %w", err)
	} else {
		if err := unstructured.SetNestedMap(live.Object, chartMap, "spec", "chart"); err != nil {
			return fmt.Errorf("updating shadow CompositionDefinition spec: %w", err)
		}
		if live, err = res.Update(ctx, live, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("updating shadow CompositionDefinition: %w", err)
		}
	}

	// status subresource — the fields the cdc getter reads.
	_ = unstructured.SetNestedField(live.Object, opts.PackageURL, "status", "packageUrl")
	_ = unstructured.SetNestedField(live.Object, opts.APIVersion, "status", "apiVersion")
	_ = unstructured.SetNestedField(live.Object, opts.Kind, "status", "kind")
	_ = unstructured.SetNestedField(live.Object, opts.Resource, "status", "resource")
	if _, err := res.UpdateStatus(ctx, live, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("setting shadow CompositionDefinition status: %w", err)
	}
	return nil
}

// RemoteSeedTeardownOptions is what a spoke teardown needs: which shadow CompositionDefinition to
// remove, and where the shared chart-inspector lives so it can be removed once no shadow CDs remain.
type RemoteSeedTeardownOptions struct {
	Namespace      string
	CDName         string
	ChartInspector ChartInspectorCoords
}

// TeardownRemoteSeed reverses SeedRemoteTarget + ProjectChartInspector when a remote hub CR is
// deleted. It always removes this CD's shadow CompositionDefinition; then, ref-counted by the shadow
// CDs still present on the spoke, it removes the SHARED chart-inspector set and the
// compositiondefinitions CRD when this was the last remote composition on the target. Idempotent —
// NotFound (incl. the CRD already gone) is treated as success. Applied through the spoke-swapped
// clients.
func TeardownRemoteSeed(ctx context.Context, kube client.Client, dyn dynamic.Interface, opts RemoteSeedTeardownOptions) error {
	res := dyn.Resource(compositionDefinitionGVR)

	// 1 — this CD's shadow CompositionDefinition.
	err := res.Namespace(opts.Namespace).Delete(ctx, opts.CDName, metav1.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		if meta.IsNoMatchError(err) {
			return nil // the CRD is already gone, so nothing to tear down
		}
		return fmt.Errorf("deleting shadow CompositionDefinition %q: %w", opts.CDName, err)
	}

	// 2 — ref-count. The chart-inspector + CD CRD stay while ANY OTHER remote composition still has a
	// shadow CD on the spoke. Exclude THIS CD's own shadow (whether or not the delete above is yet
	// visible to the list) and any already-terminating shadow — otherwise the last-one-out cleanup
	// races our own delete: on the reconcile that removes the finalizer, a list that still shows our
	// just-deleted shadow would wrongly keep the shared infra, and no further reconcile retries.
	list, err := res.Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			return nil
		}
		return fmt.Errorf("listing shadow CompositionDefinitions on target: %w", err)
	}
	for i := range list.Items {
		it := &list.Items[i]
		if it.GetNamespace() == opts.Namespace && it.GetName() == opts.CDName {
			continue // this CD's own shadow (the delete above may not be visible yet)
		}
		if it.GetDeletionTimestamp() != nil {
			continue // already terminating
		}
		return nil // another remote composition still needs the chart-inspector + CD CRD
	}

	// 3 — last one out: remove the shared chart-inspector set and the compositiondefinitions CRD.
	if err := DeleteProjectedChartInspector(ctx, kube, opts.ChartInspector); err != nil {
		return err
	}
	crd := &apiextensionsv1.CustomResourceDefinition{
		TypeMeta: metav1.TypeMeta{APIVersion: "apiextensions.k8s.io/v1", Kind: "CustomResourceDefinition"},
		ObjectMeta: metav1.ObjectMeta{
			Name: compositionDefinitionGVR.Resource + "." + compositionDefinitionGVR.Group,
		},
	}
	if err := kube.Delete(ctx, crd); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting compositiondefinitions CRD on target: %w", err)
	}
	return nil
}

// waitCRDServed polls until the given resource is served by the target API (the CRD is Established),
// so the subsequent shadow-CR create does not race CRD registration.
func waitCRDServed(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		_, err := dyn.Resource(gvr).Namespace("").List(ctx, metav1.ListOptions{Limit: 1})
		if err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
