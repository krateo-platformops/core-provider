package deploy

import (
	"fmt"

	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// deploymentApplyOptions are the options the controller Deployment is applied with: the shared
// ones, plus the keep-live-replicas hook for a scale-to-zero definition. Extracted so the decision
// is tested on its own, not only through a full Deploy.
func deploymentApplyOptions(base kubecli.ApplyOptions, opts DeployOptions) kubecli.ApplyOptions {
	if opts.KeepLiveReplicas {
		base.FromCurrent = keepLiveReplicas
	}
	return base
}

// keepLiveReplicas is the objectclient FromCurrent hook for a scale-to-zero definition's controller
// Deployment (#393, see DeployOptions.KeepLiveReplicas): it copies spec.replicas from the live
// object being replaced into the desired one, so the full Update leaves replicas to the scaletozero
// helper.
//
// A live object without spec.replicas keeps the rendered value: the apiserver defaults the field,
// so it is absent only on an object that never went through it, and copying "absent" would make
// the Update omit replicas, which the apiserver would default to 1 and wake a controller at 0.
// A replicas field that is present but not an integer is an error, not a guess.
func keepLiveReplicas(current, desired *unstructured.Unstructured) error {
	r, found, err := unstructured.NestedFieldNoCopy(current.Object, "spec", "replicas")
	if err != nil {
		return fmt.Errorf("reading the live controller Deployment's spec.replicas: %w", err)
	}
	if !found {
		return nil
	}
	n, ok := r.(int64)
	if !ok {
		return fmt.Errorf("the live controller Deployment's spec.replicas is a %T, not an integer", r)
	}
	return unstructured.SetNestedField(desired.Object, n, "spec", "replicas")
}
