package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"

	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	deployment "github.com/krateo-platformops/core-provider/internal/tools/deployment"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PoolSizing is one pool's size (docs/design/cdc-pooling.md §5c, "Sizing and workers"): its total
// concurrent reconciles across members, and its controller container's resources.
type PoolSizing struct {
	Workers   int            `json:"workers"`
	Resources map[string]any `json:"resources"`
}

// poolSizingFile is the chart's cdc.pool block as the chart renders it (templates/cdc-deployment.yaml,
// key pool-sizing.json): core-provider reads no chart values at runtime, so the chart serialises the
// block next to the asset that uses it.
type poolSizingFile struct {
	Default   map[string]any            `json:"default"`
	Overrides map[string]map[string]any `json:"overrides"`
}

// LoadPoolSizing resolves pool's size from the chart's cdc.pool block at path: overrides.<pool>
// DEEP-merged over default, so an override that states only resources.limits.memory keeps the
// default's requests and workers (Diego, 2026-11-16). A missing or unreadable file is an error, not
// a default: a pool sized from nothing would start with no memory limit.
func LoadPoolSizing(path, pool string) (PoolSizing, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return PoolSizing{}, fmt.Errorf("reading the chart's pool sizing (%s): %w", path, err)
	}
	var f poolSizingFile
	if err := json.Unmarshal(b, &f); err != nil {
		return PoolSizing{}, fmt.Errorf("decoding the chart's pool sizing (%s): %w", path, err)
	}
	if f.Default == nil {
		return PoolSizing{}, fmt.Errorf("the chart's pool sizing (%s) has no default", path)
	}
	merged := deepMerge(f.Default, f.Overrides[pool])
	mb, err := json.Marshal(merged)
	if err != nil {
		return PoolSizing{}, err
	}
	var out PoolSizing
	if err := json.Unmarshal(mb, &out); err != nil {
		return PoolSizing{}, fmt.Errorf("pool %s sizing: %w", pool, err)
	}
	if out.Workers < 1 {
		return PoolSizing{}, fmt.Errorf("pool %s sizing: workers must be at least 1, got %d", pool, out.Workers)
	}
	return out, nil
}

// deepMerge returns base with over laid on top: maps merge key by key, anything else in over
// replaces. Neither input is modified.
func deepMerge(base, over map[string]any) map[string]any {
	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	for k, ov := range over {
		bm, bok := out[k].(map[string]any)
		om, ook := ov.(map[string]any)
		if bok && ook {
			out[k] = deepMerge(bm, om)
			continue
		}
		out[k] = ov
	}
	return out
}

// PoolAssets are the chart assets a pool's objects are rendered from, resolved by the caller.
type PoolAssets struct {
	SizingPath             string // assets: pool-sizing.json
	ConfigmapTemplatePath  string // assets: pool-configmap.yaml
	DeploymentTemplatePath string // assets: pool-deployment.yaml
	AuthnURL               string
}

// PoolOptions is everything needed to apply, read back or delete the pools of one namespace.
type PoolOptions struct {
	Namespace string // the management namespace (controllerNamespace)
	Assets    PoolAssets
	DynClient dynamic.Interface
	// KubeClient lists a pool's pods while it is being deleted.
	KubeClient client.Client
}

// PoolServiceAccount is the pool's ServiceAccount: the subject of every member's derived RBAC, its
// ownership Lease grant and its RESTAction RBAC (§5c Objects).
func PoolServiceAccount(pool, namespace string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: PoolNames(pool).Workload}
}

// EnsurePool applies pool's objects, idempotently (§5c Lifecycle): its ServiceAccount, its Role and
// RoleBinding (read access to the index and to each member's env ConfigMap, by resourceNames), its
// process ConfigMap, the membership index, and its Deployment. The members are the pool's live env
// ConfigMaps (pool_index.go), recomputed here, so there is no count to drift. A member is granted its
// env ConfigMap before the index names it.
func EnsurePool(ctx context.Context, opts PoolOptions, pool string) error {
	if err := ValidatePoolName(pool); err != nil {
		return err
	}
	defer lockPool(opts.Namespace, pool)()
	return ensurePoolLocked(ctx, opts, pool)
}

