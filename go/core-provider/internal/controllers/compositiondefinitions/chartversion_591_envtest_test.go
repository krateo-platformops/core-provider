//go:build envtest

package compositiondefinitions

import (
	"bytes"
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/status"
	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	"github.com/krateo-platformops/core-provider/internal/tools/chart/chartfs"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// #591 on a real apiserver (one per version): a chart whose Chart.yaml version is not plain x.y.z
// registers a CRD that serves crdgen's normalized version ("1.0.0-RC1" -> v1-0-0-rc1). Before the
// fix, status.apiVersion was written from the chart's own spelling (v1-0-0-RC1), a version the CRD
// does not serve, and everything that reads status reached for it: statusGVR (pool members,
// scale-to-zero, observeWithoutChart, remote teardown) 404ed, scale-to-zero's composition-version
// selector matched 0 instances (and for "+build" could not be built), and the controller Deployment
// key named a Deployment that does not exist.
//
// The calls are Create's own, in its order (ChartGroupVersionKind -> GenerateCRD ->
// applyCompositionCRD -> RefreshCompositionDefinitionStatus, compositiondefinitions.go Create);
// Observe derives the GVK through chartfs.GroupVersionKind, asserted to agree. 1.0.0 is the control:
// it passed before the fix too, so a failing non-plain row is about the version, not the harness.
//
// 1.0.0--rc is valid semver (a prerelease identifier may contain '-') and the one shape whose old
// spelling, v1-0-0--rc, is itself a VALID object name and CRD version, while crdgen serves
// v1-0-0-rc (it collapses runs). Every other old spelling here is refused by the apiserver as a name,
// so only this row can show a wrongly-named object being written and then missed by the portal's GET.
//
// v1.0.0 (a valid Helm chart version) is the one population whose names MOVE: served vv1-0-0 before
// the fix, v1-0-0 after. It was consistent before and must be consistent after.
func TestEnvtest_591_StatusNamesServedVersion(t *testing.T) {
	ctx := context.Background()
	for _, version := range []string{"1.0.0", "1.0.0-RC1", "1.0.0+build.5", "2.0.0-Beta.2", "1.0.0--rc", "v1.0.0"} {
		t.Run(version, func(t *testing.T) {
			cl, dyn := startAddEnv(t)
			pkg, err := chartfs.FromReader(bytes.NewReader(versionedChartTGZ(t, version)), "test://widget-"+version+".tgz")
			if err != nil {
				t.Fatal(err)
			}
			gvk, err := chart.ChartGroupVersionKind(pkg.FS(), pkg.RootDir())
			if err != nil {
				t.Fatal(err)
			}
			if obsGVK, err := chartfs.GroupVersionKind(pkg); err != nil || obsGVK != gvk {
				t.Fatalf("Observe's chartfs.GroupVersionKind = %v (err %v), Create's chart.ChartGroupVersionKind = %v: the two derivations disagree", obsGVK, err, gvk)
			}
			sch, err := chart.ChartJsonSchema(pkg.FS(), pkg.RootDir())
			if err != nil {
				t.Fatal(err)
			}
			crd, err := crdutils.GenerateCRD(sch, gvk)
			if err != nil {
				t.Fatalf("GenerateCRD: %v", err)
			}
			// Not "default": the object templates fall back to "default" for an empty namespace, which
			// would hide a write that dropped the CD's namespace.
			cr := &compositiondefinitionsv1alpha1.CompositionDefinition{ObjectMeta: metav1.ObjectMeta{Name: "widget", Namespace: "team-591"}}
			cr.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: "test://widget.tgz", Version: version}
			gvr, err := newExternal(cl, dyn).applyCompositionCRD(ctx, cr, gvk, crd.DeepCopy())
			if err != nil {
				t.Fatalf("applyCompositionCRD: %v", err)
			}
			servedList, _ := servedOf(t, cl, crd.Name)
			served := map[string]bool{}
			for _, v := range servedList {
				served[v] = true
			}
			if !served[gvr.Version] {
				t.Fatalf("the CRD does not serve the gvr apply returned (%s); served %v", gvr.Version, servedList)
			}
			if err := status.RefreshCompositionDefinitionStatus(cr, crd, gvr, gvk, "test://widget.tgz"); err != nil {
				t.Fatal(err)
			}

			// The root fact: status.apiVersion names the served version.
			if got := cr.Status.CurrentGVK().Version; got != gvr.Version || !served[got] {
				t.Fatalf("status.apiVersion = %q, the CRD serves %v (gvr %s): status names a version the CRD does not serve", cr.Status.ApiVersion, servedList, gvr.Version)
			}

			// What reads it. One instance on the served version, labelled as the MutatingAdmissionPolicy
			// stamps it (request.requestKind.version).
			createInstance(t, ctx, dyn, schema.GroupVersionKind{Group: gvr.Group, Version: gvr.Version, Kind: crd.Spec.Names.Kind}, gvr, "w1", gvr.Version)
			sg, ok := statusGVR(cr)
			if !ok || sg != gvr {
				t.Fatalf("statusGVR = %v (ok %v), want the served %v", sg, ok, gvr)
			}
			if _, err := dyn.Resource(sg).Namespace("default").List(ctx, metav1.ListOptions{}); err != nil {
				t.Fatalf("List on statusGVR %s: %v", sg, err)
			}
			sel, err := labels.ValidatedSelectorFromSet(labels.Set{deploy.CompositionVersionLabel: cr.Status.CurrentGVK().Version})
			if err != nil {
				t.Fatalf("scale-to-zero's composition-version selector cannot be built from status: %v", err)
			}
			l, err := dyn.Resource(gvr).Namespace("default").List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
			if err != nil || len(l.Items) != 1 {
				n := -1
				if l != nil {
					n = len(l.Items)
				}
				t.Fatalf("composition-version=%s selects %d of 1 instance (err %v)", cr.Status.CurrentGVK().Version, n, err)
			}
			if a, b := deploy.ControllerDeploymentKey("ns", sg), deploy.ControllerDeploymentKey("ns", gvr); a != b {
				t.Fatalf("Deployment key from status = %v, from the served gvr = %v", a, b)
			}

			assertVersionKeyedObjectsMatchStatus(t, ctx, cl, dyn, cr, gvr, sch)
		})
	}
}

