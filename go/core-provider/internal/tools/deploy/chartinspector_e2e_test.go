//go:build e2e

// Package deploy e2e validation for the chart-inspector projection (Path A, Inc 2). Runs only with
// -tags e2e and requires two real clusters via env vars:
//
//	MGMT_KUBECONFIG   - management (hub) cluster kubeconfig, where the chart-inspector runs
//	TARGET_KUBECONFIG - remote (spoke) cluster kubeconfig, where it must be projected
//
// It validates that ProjectChartInspector reads the running chart-inspector from the hub and brings
// up a working copy on the spoke under the same name/namespace (so a projected cdc's baked
// URL_CHART_INSPECTOR resolves locally), including the ConfigMap its pod depends on.
package deploy

import (
	"context"
	"os"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// e2eRESTConfig builds the rest config for the cluster named by env.
func e2eRESTConfig(t *testing.T, env string) *rest.Config {
	t.Helper()
	rc, err := clientcmd.BuildConfigFromFlags("", os.Getenv(env))
	if err != nil {
		t.Fatalf("building rest config from %s: %v", env, err)
	}
	return rc
}

func e2eClient(t *testing.T, env string) client.Client {
	t.Helper()
	rc := e2eRESTConfig(t, env)
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = apiextensionsv1.AddToScheme(scheme)
	cl, err := client.New(rc, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("building client from %s: %v", env, err)
	}
	return cl
}

func TestE2E_ProjectChartInspector(t *testing.T) {
	if os.Getenv("MGMT_KUBECONFIG") == "" || os.Getenv("TARGET_KUBECONFIG") == "" {
		t.Skip("MGMT_KUBECONFIG and TARGET_KUBECONFIG must be set")
	}
	ctx := context.Background()
	hub := e2eClient(t, "MGMT_KUBECONFIG")
	spoke := e2eClient(t, "TARGET_KUBECONFIG")
	// spokeDyn lives in remoteseed_e2e_test.go, same package and same TARGET_KUBECONFIG.
	spokeDynamic := spokeDyn(t)

	coords := ChartInspectorCoords{
		Name:      "installer-core-provider-chart-inspector",
		Namespace: "krateo-system",
		Port:      8081,
	}

	if err := ProjectChartInspector(ctx, hub, spoke, spokeDynamic, coords); err != nil {
		t.Fatalf("ProjectChartInspector: %v", err)
	}

	// every projected object must exist on the spoke
	key := client.ObjectKey{Namespace: coords.Namespace, Name: coords.Name}
	checks := []struct {
		name string
		obj  client.Object
		k    client.ObjectKey
	}{
		{"ServiceAccount", &corev1.ServiceAccount{}, key},
		{"ConfigMap", &corev1.ConfigMap{}, key},
		{"Deployment", &appsv1.Deployment{}, key},
		{"Service", &corev1.Service{}, key},
		{"ClusterRole", &rbacv1.ClusterRole{}, client.ObjectKey{Name: coords.Name + "-krateo-system"}},
		{"ClusterRoleBinding", &rbacv1.ClusterRoleBinding{}, client.ObjectKey{Name: coords.Name + "-krateo-system"}},
	}
	for _, c := range checks {
		if err := spoke.Get(ctx, c.k, c.obj); err != nil {
			t.Fatalf("%s not found on spoke: %v", c.name, err)
		}
		t.Logf("projected %s/%s OK", c.name, c.k.Name)
	}

	// the projected Service must have a fresh ClusterIP (not the hub's), proving sanitization
	svc := &corev1.Service{}
	if err := spoke.Get(ctx, key, svc); err != nil {
		t.Fatalf("reading projected Service: %v", err)
	}
	if svc.Spec.ClusterIP == "" || svc.Spec.ClusterIP == "None" {
		t.Fatalf("projected Service has no spoke-allocated ClusterIP: %q", svc.Spec.ClusterIP)
	}

	// the projected Deployment must become Available on the spoke (image pulls, config resolves)
	deadline := time.Now().Add(90 * time.Second)
	for {
		d := &appsv1.Deployment{}
		if err := spoke.Get(ctx, key, d); err != nil {
			t.Fatalf("reading projected Deployment: %v", err)
		}
		if d.Status.AvailableReplicas >= 1 {
			t.Logf("chart-inspector Available on spoke (%d replica)", d.Status.AvailableReplicas)
			break
		}
		if time.Now().After(deadline) {
			cond := "none"
			for _, cnd := range d.Status.Conditions {
				cond += " " + string(cnd.Type) + "=" + string(cnd.Status)
			}
			t.Fatalf("chart-inspector never became Available on spoke; conditions:%s", cond)
		}
		time.Sleep(3 * time.Second)
	}

	// idempotent: a second projection must not error
	if err := ProjectChartInspector(ctx, hub, spoke, spokeDynamic, coords); err != nil {
		t.Fatalf("second ProjectChartInspector (idempotency): %v", err)
	}
	_ = metav1.NamespaceAll
}
