package deploy

import (
	"context"
	"fmt"
	"time"

	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
)

// poolMember says this Kind x version's controller runs in a pool (docs/design/cdc-pooling.md §5c).
func (o DeployOptions) poolMember() bool { return o.Pool != "" }

// controllerSubject is the identity the controller's RBAC binds: the per-version ServiceAccount
// rendered for a dedicated controller (own=true: applied and looked up with the rest), or the POOL's
// ServiceAccount for a member (own=false: EnsurePool applies it, so Deploy and Lookup only hash its
// name, both in the same place, and the digests still agree).
func controllerSubject(o DeployOptions, rendered corev1.ServiceAccount) (sa corev1.ServiceAccount, own bool) {
	if !o.poolMember() {
		return rendered, true
	}
	nn := PoolServiceAccount(o.Pool, rendered.Namespace)
	return corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: nn.Name, Namespace: nn.Namespace},
	}, false
}

// repointBindings makes the controller's own bindings name sa (a pool member's: the pool's
// ServiceAccount). The templates render the per-version name; every ServiceAccount subject is
// rewritten, and nothing else in them changes.
func repointBindings(sa corev1.ServiceAccount, bindings ...*[]rbacv1.Subject) {
	for _, subjects := range bindings {
		for i := range *subjects {
			if (*subjects)[i].Kind == rbacv1.ServiceAccountKind {
				(*subjects)[i].Name, (*subjects)[i].Namespace = sa.Name, sa.Namespace
			}
		}
	}
}

// deployedWithLease reports whether gvr's controller was last deployed with its ownership Lease, from
// its live env ConfigMap: one deployed before the Lease existed (step 2) does not hold it.
func deployedWithLease(ctx context.Context, dyn dynamic.Interface, namespace string, gvr schema.GroupVersionResource) (bool, error) {
	cm := corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: MemberEnvConfigMapName(gvr), Namespace: namespace}}
	err := kubecli.Get(ctx, dyn, configMapGVR, &cm)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading the live env ConfigMap of %s: %w", gvr, err)
	}
	return cm.Data[memberKeyLease] != "", nil
}

// joinPool is a member's last step of Deploy: its pool runs it, and its dedicated controller is retired
// (§5c Cutover). The pool is ensured FIRST, so the controller is never in neither place: the pool's
// member waits on the ownership Lease, which the dedicated controller holds until it has drained.
func joinPool(ctx context.Context, o DeployOptions, controller types.NamespacedName, dedicatedSA corev1.ServiceAccount) error {
	if o.Pools == nil {
		return fmt.Errorf("pool %s: a controller that does not run beside its pool cannot join it", o.Pool)
	}
	if err := EnsurePool(ctx, *o.Pools, o.Pool); err != nil {
		return err
	}
	return cutoverToPool(ctx, o, controller, dedicatedSA)
}

// poolCutoverDrainTimeout bounds how long joining a pool waits for the dedicated controller to exit.
const poolCutoverDrainTimeout = 5 * time.Minute

// cutoverToPool retires the dedicated controller of a Kind x version that has joined pool o.Pool
// (§5c Cutover, stop-before-start by step 2's Lease):
//  1. the compositions' derived RBAC is re-subjected to the pool's ServiceAccount, so the pool's
//     member has its permissions before it is the writer;
//  2. the dedicated Deployment is deleted and its pod's exit awaited: it drains and releases the
//     ownership Lease, and the pool's member starts its workers only on that Lease, so there is never
//     a second writer;
//  3. the dedicated Service and ServiceAccount are deleted.
//
// Idempotent: on a member whose dedicated objects are already gone every step is a no-op. A failure
// is returned; nothing after it runs, so a half-done cutover is retried, never skipped.
func cutoverToPool(ctx context.Context, o DeployOptions, controller types.NamespacedName, dedicatedSA corev1.ServiceAccount) error {
	pool := PoolServiceAccount(o.Pool, controller.Namespace)
	if _, err := ResubjectDerivedRBAC(ctx, ResubjectOptions{
		DynamicClient: o.DynClient, Namespace: dedicatedSA.Namespace, NewNamespace: pool.Namespace,
		OldGVR: o.GVR, NewGVR: o.GVR, NewServiceAccount: pool.Name,
	}); err != nil {
		return fmt.Errorf("joining pool %s: re-subjecting derived RBAC to %s: %w", o.Pool, pool, err)
	}
	dep := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: getCDCDeploymentNN(controller).Name, Namespace: controller.Namespace}}
	if err := kubecli.Get(ctx, o.DynClient, deploymentGVR, &dep); err == nil {
		if _, err := kubecli.Uninstall(ctx, o.DynClient, deploymentGVR, &dep, kubecli.UninstallOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("joining pool %s: deleting the dedicated Deployment %s: %w", o.Pool, dep.Name, err)
		}
		if err := waitForControllerDrained(ctx, o.KubeClient, &dep, poolCutoverDrainTimeout); err != nil {
			return fmt.Errorf("joining pool %s: waiting for the dedicated controller %s to exit: %w", o.Pool, dep.Name, err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("joining pool %s: reading the dedicated Deployment %s: %w", o.Pool, dep.Name, err)
	}
	svc := corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: getCDCDeploymentNN(controller).Name, Namespace: controller.Namespace}}
	if _, err := kubecli.Uninstall(ctx, o.DynClient, serviceGVR, &svc, kubecli.UninstallOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("joining pool %s: deleting the dedicated Service %s: %w", o.Pool, svc.Name, err)
	}
	if _, err := uninstallRecorded(ctx, o.DynClient, serviceAccountGVR, "ServiceAccount", auditrec.ActionRBACDelete, &dedicatedSA, kubecli.UninstallOptions{}); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("joining pool %s: deleting the dedicated ServiceAccount %s: %w", o.Pool, dedicatedSA.Name, err)
	}
	return nil
}
