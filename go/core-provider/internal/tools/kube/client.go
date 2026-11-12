package kube

import (
	"context"

	"github.com/krateo-platformops/plumbing/kubeutil/objectclient"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Apply creates obj if no object with its namespace/name exists yet, or updates it (carrying over the
// current resourceVersion) if one does, then copies the server's response back into obj. A thin,
// typed-object wrapper over plumbing/kubeutil/objectclient (which retries internally on any transient
// error): obj's GroupVersionKind must be set (as it already is for objects rendered from a YAML template,
// or built as a Go struct literal with TypeMeta set explicitly) since gvr cannot be inferred from a
// dynamic-client call the way client.Client's REST mapper would.
//
// It returns objectclient's report of the write (created, updated, or an Update the apiserver
// persisted nothing for; on error, the write the final attempt sent): core-provider#423 records
// one audit row per real change from it. A caller with no use for it discards it.
func Apply(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, obj client.Object, opts ApplyOptions) (objectclient.ApplyResult, error) {
	u, err := toUnstructured(obj)
	if err != nil {
		return objectclient.ApplyResult{}, err
	}
	res, err := objectclient.Apply(ctx, dyn, gvr, u, opts)
	if err != nil {
		return res, err
	}
	return res, fromUnstructured(u, obj)
}

// Uninstall deletes obj by namespace/name. IsNotFound-tolerant (via objectclient.Uninstall), and
// reports whether it deleted (UninstallDeleted) or found nothing (UninstallAbsent).
func Uninstall(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, obj client.Object, opts UninstallOptions) (objectclient.UninstallResult, error) {
	return objectclient.Uninstall(ctx, dyn, gvr, obj.GetNamespace(), obj.GetName(), opts)
}

// Get reads obj by namespace/name, populating it in place.
func Get(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, obj client.Object) error {
	u, err := objectclient.Get(ctx, dyn, gvr, obj.GetNamespace(), obj.GetName())
	if err != nil {
		return err
	}
	return fromUnstructured(u, obj)
}

func toUnstructured(obj client.Object) (*unstructured.Unstructured, error) {
	raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: raw}, nil
}

func fromUnstructured(u *unstructured.Unstructured, obj client.Object) error {
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, obj)
}
