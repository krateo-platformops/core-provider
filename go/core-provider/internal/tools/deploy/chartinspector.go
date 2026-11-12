package deploy

import (
	"context"
	"fmt"
	"github.com/krateo-platformops/core-provider/internal/tools/auditrec"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"net/url"
	"os"
	"strconv"
	"strings"

	kubecli "github.com/krateo-platformops/core-provider/internal/tools/kube"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ChartInspectorCoords locate the core-provider chart-inspector Service that a cdc's
// URL_CHART_INSPECTOR points at. On a remote target the cdc is projected but this Service is not, so
// that URL — a management-cluster DNS name — never resolves on the spoke and package inspection
// fails. Path A / Inc 2 projects the chart-inspector onto the spoke under the SAME name/namespace so
// the baked URL resolves locally, with no cdc-config change.
type ChartInspectorCoords struct {
	Name      string
	Namespace string
	Port      int32
}

// ChartInspectorCoordsFromConfigmapTemplate reads the cdc-configmap template (mounted from the
// core-provider chart) for URL_CHART_INSPECTOR and derives the Service {name,namespace,port}. That
// URL is the single source of truth core-provider already uses to wire every cdc, so Inc 2
// introduces no new configuration — it reuses what the chart set.
//
// The file is a Go TEMPLATE (unrendered {{ }} directives), so it is NOT valid YAML — the
// URL_CHART_INSPECTOR value is a static Service DNS, extracted here by line rather than parsed.
func ChartInspectorCoordsFromConfigmapTemplate(path string) (ChartInspectorCoords, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ChartInspectorCoords{}, fmt.Errorf("reading cdc-configmap template %q: %w", path, err)
	}
	const key = "URL_CHART_INSPECTOR:"
	rawURL := ""
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, key) {
			rawURL = strings.Trim(strings.TrimSpace(strings.TrimPrefix(t, key)), `"'`)
			break
		}
	}
	if rawURL == "" {
		return ChartInspectorCoords{}, fmt.Errorf("URL_CHART_INSPECTOR not found in cdc-configmap template %q", path)
	}
	return parseChartInspectorURL(rawURL)
}

// parseChartInspectorURL turns http://<name>.<ns>.svc.cluster.local:8081/ into coords.
func parseChartInspectorURL(rawURL string) (ChartInspectorCoords, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return ChartInspectorCoords{}, fmt.Errorf("parsing URL_CHART_INSPECTOR %q: %w", rawURL, err)
	}
	host := u.Hostname()
	parts := strings.Split(host, ".")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return ChartInspectorCoords{}, fmt.Errorf("URL_CHART_INSPECTOR host %q is not a <name>.<namespace>.svc form", host)
	}
	port := int32(8081)
	if p := u.Port(); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil {
			return ChartInspectorCoords{}, fmt.Errorf("URL_CHART_INSPECTOR port %q is not numeric: %w", p, err)
		}
		port = int32(n)
	}
	return ChartInspectorCoords{Name: parts[0], Namespace: parts[1], Port: port}, nil
}