// assertVersionKeyedObjectsMatchStatus deploys the definition's controller objects from the SHIPPED
// chart's assets and checks every object keyed on the version against the definition's status, the
// way the portal reads it (frontend, #591): the create and edit forms GET
// `<status.resource>-<version segment of status.apiVersion>-jsonschema-configmap` in the CD's
// namespace. The names are computed here from status.resource and status.apiVersion only, never from
// core-provider's helpers, so a disagreement between what is written and what status says fails.
//
// The DeployOptions are Create's (compositiondefinitions.go Create, deploy.DeployOptions{...}) for
// the fields that name objects: GVR is the gvr applyCompositionCRD returned, Namespace the CD's,
// ControllerNamespace the management namespace, Spec the CD's chart. Deploy itself runs unmodified;
// the Deployment is marked Available here because envtest runs no controller-manager.
func assertVersionKeyedObjectsMatchStatus(t *testing.T, ctx context.Context, cl client.Client, dyn dynamic.Interface, cr *compositiondefinitionsv1alpha1.CompositionDefinition, gvr schema.GroupVersionResource, schemaBytes []byte) {
	t.Helper()
	const ctrlNS = "krateo-system"
	for _, ns := range []string{ctrlNS, cr.Namespace} {
		if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
			t.Fatal(err)
		}
	}
	root := t.TempDir()
	if _, err := renderChartAssets(root, ctrlNS, "ghcr.io/krateo-platformops/composition-dynamic-controller:591-test"); err != nil {
		t.Fatal(err)
	}
	opts := deploy.DeployOptions{
		RBACFolderPath:         filepath.Join(root, "cdc-rbac"),
		KubeClient:             cl,
		Namespace:              cr.Namespace,
		ControllerNamespace:    ctrlNS,
		GVR:                    gvr,
		Spec:                   cr.Spec.Chart.DeepCopy(),
		DeploymentTemplatePath: filepath.Join(root, "cdc-deployment", filepath.Base(CDCtemplateDeploymentPath)),
		ConfigmapTemplatePath:  filepath.Join(root, "cdc-configmap", filepath.Base(CDCtemplateConfigmapPath)),
		JsonSchemaTemplatePath: filepath.Join(root, "json-schema-configmap", filepath.Base(JSONSchemaTemplateConfigmapPath)),
		ServiceTemplatePath:    filepath.Join(root, "cdc-service", filepath.Base(ServiceTemplatePath)),
		JsonSchemaBytes:        schemaBytes,
		DynClient:              dyn,
		HubDynClient:           dyn,
	}

	// The portal's derivation, from status alone.
	apiVersion := cr.Status.ApiVersion
	seg := apiVersion[strings.LastIndex(apiVersion, "/")+1:]
	prefix := cr.Status.Resource + "-" + seg
	schemaCM := types.NamespacedName{Namespace: cr.Namespace, Name: prefix + "-jsonschema-configmap"}
	controller := types.NamespacedName{Namespace: ctrlNS, Name: prefix + "-controller"}
	lease := seg + "." + cr.Status.Resource + "." + gvr.Group

	stop := make(chan struct{})
	defer close(stop)
	go markAvailable(ctx, cl, ctrlNS, stop)
	dctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	if _, err := deploy.Deploy(dctx, cl, opts); err != nil {
		t.Fatalf("Deploy: %v", err)
	}

	// The portal's GET, exactly that name.
	var cm corev1.ConfigMap
	if err := cl.Get(ctx, schemaCM, &cm); err != nil {
		t.Errorf("portal GET %s (from status.apiVersion %q): %v", schemaCM, apiVersion, err)
	} else if !strings.Contains(strings.Join(mapValues(cm.Data), ""), `"size"`) {
		t.Errorf("%s does not carry the chart's schema: %v", schemaCM, cm.Data)
	}
	var dep appsv1.Deployment
	if err := cl.Get(ctx, controller, &dep); err != nil {
		t.Errorf("GET Deployment %s: %v", controller, err)
	}
	var ccm corev1.ConfigMap
	if err := cl.Get(ctx, types.NamespacedName{Namespace: ctrlNS, Name: prefix + "-configmap"}, &ccm); err != nil {
		t.Errorf("GET controller ConfigMap %s-configmap: %v", prefix, err)
	} else if got := ccm.Data["COMPOSITION_CONTROLLER_LEASE_NAME"]; got != lease {
		t.Errorf("COMPOSITION_CONTROLLER_LEASE_NAME = %q, want %q (from status)", got, lease)
	}

	// Every object Deploy wrote for this Kind, by kind: each must carry status's version segment.
	// Logged in full, so the names are on record for the pre-fix comparison.
	names := writtenNames(t, ctx, cl, cr.Status.Resource)
	if len(names) < 8 {
		t.Fatalf("found only %d objects named after %s: %v (the instrument did not see Deploy's writes)", len(names), cr.Status.Resource, names)
	}
	for _, n := range names {
		if !strings.Contains(n, prefix+"-") && !strings.Contains(n, prefix+".") && !strings.HasSuffix(n, prefix) {
			t.Errorf("%s is not named with status's version %q (prefix %q)", n, seg, prefix)
		}
	}
	// The Lease Role grants exactly the Lease named from status.
	foundLease := false
	var roles rbacv1.RoleList
	if err := cl.List(ctx, &roles, client.InNamespace(ctrlNS)); err != nil {
		t.Fatal(err)
	}
	for _, r := range roles.Items {
		for _, rule := range r.Rules {
			for _, rn := range rule.ResourceNames {
				if rn == lease {
					foundLease = true
				}
			}
		}
	}
	if !foundLease {
		t.Errorf("no Role in %s grants the Lease %q", ctrlNS, lease)
	}
	t.Logf("status.apiVersion=%s portal schema ConfigMap=%s lease=%s; %d objects: %v", apiVersion, schemaCM, lease, len(names), names)
}

