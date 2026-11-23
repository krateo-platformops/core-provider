//go:build envtest

package composition

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gobuffalo/flect"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	mapper "github.com/krateo-platformops/composition-dynamic-controller/internal/tools/dynamic"
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	helmconfig "github.com/krateo-platformops/plumbing/helm"
	helmv3 "github.com/krateo-platformops/plumbing/helm/v3"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/plumbing/kubeutil/eventrecorder"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// pluralizer419 is the composition package's FakePluralizer, which lives behind the integration tag.
type pluralizer419 struct{}

func (pluralizer419) GVKtoGVR(gvk schema.GroupVersionKind) (schema.GroupVersionResource, error) {
	return schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: flect.Pluralize(strings.ToLower(gvk.Kind))}, nil
}

var (
	cdGVR419   = schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"}
	fireGVR419 = schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v1-1-10", Resource: "fireworksapps"}
)

// serve419Chart serves a one-ConfigMap chart, so a release this composition's name can be installed.
func serve419Chart(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"Chart.yaml":        "apiVersion: v2\nname: fireworks419\nversion: 1.1.10\n",
		"templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-cm\ndata:\n  k: v\n",
	} {
		if err := tw.WriteHeader(&tar.Header{Name: "fireworks419/" + name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	_ = tw.Close()
	_ = gz.Close()
	tgz := buf.Bytes()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) }))
	t.Cleanup(srv.Close)
	return srv.URL + "/fireworks419-1.1.10.tgz"
}

type env419 struct {
	ctx  context.Context
	cfg  *rest.Config
	dyn  dynamic.Interface
	kube kubernetes.Interface
	h    *handler
}

func start419(t *testing.T) *env419 {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS not set; run via setup-envtest")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	env := &envtest.Environment{CRDDirectoryPaths: []string{"../../testdata/crds/core"}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("start envtest: %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	pig, err := archive.Dynamic(cfg, pluralizer419{})
	if err != nil {
		t.Fatal(err)
	}
	rec, err := eventrecorder.Create(ctx, cfg, "test-419", nil)
	if err != nil {
		t.Fatal(err)
	}
	mp, err := mapper.NewRESTMapper(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// chart-inspector answers with no resources: the genuine-delete control reaches the RBAC
	// regeneration fallback (no owner-labelled RBAC exists here), which must not fail the teardown.
	ci := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]string{})
	}))
	t.Cleanup(ci.Close)
	h := NewHandler(&HandlerOptions{
		Kubeconfig: cfg, PackageInfoGetter: pig, EventRecorder: *event.NewAPIRecorder(rec),
		Pluralizer: pluralizer419{}, ChartInspectorUrl: ci.URL, SaName: "test-sa", SaNamespace: "default",
		SafeReleaseName: true, Mapper: mp,
	}).(*handler)
	return &env419{ctx: ctx, cfg: cfg, dyn: dynamic.NewForConfigOrDie(cfg), kube: kubernetes.NewForConfigOrDie(cfg), h: h}
}

func (e *env419) namespace(t *testing.T, ns string) {
	t.Helper()
	if _, err := e.kube.CoreV1().Namespaces().Create(e.ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
}

// definition creates a CompositionDefinition with NO status: the status subresource is never
// written, which is the state of a definition deleted before core-provider's first status write.
func (e *env419) definition(t *testing.T, ns, name, chartVersion string) {
	t.Helper()
	cd := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec":     map[string]any{"chart": map[string]any{"url": "oci://registry.example/charts/fireworks-app", "version": chartVersion}},
	}}
	got, err := e.dyn.Resource(cdGVR419).Namespace(ns).Create(e.ctx, cd, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedString(got.Object, "status", "apiVersion"); found {
		t.Fatalf("premise: definition %s/%s must have no status.apiVersion", ns, name)
	}
}