func ensurePoolLocked(ctx context.Context, opts PoolOptions, pool string) error {
	applyOpts := kubecli.ApplyOptions{}
	saNN := PoolServiceAccount(pool, opts.Namespace)
	sa := corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: saNN.Name, Namespace: saNN.Namespace, Labels: poolLabels(pool)},
	}
	if _, err := applyRecorded(ctx, opts.DynClient, serviceAccountGVR, "ServiceAccount", auditrec.ActionRBACApply, &sa, applyOpts); err != nil {
		return fmt.Errorf("pool %s: applying its ServiceAccount: %w", pool, err)
	}
	cm, err := renderPoolConfigMap(opts, pool)
	if err != nil {
		return err
	}
	dep, err := renderPoolDeployment(opts, pool, cm.Data)
	if err != nil {
		return err
	}
	if _, err := kubecli.Apply(ctx, opts.DynClient, configMapGVR, &cm, applyOpts); err != nil {
		return fmt.Errorf("pool %s: applying its process ConfigMap: %w", pool, err)
	}

	if err := syncPoolIndex(ctx, opts.DynClient, opts.Namespace, pool); err != nil {
		return err
	}
	if _, err := kubecli.Apply(ctx, opts.DynClient, deploymentGVR, &dep, applyOpts); err != nil {
		return fmt.Errorf("pool %s: applying its Deployment: %w", pool, err)
	}
	return nil
}

// ReconcilePools brings the pools in touched to their members, and deletes every pool of the namespace
// that has none left (§5c Lifecycle: the LAST LEAVER deletes it). A pool whose only ConfigMaps are
// draining is kept: its process is still draining them. Anything that cannot be read changes nothing.
func ReconcilePools(ctx context.Context, opts PoolOptions, touched map[string]bool) error {
	pools, err := listPools(ctx, opts.DynClient, opts.Namespace)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(pools))
	for p := range pools {
		names = append(names, p)
	}
	for p := range touched {
		if _, ok := pools[p]; !ok {
			names = append(names, p)
		}
	}
	sort.Strings(names)
	for _, p := range names {
		if err := reconcilePool(ctx, opts, p, touched[p]); err != nil {
			return err
		}
	}
	return nil
}

func reconcilePool(ctx context.Context, opts PoolOptions, pool string, touched bool) error {
	if err := ValidatePoolName(pool); err != nil {
		return err
	}
	defer lockPool(opts.Namespace, pool)()
	// Under the lock, so a member that joined since the caller's list is seen and its pool is not
	// deleted. Correctness does not rest on it: the joiner's own EnsurePool takes this lock after, and
	// recreates a pool deleted under it before its dedicated controller is retired.
	pools, err := listPools(ctx, opts.DynClient, opts.Namespace)
	if err != nil {
		return err
	}
	v, exists := pools[pool]
	switch {
	case len(v.members)+len(v.draining) > 0:
		if !touched {
			return nil
		}
		return ensurePoolLocked(ctx, opts, pool)
	case exists:
		return DeletePool(ctx, opts, pool)
	}
	return nil
}

// PoolsOfKind are the pools that run a controller of group/resource at any version, or are draining one.
func PoolsOfKind(ctx context.Context, dyn dynamic.Interface, namespace, group, resource string) (map[string]bool, error) {
	pools, err := listPools(ctx, dyn, namespace)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for p, v := range pools {
		for _, m := range append(append([]PoolMember{}, v.members...), v.draining...) {
			if m.GVR.Group == group && m.GVR.Resource == resource {
				out[p] = true
			}
		}
	}
	return out, nil
}

