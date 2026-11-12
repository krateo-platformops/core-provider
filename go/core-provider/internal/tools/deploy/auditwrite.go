package deploy

import (
	"context"

	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/plumbing/kubeutil/objectclient"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// applyRecorded applies obj and records the write under action (core-provider#423), in the scope
// the reconcile put on ctx. A dry run persists nothing and records nothing.
func applyRecorded(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, kind, action string, obj client.Object, opts kubecli.ApplyOptions) (objectclient.ApplyResult, error) {
	t := auditrec.Target{GVR: gvr, Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName()}
	res, err := kubecli.Apply(ctx, dyn, gvr, obj, opts)
	if len(opts.DryRun) == 0 {
		auditrec.RecordApply(ctx, action, t, res, err)
	}
	return res, err
}

// uninstallRecorded deletes obj and records the deletion under action.
func uninstallRecorded(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, kind, action string, obj client.Object, opts kubecli.UninstallOptions) (objectclient.UninstallResult, error) {
	t := auditrec.Target{GVR: gvr, Kind: kind, Namespace: obj.GetNamespace(), Name: obj.GetName()}
	res, err := kubecli.Uninstall(ctx, dyn, gvr, obj, opts)
	if len(opts.DryRun) == 0 {
		auditrec.RecordDelete(ctx, action, t, res, err)
	}
	return res, err
}
