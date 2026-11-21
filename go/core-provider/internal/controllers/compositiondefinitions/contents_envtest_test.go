//go:build envtest

package compositiondefinitions

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"
)

// shippedChartFS is core-provider's own chart (helm/core-provider), a real chart, with its release
// placeholders filled as the release workflow fills them.
func shippedChartFS(t *testing.T) fstest.MapFS {
	t.Helper()
	root := filepath.Join("..", "..", "..", "..", "..", "helm", "core-provider")
	m := fstest.MapFS{}
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if rel == "Chart.yaml" {
			b = []byte(strings.NewReplacer("CHART_VERSION", "0.0.0-envtest", "APP_VERSION", "0.0.0-envtest").Replace(string(b)))
		}
		m["core-provider/"+filepath.ToSlash(rel)] = &fstest.MapFile{Data: b}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// #599 end to end through the REAL generated CRD: status.contents is neither pruned nor refused, its
// validation holds (maxItems, the values enum), and the value read back is the value written.
func TestEnvtest_ChartContentsRoundTripsTheCRD(t *testing.T) {
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	defer func() { _ = env.Stop() }()
	dyn, err := dynamic.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ri := dyn.Resource(schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"}).Namespace("default")
	ctx := context.Background()

	create := func(name string) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
			"metadata": map[string]any{"name": name, "namespace": "default"},
			"spec":     map[string]any{"chart": map[string]any{"url": "oci://ghcr.io/example/charts/demo", "version": "1.0.0"}}}}
		got, err := ri.Create(ctx, u, metav1.CreateOptions{})
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		return got
	}
	writeContents := func(u *unstructured.Unstructured, c compositiondefinitionsv1alpha1.ChartContents) (*unstructured.Unstructured, error) {
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&c)
		if err != nil {
			t.Fatal(err)
		}
		if err := unstructured.SetNestedField(u.Object, raw, "status", "contents"); err != nil {
			t.Fatal(err)
		}
		return ri.UpdateStatus(ctx, u, metav1.UpdateOptions{})
	}

	// A real chart's render, written and read back unchanged.
	c := chart.Contents(shippedChartFS(t), "core-provider")
	if c.Unknown != "" || len(c.Kinds) == 0 {
		t.Fatalf("premise: core-provider's own chart must render at its defaults; got unknown=%q kinds=%d", c.Unknown, len(c.Kinds))
	}
	got, err := writeContents(create("real-chart"), c)
	if err != nil {
		t.Fatalf("status.contents refused: %v", err)
	}
	back, found, err := unstructured.NestedMap(got.Object, "status", "contents")
	if err != nil || !found {
		t.Fatalf("status.contents was pruned (found=%v err=%v)", found, err)
	}
	var round compositiondefinitionsv1alpha1.ChartContents
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(back, &round); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(round, c) {
		t.Errorf("read back differs from written:\n%+v\n%+v", round, c)
	}
	if out := os.Getenv("CONTENTS_SAMPLE_OUT"); out != "" {
		y, err := yaml.Marshal(map[string]any{"status": map[string]any{"contents": back}})
		if err != nil {
			t.Fatal(err)
		}
		hdr := "# ENVTEST OUTPUT, core-provider#599: status.contents as the apiserver returned it, through the\n" +
			"# generated CRD, for core-provider's own chart (helm/core-provider, version placeholder 0.0.0-envtest)\n" +
			"# rendered at its default values. Not read from any cluster.\n"
		if err := os.WriteFile(out, append([]byte(hdr), y...), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// An Unknown render is accepted too (no kinds).
	if _, err := writeContents(create("unknown"), compositiondefinitionsv1alpha1.ChartContents{ChartVersion: "1.0.0", Values: "default", Unknown: "the chart does not render at its default values: x"}); err != nil {
		t.Errorf("an unknown render refused: %v", err)
	}
	// The CRD's own validation holds.
	var tooMany []compositiondefinitionsv1alpha1.ChartContentsKind
	for i := 0; i <= compositiondefinitionsv1alpha1.ChartContentsMaxKinds; i++ {
		tooMany = append(tooMany, compositiondefinitionsv1alpha1.ChartContentsKind{APIVersion: "v1", Kind: fmt.Sprintf("K%d", i), Count: 1})
	}
	if _, err := writeContents(create("too-many"), compositiondefinitionsv1alpha1.ChartContents{ChartVersion: "1.0.0", Values: "default", Kinds: tooMany}); err == nil {
		t.Errorf("%d kinds accepted; the CRD caps the list at %d", len(tooMany), compositiondefinitionsv1alpha1.ChartContentsMaxKinds)
	}
	if _, err := writeContents(create("not-default"), compositiondefinitionsv1alpha1.ChartContents{ChartVersion: "1.0.0", Values: "custom"}); err == nil {
		t.Error(`values "custom" accepted; the CRD allows only "default"`)
	}

	// The name bounds are enforced at admission, not only by the writer. POSITIVE CONTROL first: a
	// name exactly at each limit (a 253-char DNS subdomain group + "/" + a 63-char version = 317; a
	// 253-char kind) is ACCEPTED, so the cap cannot be refusing a valid Kubernetes name. Then one
	// byte past each limit is refused.
	atLimit := compositiondefinitionsv1alpha1.ChartContentsKind{APIVersion: strings.Repeat("g", 253) + "/" + strings.Repeat("v", 63), Kind: strings.Repeat("K", 253), Count: 1}
	if _, err := writeContents(create("name-at-limit"), compositiondefinitionsv1alpha1.ChartContents{ChartVersion: "1.0.0", Values: "default", Kinds: []compositiondefinitionsv1alpha1.ChartContentsKind{atLimit}}); err != nil {
		t.Errorf("control: names exactly at the limits refused: %v", err)
	}
	for name, k := range map[string]compositiondefinitionsv1alpha1.ChartContentsKind{
		"api-too-long":  {APIVersion: strings.Repeat("g", 254) + "/" + strings.Repeat("v", 63), Kind: "K", Count: 1},
		"kind-too-long": {APIVersion: "v1", Kind: strings.Repeat("K", 254), Count: 1},
	} {
		if _, err := writeContents(create(name), compositiondefinitionsv1alpha1.ChartContents{ChartVersion: "1.0.0", Values: "default", Kinds: []compositiondefinitionsv1alpha1.ChartContentsKind{k}}); err == nil {
			t.Errorf("%s: accepted; the CRD must bound apiVersion at 317 and kind at 253", name)
		}
	}
}
