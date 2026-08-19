//go:build e2e

// Package deploy e2e validation for the remote-seed teardown (Path A, Inc 3). Runs only with
// -tags e2e and requires two real clusters:
//
//	MGMT_KUBECONFIG   - management (hub) cluster kubeconfig, where the chart-inspector runs
//	TARGET_KUBECONFIG - remote (spoke) cluster kubeconfig, where the seed is projected + torn down
//
// It validates the ref-counted teardown: with two remote compositions seeded on the spoke, tearing
// down the first removes only its shadow CompositionDefinition (the shared chart-inspector stays);
// tearing down the second — the last — removes the chart-inspector set and the compositiondefinitions
// CRD too.
package deploy

import (
	"context"
	"os"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func spokeDyn(t *testing.T) dynamic.Interface {
	t.Helper()
	rc, err := clientcmd.BuildConfigFromFlags("", os.Getenv("TARGET_KUBECONFIG"))
	if err != nil {
		t.Fatalf("spoke rest config: %v", err)
	}
	d, err := dynamic.NewForConfig(rc)
	if err != nil {
		t.Fatalf("spoke dynamic client: %v", err)
	}
	return d
}

func TestE2E_RemoteSeedTeardown(t *testing.T) {
	if os.Getenv("MGMT_KUBECONFIG") == "" || os.Getenv("TARGET_KUBECONFIG") == "" {
		t.Skip("MGMT_KUBECONFIG and TARGET_KUBECONFIG must be set")
	}
	ctx := context.Background()
	hub := e2eClient(t, "MGMT_KUBECONFIG")
	spoke := e2eClient(t, "TARGET_KUBECONFIG")
	dyn := spokeDyn(t)

	coords := ChartInspectorCoords{Name: "installer-core-provider-chart-inspector", Namespace: "krateo-system", Port: 8081}
	chart := &compositiondefinitionsv1alpha1.ChartInfo{Url: "oci://ghcr.io/example/krateo/demo", Version: "1.0.0"}

	// --- setup: project chart-inspector + seed two shadow CDs in different namespaces ---
	if err := ProjectChartInspector(ctx, hub, spoke, coords); err != nil {
		t.Fatalf("ProjectChartInspector: %v", err)
	}
	seed := func(ns, name string) {
		if err := SeedRemoteTarget(ctx, spoke, dyn, RemoteSeedOptions{
			Namespace: ns, CDName: name, Chart: chart,
			PackageURL: "oci://ghcr.io/example/krateo/demo:1.0.0",
			APIVersion: "composition.krateo.io/v1-0-0", Kind: "Demo", Resource: "demos",
		}); err != nil {
			t.Fatalf("SeedRemoteTarget %s/%s: %v", ns, name, err)
		}
	}
	seed("team-a", "app-a")
	seed("team-b", "app-b")

	depKey := client.ObjectKey{Namespace: coords.Namespace, Name: coords.Name}
	assertCI := func(wantExists bool) {
		t.Helper()
		err := spoke.Get(ctx, depKey, &appsv1.Deployment{})
		if wantExists && err != nil {
			t.Fatalf("chart-inspector Deployment expected present: %v", err)
		}
		if !wantExists && !apierrors.IsNotFound(err) {
			t.Fatalf("chart-inspector Deployment expected gone, got err=%v", err)
		}
	}

	// --- teardown #1 (team-a/app-a): shadow CD gone, chart-inspector STAYS (app-b still there) ---
	if err := TeardownRemoteSeed(ctx, spoke, dyn, RemoteSeedTeardownOptions{Namespace: "team-a", CDName: "app-a", ChartInspector: coords}); err != nil {
		t.Fatalf("teardown app-a: %v", err)
	}
	if _, err := dyn.Resource(compositionDefinitionGVR).Namespace("team-a").Get(ctx, "app-a", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("shadow CD app-a expected gone, err=%v", err)
	}
	assertCI(true)
	t.Log("teardown #1: app-a shadow CD removed, chart-inspector retained (ref-count > 0)")

	// --- teardown #2 (team-b/app-b): last one -> chart-inspector + CD CRD removed ---
	if err := TeardownRemoteSeed(ctx, spoke, dyn, RemoteSeedTeardownOptions{Namespace: "team-b", CDName: "app-b", ChartInspector: coords}); err != nil {
		t.Fatalf("teardown app-b: %v", err)
	}
	assertCI(false)
	// the compositiondefinitions CRD should be gone (best-effort: allow brief finalization)
	crdName := compositionDefinitionGVR.Resource + "." + compositionDefinitionGVR.Group
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := spoke.Get(ctx, client.ObjectKey{Name: crdName}, &apiextensionsv1.CustomResourceDefinition{})
		if apierrors.IsNotFound(err) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("compositiondefinitions CRD expected gone; last err=%v", err)
		}
		time.Sleep(2 * time.Second)
	}
	t.Log("teardown #2: last remote composition removed -> chart-inspector + CD CRD gone")

	// idempotent re-run (CRD already gone) must not error
	if err := TeardownRemoteSeed(ctx, spoke, dyn, RemoteSeedTeardownOptions{Namespace: "team-b", CDName: "app-b", ChartInspector: coords}); err != nil {
		t.Fatalf("idempotent teardown: %v", err)
	}
}