// PoolDrift says what is not current about gvr's membership of pool, or about the pool itself, or "".
// It is what makes the pool's objects part of a member's Observe: they are in no definition's digest
// (Deploy and Lookup stop at the member's own objects), so without it a lost index entry, or a release
// that changes only the pool's image or sizing, would stay as it is until some member's next Update.
func PoolDrift(ctx context.Context, opts PoolOptions, pool string, gvr schema.GroupVersionResource) (string, error) {
	if err := ValidatePoolName(pool); err != nil {
		return "", err
	}
	pools, err := listPools(ctx, opts.DynClient, opts.Namespace)
	if err != nil {
		return "", err
	}
	v := pools[pool]
	names := PoolNames(pool)
	if _, draining, ok := v.find(gvr); !ok || draining {
		return "pool " + pool + ": this controller's env ConfigMap is not a member's", nil
	}
	for p, other := range pools {
		if _, listed := other.indexData()[MemberKey(gvr)]; listed && p != pool {
			return "pool " + p + " still lists this controller", nil
		}
	}
	index, err := RenderPoolIndex(pool, opts.Namespace, v.members)
	if err != nil {
		return "", err
	}
	if v.index == nil || !sameData(v.indexData(), index.Data) {
		return "pool " + pool + ": its membership index is not current", nil
	}

	role, binding := PoolRBAC(pool, opts.Namespace, v.envNames())
	liveRole, liveBinding := role, binding
	sa := corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: names.Workload, Namespace: opts.Namespace}}
	cm, err := renderPoolConfigMap(opts, pool)
	if err != nil {
		return "", err
	}
	liveCM := corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: cm.Name, Namespace: cm.Namespace}}
	dep, err := renderPoolDeployment(opts, pool, cm.Data)
	if err != nil {
		return "", err
	}
	liveDep := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: dep.Name, Namespace: dep.Namespace}}
	for _, o := range []struct {
		gvr  schema.GroupVersionResource
		kind string
		obj  client.Object
	}{
		{roleGVR, "Role", &liveRole},
		{roleBindingGVR, "RoleBinding", &liveBinding},
		{serviceAccountGVR, "ServiceAccount", &sa},
		{configMapGVR, "process ConfigMap", &liveCM},
		{deploymentGVR, "Deployment", &liveDep},
	} {
		err := kubecli.Get(ctx, opts.DynClient, o.gvr, o.obj)
		if apierrors.IsNotFound(err) {
			return "pool " + pool + ": its " + o.kind + " does not exist", nil
		}
		if err != nil {
			return "", fmt.Errorf("pool %s: reading its %s: %w", pool, o.kind, err)
		}
	}
	switch {
	case !reflect.DeepEqual(liveRole.Rules, role.Rules):
		return "pool " + pool + ": its Role is not current", nil
	case !reflect.DeepEqual(liveBinding.Subjects, binding.Subjects), liveBinding.RoleRef != binding.RoleRef:
		return "pool " + pool + ": its RoleBinding is not current", nil
	case !sameData(liveCM.Data, cm.Data):
		return "pool " + pool + ": its process ConfigMap is not current", nil
	}
	// The Deployment is compared through a server dry run, as a dedicated controller's is: the
	// apiserver fills the defaults the template leaves out.
	if _, err := kubecli.Apply(ctx, opts.DynClient, deploymentGVR, &dep, kubecli.ApplyOptions{DryRun: []string{"All"}}); err != nil {
		return "", fmt.Errorf("pool %s: dry-running its Deployment: %w", pool, err)
	}
	deployment.CleanFromRestartAnnotation(&dep)
	deployment.CleanFromRestartAnnotation(&liveDep)
	if !equality.Semantic.DeepEqual(dep.Spec, liveDep.Spec) {
		return "pool " + pool + ": its Deployment is not current", nil
	}
	return "", nil
}

func poolLabels(pool string) map[string]string {
	return map[string]string{PoolLabel: pool, "app.kubernetes.io/part-of": "krateo", "app.kubernetes.io/managed-by": "krateo"}
}

func renderPoolConfigMap(opts PoolOptions, pool string) (corev1.ConfigMap, error) {
	sizing, err := LoadPoolSizing(opts.Assets.SizingPath, pool)
	if err != nil {
		return corev1.ConfigMap{}, err
	}
	cm := corev1.ConfigMap{}
	if err := objects.CreateK8sObject(&cm, schema.GroupVersionResource{}, types.NamespacedName{Namespace: opts.Namespace, Name: PoolNames(pool).EnvConfigMap},
		opts.Assets.ConfigmapTemplatePath, poolConfigMapValues(opts, pool, sizing)...); err != nil {
		return corev1.ConfigMap{}, fmt.Errorf("pool %s: rendering its process ConfigMap: %w", pool, err)
	}
	return cm, nil
}

// PoolRBAC is the pool's least-privilege read of its own configuration (§5c Membership): get, list
// and watch on its index and get on each member's env ConfigMap, all by resourceNames. Watching by
// label would need a namespace-wide ConfigMap read in the management namespace, which RBAC cannot
// narrow by label, so the Role is rewritten as members join and leave. envs are the env ConfigMaps'
// names, sorted, so an unchanged membership renders an unchanged Role.
func PoolRBAC(pool, namespace string, envs []string) (rbacv1.Role, rbacv1.RoleBinding) {
	names := PoolNames(pool)
	labels := poolLabels(pool)
	role := rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: names.Workload, Namespace: namespace, Labels: labels},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{names.Index}, Verbs: []string{"get", "list", "watch"}},
		},
	}
	// A rule without resourceNames grants every ConfigMap of the namespace, so no env, no rule.
	if len(envs) > 0 {
		role.Rules = append(role.Rules, rbacv1.PolicyRule{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: envs, Verbs: []string{"get"}})
	}
	binding := rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: names.Workload, Namespace: namespace, Labels: labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: names.Workload},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: names.Workload, Namespace: namespace}},
	}
	return role, binding
}