func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// writtenNames lists every object of the kinds Deploy writes whose name mentions resource, as
// "Kind ns/name".
func writtenNames(t *testing.T, ctx context.Context, cl client.Client, resource string) []string {
	t.Helper()
	var out []string
	add := func(kind string, objs []client.Object) {
		for _, o := range objs {
			if strings.Contains(o.GetName(), resource) {
				out = append(out, kind+" "+o.GetNamespace()+"/"+o.GetName())
			}
		}
	}
	lists := []struct {
		kind string
		l    client.ObjectList
		get  func(client.ObjectList) []client.Object
	}{
		{"Deployment", &appsv1.DeploymentList{}, func(l client.ObjectList) []client.Object { return ptrs(l.(*appsv1.DeploymentList).Items) }},
		{"ConfigMap", &corev1.ConfigMapList{}, func(l client.ObjectList) []client.Object { return ptrs(l.(*corev1.ConfigMapList).Items) }},
		{"Service", &corev1.ServiceList{}, func(l client.ObjectList) []client.Object { return ptrs(l.(*corev1.ServiceList).Items) }},
		{"ServiceAccount", &corev1.ServiceAccountList{}, func(l client.ObjectList) []client.Object { return ptrs(l.(*corev1.ServiceAccountList).Items) }},
		{"Role", &rbacv1.RoleList{}, func(l client.ObjectList) []client.Object { return ptrs(l.(*rbacv1.RoleList).Items) }},
		{"RoleBinding", &rbacv1.RoleBindingList{}, func(l client.ObjectList) []client.Object { return ptrs(l.(*rbacv1.RoleBindingList).Items) }},
		{"ClusterRole", &rbacv1.ClusterRoleList{}, func(l client.ObjectList) []client.Object { return ptrs(l.(*rbacv1.ClusterRoleList).Items) }},
		{"ClusterRoleBinding", &rbacv1.ClusterRoleBindingList{}, func(l client.ObjectList) []client.Object { return ptrs(l.(*rbacv1.ClusterRoleBindingList).Items) }},
	}
	for _, x := range lists {
		if err := cl.List(ctx, x.l); err != nil {
			t.Fatalf("list %s: %v", x.kind, err)
		}
		add(x.kind, x.get(x.l))
	}
	sort.Strings(out)
	return out
}