func (e *env419) composition(t *testing.T, ns, name, release string, labels, annotations map[string]string) *unstructured.Unstructured {
	t.Helper()
	l := map[string]any{compositionMeta.ReleaseNameLabel: release, compositionMeta.CompositionVersionLabel: fireGVR419.Version}
	for k, v := range labels {
		l[k] = v
	}
	a := map[string]any{}
	for k, v := range annotations {
		a[k] = v
	}
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/" + fireGVR419.Version, "kind": "FireworksApp",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": l, "annotations": a},
		// The fixture CRD's only required field (spec.git.toRepo.name).
		"spec": map[string]any{"git": map[string]any{"toRepo": map[string]any{"name": "repo-419"}}},
	}}
	got, err := e.dyn.Resource(fireGVR419).Namespace(ns).Create(e.ctx, u, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func (e *env419) install(t *testing.T, ns, release string) {
	t.Helper()
	hc, err := helmv3.NewClient(e.cfg, helmv3.WithNamespace(ns))
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	if _, err := hc.Install(e.ctx, release, serve419Chart(t), &helmconfig.InstallConfig{ActionConfig: &helmconfig.ActionConfig{}, Namespace: ns}); err != nil {
		t.Fatalf("install %s: %v", release, err)
	}
}

func (e *env419) releaseExists(t *testing.T, ns, release string) bool {
	t.Helper()
	hc, err := helmv3.NewClient(e.cfg, helmv3.WithNamespace(ns))
	if err != nil {
		t.Fatal(err)
	}
	defer hc.Close()
	rel, err := hc.GetRelease(e.ctx, release, &helmconfig.GetConfig{})
	if err != nil {
		t.Fatalf("get release %s: %v", release, err)
	}
	return rel != nil
}

func definitionLabels(ns, name string) map[string]string {
	return map[string]string{
		compositionMeta.CompositionDefinitionGroupLabel:     cdGVR419.Group,
		compositionMeta.CompositionDefinitionVersionLabel:   cdGVR419.Version,
		compositionMeta.CompositionDefinitionResourceLabel:  cdGVR419.Resource,
		compositionMeta.CompositionDefinitionNameLabel:      name,
		compositionMeta.CompositionDefinitionNamespaceLabel: ns,
	}
}

// #419, deterministically. The incident's shape: a definition deleted before core-provider wrote its
// status, so the cdc's Create never ran (Get requires status), so its composition carries NO
// definition-ref labels and has NO release. With more than one definition in the cluster, Locate's
// label-less search skips every definition without status and fails ("too many definitions"). Delete
// must not need the definition at all when nothing is installed: it returns nil (the finalizer is
// released) and installs nothing. CONTROL: the same composition, the same two definitions, Locate
// called directly, fails, which proves the fixture really is the one that deadlocked.
func TestEnvtest_419_DeleteOfANeverInstalledCompositionNeedsNoDefinition(t *testing.T) {
	e := start419(t)
	ns := "nsdelete-419"
	e.namespace(t, ns)
	e.definition(t, ns, "fireworks-app", "1.1.10")
	e.definition(t, ns, "fireworks-app-2", "1.1.10")
	comp := e.composition(t, ns, "probe", "probe-419", nil, nil)

	if _, err := e.h.packageInfoGetter.Locate(comp); err == nil {
		t.Fatal("control: Locate found a definition for an unlabelled composition among two definitions with no status; the fixture does not reproduce #419")
	}
	if e.releaseExists(t, ns, "probe-419") {
		t.Fatal("premise: nothing is installed for this composition")
	}
	if err := e.h.Delete(e.ctx, comp); err != nil {
		t.Fatalf("Delete of a composition with nothing installed failed, so its finalizer is never released and the namespace deadlocks (#419): %v", err)
	}
}

// The teardown rule holds where something IS installed: the same unlocatable definition, but a
// release exists. The handover decision cannot be made without the definition, so Delete must
// return an error (the finalizer stays) and must NOT uninstall.
func TestEnvtest_419_DeleteRefusesWhenAReleaseExistsAndTheDefinitionCannotBeLocated(t *testing.T) {
	e := start419(t)
	ns := "nsdelete-419-rel"
	e.namespace(t, ns)
	e.definition(t, ns, "fireworks-app", "1.1.10")
	e.definition(t, ns, "fireworks-app-2", "1.1.10")
	comp := e.composition(t, ns, "probe", "probe-419", nil, nil)
	e.install(t, ns, "probe-419")

	err := e.h.Delete(e.ctx, comp)
	if err == nil || !strings.Contains(err.Error(), "getting package info") {
		t.Fatalf("Delete = %v; want the definition lookup's error, so the finalizer stays", err)
	}
	if !e.releaseExists(t, ns, "probe-419") {
		t.Fatal("Delete uninstalled a release although it could not decide whether a version migration shares it")
	}
}

// The dangerous direction with status ABSENT: a parent-rendered composition at v1-1-10 whose
// definition's spec already names 1.1.11 (a migration in flight; the definition recreated or its
// status not yet written). Locate trusts the labels, and the handover decision reads the
// definition's SPEC, so the release survives. CONTROL: the same composition with the definition's
// spec at 1.1.10 is a genuine delete and IS uninstalled, which proves the handover arm is what
// kept the release, not some failure before the uninstall.
func TestEnvtest_419_HandoverIsDetectedFromTheSpecWhenTheDefinitionHasNoStatus(t *testing.T) {
	e := start419(t)
	parentRendered := map[string]string{
		compositionMeta.AnnotationKeyHelmReleaseName: "umbrella",
		compositionMeta.AnnotationKeyOriginVersion:   fireGVR419.Version,
	}

	t.Run("handover: spec names the new version", func(t *testing.T) {
		ns := "nsdelete-419-handover"
		e.namespace(t, ns)
		e.definition(t, ns, "fireworks-app", "1.1.11")
		comp := e.composition(t, ns, "probe", "probe-419", definitionLabels(ns, "fireworks-app"), parentRendered)
		e.install(t, ns, "probe-419")
		if err := e.h.Delete(e.ctx, comp); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if !e.releaseExists(t, ns, "probe-419") {
			t.Fatal("Delete uninstalled a release a version migration still shares, because the definition had no status")
		}
	})

	t.Run("control: spec names this version, a genuine delete", func(t *testing.T) {
		ns := "nsdelete-419-genuine"
		e.namespace(t, ns)
		e.definition(t, ns, "fireworks-app", "1.1.10")
		comp := e.composition(t, ns, "probe", "probe-419", definitionLabels(ns, "fireworks-app"), parentRendered)
		e.install(t, ns, "probe-419")
		if err := e.h.Delete(e.ctx, comp); err != nil {
			t.Fatalf("Delete: %v", err)
		}
		if e.releaseExists(t, ns, "probe-419") {
			t.Fatal("control: a genuine delete left the release installed")
		}
	})
}