// ProjectChartInspector reads the running chart-inspector workload from the management cluster
// (mgmt) by name and applies a sanitized copy onto the remote target (spoke) under the same
// name/namespace, so a projected cdc's URL_CHART_INSPECTOR resolves locally (Path A, Inc 2).
//
// READ-FROM-HUB: the projection uses the real running spec (image, args, RBAC) rather than embedded
// assets, so it tracks the chart automatically — a chart bump to the chart-inspector image/RBAC
// needs no code change here. Idempotent; every object is applied create-or-update through the
// spoke-swapped client.
func ProjectChartInspector(ctx context.Context, mgmt client.Client, spoke client.Client, spokeDynamic dynamic.Interface, coords ChartInspectorCoords) error {
	// target namespace on the spoke (the chart-inspector Service DNS lives in it).
	ns := &corev1.Namespace{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Namespace"},
		ObjectMeta: metav1.ObjectMeta{Name: coords.Namespace},
	}
	if _, err := kubecli.Apply(ctx, spokeDynamic, namespaceGVR, ns, kubecli.ApplyOptions{}); err != nil {
		return fmt.Errorf("ensuring chart-inspector namespace %q on target: %w", coords.Namespace, err)
	}

	// ServiceAccount (referenced by the Deployment; Secrets are cluster-local, drop them).
	sa := &corev1.ServiceAccount{}
	if err := mgmt.Get(ctx, client.ObjectKey{Namespace: coords.Namespace, Name: coords.Name}, sa); err != nil {
		return fmt.Errorf("reading chart-inspector ServiceAccount %q from hub: %w", coords.Name, err)
	}
	sa.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}
	stripServerMetadata(&sa.ObjectMeta)
	sa.Secrets = nil
	sa.ImagePullSecrets = nil
	if _, err := applyRecorded(ctx, spokeDynamic, serviceAccountGVR, "ServiceAccount", auditrec.ActionRBACApply, sa, kubecli.ApplyOptions{}); err != nil {
		return fmt.Errorf("projecting chart-inspector ServiceAccount: %w", err)
	}

	// Cluster RBAC bound to that SA (if any — chart-inspector reads charts, not necessarily the API).
	crb, err := findChartInspectorCRB(ctx, mgmt, coords)
	if err != nil {
		return err
	}
	if crb != nil {
		cr := &rbacv1.ClusterRole{}
		if err := mgmt.Get(ctx, client.ObjectKey{Name: crb.RoleRef.Name}, cr); err != nil {
			return fmt.Errorf("reading chart-inspector ClusterRole %q from hub: %w", crb.RoleRef.Name, err)
		}
		cr.TypeMeta = metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"}
		stripServerMetadata(&cr.ObjectMeta)
		if _, err := applyRecorded(ctx, spokeDynamic, clusterRoleGVR, "ClusterRole", auditrec.ActionRBACApply, cr, kubecli.ApplyOptions{}); err != nil {
			return fmt.Errorf("projecting chart-inspector ClusterRole: %w", err)
		}
		crb.TypeMeta = metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"}
		stripServerMetadata(&crb.ObjectMeta)
		if _, err := applyRecorded(ctx, spokeDynamic, clusterRoleBindingGVR, "ClusterRoleBinding", auditrec.ActionRBACApply, crb, kubecli.ApplyOptions{}); err != nil {
			return fmt.Errorf("projecting chart-inspector ClusterRoleBinding: %w", err)
		}
	}

	// Deployment (the workload behind the Service).
	dep := &appsv1.Deployment{}
	if err := mgmt.Get(ctx, client.ObjectKey{Namespace: coords.Namespace, Name: coords.Name}, dep); err != nil {
		return fmt.Errorf("reading chart-inspector Deployment %q from hub: %w", coords.Name, err)
	}
	// Project the ConfigMaps/Secrets the pod references (envFrom/env/volumes/imagePullSecrets) FIRST,
	// else its pods fail with CreateContainerConfigError on the spoke (the chart-inspector reads its
	// HOME / KRATEO_NAMESPACE from an envFrom ConfigMap).
	if err := projectPodRefs(ctx, mgmt, spoke, spokeDynamic, coords.Namespace, &dep.Spec.Template.Spec); err != nil {
		return err
	}
	dep.TypeMeta = metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"}
	stripServerMetadata(&dep.ObjectMeta)
	dep.Status = appsv1.DeploymentStatus{}
	if _, err := kubecli.Apply(ctx, spokeDynamic, deploymentGVR, dep, kubecli.ApplyOptions{}); err != nil {
		return fmt.Errorf("projecting chart-inspector Deployment: %w", err)
	}

	// Service — the DNS name the cdc's URL_CHART_INSPECTOR resolves to. Clear cluster-assigned
	// fields so the spoke's apiserver allocates its own.
	svc := &corev1.Service{}
	if err := mgmt.Get(ctx, client.ObjectKey{Namespace: coords.Namespace, Name: coords.Name}, svc); err != nil {
		return fmt.Errorf("reading chart-inspector Service %q from hub: %w", coords.Name, err)
	}
	svc.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Service"}
	stripServerMetadata(&svc.ObjectMeta)
	svc.Spec.ClusterIP = ""
	svc.Spec.ClusterIPs = nil
	svc.Status = corev1.ServiceStatus{}
	for i := range svc.Spec.Ports {
		svc.Spec.Ports[i].NodePort = 0
	}
	if _, err := kubecli.Apply(ctx, spokeDynamic, serviceGVR, svc, kubecli.ApplyOptions{}); err != nil {
		return fmt.Errorf("projecting chart-inspector Service: %w", err)
	}
	return nil
}

