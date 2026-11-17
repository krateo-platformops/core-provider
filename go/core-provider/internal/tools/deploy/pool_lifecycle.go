package deploy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
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

// PoolOptions is everything EnsurePool needs to apply one pool.
type PoolOptions struct {
	Pool      string
	Namespace string // the management namespace (controllerNamespace)
	// Members are the pool's members as rendered (their env ConfigMap data), a pure function of the
	// live local definitions naming Pool. Never empty here: an empty pool is deleted (DeletePool).
	Members                []PoolMember
	Sizing                 PoolSizing
	ConfigmapTemplatePath  string // assets: pool-configmap.yaml
	DeploymentTemplatePath string // assets: pool-deployment.yaml
	AuthnURL               string
	DynClient              dynamic.Interface
	DryRunServer           bool
}

// PoolServiceAccount is the pool's ServiceAccount: the subject of every member's derived RBAC, its
// ownership Lease grant and its RESTAction RBAC (§5c Objects).
func PoolServiceAccount(pool, namespace string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: PoolNames(pool).Workload}
}

// EnsurePool applies pool's objects, idempotently (§5c Lifecycle): its ServiceAccount, its Role and
// RoleBinding (read access to the index and to each member's env ConfigMap, by resourceNames), its
// process ConfigMap, the membership index, and its Deployment. Called from every member's reconcile;
// the index is recomputed each time, so there is no count to drift.
func EnsurePool(ctx context.Context, opts PoolOptions) error {
	if err := ValidatePoolName(opts.Pool); err != nil {
		return err
	}
	if len(opts.Members) == 0 {
		return fmt.Errorf("pool %s: no members; an empty pool is deleted, not ensured", opts.Pool)
	}
	applyOpts := kubecli.ApplyOptions{}
	if opts.DryRunServer {
		applyOpts.DryRun = []string{"All"}
	}
	names := PoolNames(opts.Pool)
	saNN := PoolServiceAccount(opts.Pool, opts.Namespace)
	labels := map[string]string{PoolLabel: opts.Pool, "app.kubernetes.io/part-of": "krateo", "app.kubernetes.io/managed-by": "krateo"}

	sa := corev1.ServiceAccount{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"},
		ObjectMeta: metav1.ObjectMeta{Name: saNN.Name, Namespace: saNN.Namespace, Labels: labels},
	}
	if _, err := applyRecorded(ctx, opts.DynClient, serviceAccountGVR, "ServiceAccount", auditrec.ActionRBACApply, &sa, applyOpts); err != nil {
		return fmt.Errorf("pool %s: applying its ServiceAccount: %w", opts.Pool, err)
	}
	role, binding := PoolRBAC(opts.Pool, opts.Namespace, opts.Members)
	if _, err := applyRecorded(ctx, opts.DynClient, roleGVR, "Role", auditrec.ActionRBACApply, &role, applyOpts); err != nil {
		return fmt.Errorf("pool %s: applying its Role: %w", opts.Pool, err)
	}
	if _, err := applyRecorded(ctx, opts.DynClient, roleBindingGVR, "RoleBinding", auditrec.ActionRBACApply, &binding, applyOpts); err != nil {
		return fmt.Errorf("pool %s: applying its RoleBinding: %w", opts.Pool, err)
	}

	cm := corev1.ConfigMap{}
	if err := objects.CreateK8sObject(&cm, schema.GroupVersionResource{}, types.NamespacedName{Namespace: opts.Namespace, Name: names.EnvConfigMap},
		opts.ConfigmapTemplatePath,
		"pool", opts.Pool,
		"poolConfigMap", names.EnvConfigMap,
		"poolDeployment", names.Workload,
		"poolIndex", names.Index,
		"poolWorkers", opts.Sizing.Workers,
		"composition_controller_sa_name", saNN.Name,
		"composition_controller_sa_namespace", saNN.Namespace,
		"authn_url", opts.AuthnURL); err != nil {
		return fmt.Errorf("pool %s: rendering its process ConfigMap: %w", opts.Pool, err)
	}
	if _, err := kubecli.Apply(ctx, opts.DynClient, configMapGVR, &cm, applyOpts); err != nil {
		return fmt.Errorf("pool %s: applying its process ConfigMap: %w", opts.Pool, err)
	}

	index, err := RenderPoolIndex(opts.Pool, opts.Namespace, opts.Members)
	if err != nil {
		return err
	}
	if _, err := kubecli.Apply(ctx, opts.DynClient, configMapGVR, &index, applyOpts); err != nil {
		return fmt.Errorf("pool %s: applying its membership index: %w", opts.Pool, err)
	}

	resources, err := json.Marshal(opts.Sizing.Resources)
	if err != nil {
		return fmt.Errorf("pool %s: encoding its resources: %w", opts.Pool, err)
	}
	dep := appsv1.Deployment{}
	if err := objects.CreateK8sObject(&dep, schema.GroupVersionResource{}, types.NamespacedName{Namespace: opts.Namespace, Name: names.Workload},
		opts.DeploymentTemplatePath,
		"pool", opts.Pool,
		"poolDeployment", names.Workload,
		"poolConfigMap", names.EnvConfigMap,
		"poolServiceAccount", saNN.Name,
		"poolResources", string(resources),
		"config_checksum", configChecksum(cm.Data)); err != nil {
		return fmt.Errorf("pool %s: rendering its Deployment: %w", opts.Pool, err)
	}
	if _, err := kubecli.Apply(ctx, opts.DynClient, deploymentGVR, &dep, applyOpts); err != nil {
		return fmt.Errorf("pool %s: applying its Deployment: %w", opts.Pool, err)
	}
	return nil
}

