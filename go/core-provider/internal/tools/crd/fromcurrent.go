package crd

import (
	"context"
	"fmt"

	"github.com/krateo-platformops/core-provider/internal/tools/kube"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/dynamic"
)

// crdGVR is the CustomResourceDefinition resource every CRD write here targets.
var crdGVR = apiextensionsv1.SchemeGroupVersion.WithResource("customresourcedefinitions")

// ApplyProjected writes a CRD that core-provider shares with other writers (#633). The object sent
// on the update path is project(current), where current is the CRD Apply has JUST read from the
// apiserver on that attempt (objectclient.ApplyOptions.FromCurrent), never the copy the caller read
// earlier: that copy may come from the controller-runtime cache, and objectclient.Apply attaches the
// resourceVersion of the object it re-reads, so writing the caller's copy silently overwrote
// whatever another writer changed in between (annotations such as provider-runtime's credential-use
// Disown switch, labels, fields core-provider does not own). Because the written object is derived
// from the very version whose resourceVersion it carries, a concurrent write makes the Update fail
// with a conflict, and Apply re-reads and calls project again: both changes survive.
//
// seed is what is created when no CRD exists yet (project is not called then). project must be a
// pure function of current: it receives a copy, and is called again on every retry.
func ApplyProjected(ctx context.Context, dyn dynamic.Interface, seed *apiextensionsv1.CustomResourceDefinition,
	project func(current *apiextensionsv1.CustomResourceDefinition) (*apiextensionsv1.CustomResourceDefinition, error)) error {
	seed = seed.DeepCopy()
	seed.SetGroupVersionKind(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"))
	_, err := kube.Apply(ctx, dyn, crdGVR, seed, kube.ApplyOptions{
		FromCurrent: func(current, desired *unstructured.Unstructured) error {
			var cur apiextensionsv1.CustomResourceDefinition
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(current.UnstructuredContent(), &cur); err != nil {
				return fmt.Errorf("decoding the CRD being replaced: %w", err)
			}
			out, err := project(cur.DeepCopy())
			if err != nil {
				return err
			}
			out.SetGroupVersionKind(apiextensionsv1.SchemeGroupVersion.WithKind("CustomResourceDefinition"))
			u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(out)
			if err != nil {
				return fmt.Errorf("encoding the projected CRD: %w", err)
			}
			desired.SetUnstructuredContent(u)
			return nil
		},
	})
	return err
}