// DeletePool removes pool's objects. Its Deployment goes first and its pods are waited for, so the
// process has stopped before the permissions and configuration it runs with disappear (#263). The
// process ConfigMap and the index go last: they are what a later ReconcilePools finds an unfinished
// deletion by. NotFound is success; any other error stops and is returned, so a pool is never
// half-deleted silently. The caller has established that no member is left.
func DeletePool(ctx context.Context, opts PoolOptions, pool string) error {
	if err := ValidatePoolName(pool); err != nil {
		return err
	}
	names := PoolNames(pool)
	meta := metav1.ObjectMeta{Name: names.Workload, Namespace: opts.Namespace}
	dep := appsv1.Deployment{ObjectMeta: meta}
	if err := kubecli.Get(ctx, opts.DynClient, deploymentGVR, &dep); err == nil {
		if _, err := kubecli.Uninstall(ctx, opts.DynClient, deploymentGVR, &dep, kubecli.UninstallOptions{}); err != nil {
			return fmt.Errorf("pool %s: deleting Deployment %s: %w", pool, names.Workload, err)
		}
		// Unobservable is not a reason to stop: every member has released its Lease by now, so the
		// process has nothing in flight, and waiting would never grant the permission (#270).
		if err := waitForControllerDrained(ctx, opts.KubeClient, &dep, 0); err != nil && !errors.Is(err, ErrDrainUnobservable) {
			return fmt.Errorf("pool %s: waiting for its process to exit: %w", pool, err)
		}
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("pool %s: reading Deployment %s: %w", pool, names.Workload, err)
	}
	for _, o := range []struct {
		gvr    schema.GroupVersionResource
		kind   string
		action string
		obj    client.Object
	}{
		{roleBindingGVR, "RoleBinding", auditrec.ActionRBACDelete, &rbacv1.RoleBinding{ObjectMeta: meta}},
		{roleGVR, "Role", auditrec.ActionRBACDelete, &rbacv1.Role{ObjectMeta: meta}},
		{serviceAccountGVR, "ServiceAccount", auditrec.ActionRBACDelete, &corev1.ServiceAccount{ObjectMeta: meta}},
		{configMapGVR, "ConfigMap", "", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.EnvConfigMap, Namespace: opts.Namespace}}},
		{configMapGVR, "ConfigMap", "", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.Index, Namespace: opts.Namespace}}},
	} {
		var err error
		if o.action != "" {
			_, err = uninstallRecorded(ctx, opts.DynClient, o.gvr, o.kind, o.action, o.obj, kubecli.UninstallOptions{})
		} else {
			_, err = kubecli.Uninstall(ctx, opts.DynClient, o.gvr, o.obj, kubecli.UninstallOptions{})
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("pool %s: deleting %s %s: %w", pool, o.kind, o.obj.GetName(), err)
		}
	}
	return nil
}

// renderPoolDeployment renders the pool's Deployment from its template (assets/cdc/
// pool-deployment.yaml): the pool's ServiceAccount, its process ConfigMap, its sizing, and that
// ConfigMap's checksum (processEnv is its data) on the pod.
func renderPoolDeployment(opts PoolOptions, pool string, processEnv map[string]string) (appsv1.Deployment, error) {
	names := PoolNames(pool)
	sizing, err := LoadPoolSizing(opts.Assets.SizingPath, pool)
	if err != nil {
		return appsv1.Deployment{}, err
	}
	resources, err := json.Marshal(sizing.Resources)
	if err != nil {
		return appsv1.Deployment{}, fmt.Errorf("pool %s: encoding its resources: %w", pool, err)
	}
	dep := appsv1.Deployment{}
	if err := objects.CreateK8sObject(&dep, schema.GroupVersionResource{}, types.NamespacedName{Namespace: opts.Namespace, Name: names.Workload},
		opts.Assets.DeploymentTemplatePath, poolDeploymentValues(opts, pool, string(resources), processEnv)...); err != nil {
		return appsv1.Deployment{}, fmt.Errorf("pool %s: rendering its Deployment: %w", pool, err)
	}
	return dep, nil
}