// stripServerMetadata clears server-owned metadata so an object read from one cluster can be applied
// to another (kubecli.Apply resolves the target resourceVersion on update).
func stripServerMetadata(m *metav1.ObjectMeta) {
	m.ResourceVersion = ""
	m.UID = ""
	m.Generation = 0
	m.CreationTimestamp = metav1.Time{}
	m.ManagedFields = nil
	m.OwnerReferences = nil
	m.SelfLink = ""
	m.DeletionTimestamp = nil
	if m.Annotations != nil {
		delete(m.Annotations, "kubectl.kubernetes.io/last-applied-configuration")
		delete(m.Annotations, "deployment.kubernetes.io/revision")
	}
}

// projectPodRefs projects every ConfigMap and Secret a pod spec references (envFrom, env valueFrom,
// volumes, imagePullSecrets) from the hub onto the spoke, in the given namespace. A referenced object
// that is absent on the hub is skipped (it may be optional or auto-created, e.g. kube-root-ca.crt).
func projectPodRefs(ctx context.Context, mgmt client.Client, spoke client.Client, spokeDynamic dynamic.Interface, namespace string, ps *corev1.PodSpec) error {
	cms, secrets := referencedConfig(ps)
	for _, name := range cms {
		cm := &corev1.ConfigMap{}
		if err := mgmt.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, cm); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("reading referenced ConfigMap %q from hub: %w", name, err)
		}
		cm.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "ConfigMap"}
		stripServerMetadata(&cm.ObjectMeta)
		if _, err := kubecli.Apply(ctx, spokeDynamic, configMapGVR, cm, kubecli.ApplyOptions{}); err != nil {
			return fmt.Errorf("projecting referenced ConfigMap %q: %w", name, err)
		}
	}
	for _, name := range secrets {
		sec := &corev1.Secret{}
		if err := mgmt.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sec); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("reading referenced Secret %q from hub: %w", name, err)
		}
		sec.TypeMeta = metav1.TypeMeta{APIVersion: "v1", Kind: "Secret"}
		stripServerMetadata(&sec.ObjectMeta)
		if _, err := applyRecorded(ctx, spokeDynamic, secretGVR, "Secret", auditrec.ActionSecretApply, sec, kubecli.ApplyOptions{}); err != nil {
			return fmt.Errorf("projecting referenced Secret %q: %w", name, err)
		}
	}
	return nil
}

// referencedConfig returns the ConfigMap and Secret names a pod spec depends on, deduplicated.
func referencedConfig(ps *corev1.PodSpec) (cms []string, secrets []string) {
	seen := map[string]bool{}
	addCM := func(n string) {
		if n != "" && !seen["cm/"+n] {
			seen["cm/"+n] = true
			cms = append(cms, n)
		}
	}
	addSecret := func(n string) {
		if n != "" && !seen["s/"+n] {
			seen["s/"+n] = true
			secrets = append(secrets, n)
		}
	}
	for _, ips := range ps.ImagePullSecrets {
		addSecret(ips.Name)
	}
	containers := append(append([]corev1.Container{}, ps.InitContainers...), ps.Containers...)
	for i := range containers {
		for _, ef := range containers[i].EnvFrom {
			if ef.ConfigMapRef != nil {
				addCM(ef.ConfigMapRef.Name)
			}
			if ef.SecretRef != nil {
				addSecret(ef.SecretRef.Name)
			}
		}
		for _, e := range containers[i].Env {
			if e.ValueFrom == nil {
				continue
			}
			if e.ValueFrom.ConfigMapKeyRef != nil {
				addCM(e.ValueFrom.ConfigMapKeyRef.Name)
			}
			if e.ValueFrom.SecretKeyRef != nil {
				addSecret(e.ValueFrom.SecretKeyRef.Name)
			}
		}
	}
	for _, v := range ps.Volumes {
		if v.ConfigMap != nil {
			addCM(v.ConfigMap.Name)
		}
		if v.Secret != nil {
			addSecret(v.Secret.SecretName)
		}
		if v.Projected != nil {
			for _, s := range v.Projected.Sources {
				if s.ConfigMap != nil {
					addCM(s.ConfigMap.Name)
				}
				if s.Secret != nil {
					addSecret(s.Secret.Name)
				}
			}
		}
	}
	return cms, secrets
}