// PoolRBAC is the pool's least-privilege read of its own configuration (§5c Membership): get, list
// and watch on its index and get on each member's env ConfigMap, all by resourceNames. Watching by
// label would need a namespace-wide ConfigMap read in the management namespace, which RBAC cannot
// narrow by label, so the Role is rewritten as members join and leave. A pure function of the
// members, sorted, so an unchanged membership renders an unchanged Role.
func PoolRBAC(pool, namespace string, members []PoolMember) (rbacv1.Role, rbacv1.RoleBinding) {
	names := PoolNames(pool)
	envs := make([]string, 0, len(members))
	for _, m := range members {
		envs = append(envs, MemberEnvConfigMapName(m.GVR))
	}
	sort.Strings(envs)
	labels := map[string]string{PoolLabel: pool, "app.kubernetes.io/part-of": "krateo", "app.kubernetes.io/managed-by": "krateo"}
	role := rbacv1.Role{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{Name: names.Workload, Namespace: namespace, Labels: labels},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: []string{names.Index}, Verbs: []string{"get", "list", "watch"}},
			{APIGroups: []string{""}, Resources: []string{"configmaps"}, ResourceNames: envs, Verbs: []string{"get"}},
		},
	}
	binding := rbacv1.RoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: names.Workload, Namespace: namespace, Labels: labels},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: names.Workload},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: names.Workload, Namespace: namespace}},
	}
	return role, binding
}

// DeletePool removes pool's objects (§5c Lifecycle: the LAST LEAVER deletes them). The Deployment
// goes first, so the process stops before the permissions and configuration it reads disappear.
// NotFound is success; any other error stops and is returned, so a pool is never half-deleted
// silently.
func DeletePool(ctx context.Context, dyn dynamic.Interface, pool, namespace string) error {
	if err := ValidatePoolName(pool); err != nil {
		return err
	}
	names := PoolNames(pool)
	for _, o := range []struct {
		gvr    schema.GroupVersionResource
		kind   string
		action string
		obj    client.Object
	}{
		{deploymentGVR, "Deployment", "", &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: names.Workload, Namespace: namespace}}},
		{configMapGVR, "ConfigMap", "", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.Index, Namespace: namespace}}},
		{configMapGVR, "ConfigMap", "", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: names.EnvConfigMap, Namespace: namespace}}},
		{roleBindingGVR, "RoleBinding", auditrec.ActionRBACDelete, &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: names.Workload, Namespace: namespace}}},
		{roleGVR, "Role", auditrec.ActionRBACDelete, &rbacv1.Role{ObjectMeta: metav1.ObjectMeta{Name: names.Workload, Namespace: namespace}}},
		{serviceAccountGVR, "ServiceAccount", auditrec.ActionRBACDelete, &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: names.Workload, Namespace: namespace}}},
	} {
		var err error
		if o.action != "" {
			_, err = uninstallRecorded(ctx, dyn, o.gvr, o.kind, o.action, o.obj, kubecli.UninstallOptions{})
		} else {
			_, err = kubecli.Uninstall(ctx, dyn, o.gvr, o.obj, kubecli.UninstallOptions{})
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("pool %s: deleting %s %s: %w", pool, o.kind, o.obj.GetName(), err)
		}
	}
	return nil
}