func ptrs[T any, P interface {
	*T
	client.Object
}](items []T) []client.Object {
	out := make([]client.Object, len(items))
	for i := range items {
		out[i] = P(&items[i])
	}
	return out
}

// markAvailable stands in for the Deployment controller envtest does not run: every Deployment in ns
// is marked with every replica ready and Available, which is what Deploy waits for. By namespace, not
// by name, so a wrongly-named Deployment fails the name assertions below rather than a timeout.
func markAvailable(ctx context.Context, cl client.Client, ns string, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case <-time.After(200 * time.Millisecond):
		}
		var list appsv1.DeploymentList
		if err := cl.List(ctx, &list, client.InNamespace(ns)); err != nil {
			continue
		}
		for i := range list.Items {
			markOneAvailable(ctx, cl, &list.Items[i])
		}
	}
}

func markOneAvailable(ctx context.Context, cl client.Client, d *appsv1.Deployment) {
	r := int32(1)
	if d.Spec.Replicas != nil {
		r = *d.Spec.Replicas
	}
	d.Status.Replicas, d.Status.ReadyReplicas, d.Status.AvailableReplicas, d.Status.UpdatedReplicas = r, r, r, r
	d.Status.ObservedGeneration = d.Generation
	d.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentAvailable, Status: corev1.ConditionTrue, Reason: "Test", LastUpdateTime: metav1.Now(), LastTransitionTime: metav1.Now()}}
	_ = cl.Status().Update(ctx, d)
}