// DeleteProjectedChartInspector removes the chart-inspector set projected onto a spoke (the reverse
// of ProjectChartInspector). Call it only once the last remote composition on the spoke is gone
// (the caller ref-counts). Idempotent — a NotFound on any object is ignored. The referenced
// ConfigMap shares the chart-inspector name, so it is removed with the namespaced set.
func DeleteProjectedChartInspector(ctx context.Context, spoke client.Client, coords ChartInspectorCoords) error {
	// core-provider#423: the RBAC kinds of the set are recorded (rbac.delete); the Service,
	// Deployment and ConfigMap are not in AUDIT-4's scope.
	rbacGVR := map[string]schema.GroupVersionResource{
		"ClusterRole": clusterRoleGVR, "ClusterRoleBinding": clusterRoleBindingGVR, "ServiceAccount": serviceAccountGVR,
	}
	del := func(obj client.Object, what string) error {
		err := spoke.Delete(ctx, obj)
		if gvr, ok := rbacGVR[what]; ok {
			auditrec.RecordDeleteSent(ctx, auditrec.ActionRBACDelete, auditrec.Target{GVR: gvr, Kind: what, Namespace: obj.GetNamespace(), Name: obj.GetName(), Namespaces: auditrec.LabelsFromClient(spoke)}, string(obj.GetUID()), err)
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting chart-inspector %s: %w", what, err)
		}
		return nil
	}

	// cluster-scoped RBAC first (found by the SA it binds).
	if crb, err := findChartInspectorCRB(ctx, spoke, coords); err != nil {
		return err
	} else if crb != nil {
		cr := &rbacv1.ClusterRole{ObjectMeta: metav1.ObjectMeta{Name: crb.RoleRef.Name}}
		if err := del(cr, "ClusterRole"); err != nil {
			return err
		}
		b := &rbacv1.ClusterRoleBinding{ObjectMeta: metav1.ObjectMeta{Name: crb.Name}}
		if err := del(b, "ClusterRoleBinding"); err != nil {
			return err
		}
	}

	nn := metav1.ObjectMeta{Name: coords.Name, Namespace: coords.Namespace}
	for _, o := range []struct {
		obj  client.Object
		what string
	}{
		{&corev1.Service{ObjectMeta: nn}, "Service"},
		{&appsv1.Deployment{ObjectMeta: nn}, "Deployment"},
		{&corev1.ConfigMap{ObjectMeta: nn}, "ConfigMap"},
		{&corev1.ServiceAccount{ObjectMeta: nn}, "ServiceAccount"},
	} {
		if err := del(o.obj, o.what); err != nil {
			return err
		}
	}
	return nil
}

// findChartInspectorCRB returns the ClusterRoleBinding whose subjects include the chart-inspector
// ServiceAccount, or nil when the chart-inspector needs no cluster RBAC.
func findChartInspectorCRB(ctx context.Context, mgmt client.Client, coords ChartInspectorCoords) (*rbacv1.ClusterRoleBinding, error) {
	var list rbacv1.ClusterRoleBindingList
	if err := mgmt.List(ctx, &list); err != nil {
		return nil, fmt.Errorf("listing ClusterRoleBindings on hub: %w", err)
	}
	for i := range list.Items {
		for _, s := range list.Items[i].Subjects {
			if s.Kind == "ServiceAccount" && s.Name == coords.Name && s.Namespace == coords.Namespace {
				return &list.Items[i], nil
			}
		}
	}
	return nil, nil
}
