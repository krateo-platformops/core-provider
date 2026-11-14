//go:build integration
// +build integration

package composition

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/krateo-platformops/audit"
	"github.com/krateo-platformops/audit/audittest"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/archive"
	mapper "github.com/krateo-platformops/composition-dynamic-controller/internal/tools/dynamic"
	compositionMeta "github.com/krateo-platformops/composition-dynamic-controller/pkg/meta"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
	"github.com/krateo-platformops/plumbing/kubeutil/eventrecorder"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/repo"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/e2e-framework/klient/decoder"
	"sigs.k8s.io/e2e-framework/pkg/envconf"
	"sigs.k8s.io/e2e-framework/pkg/features"
)

// core-provider#423, the AUTHORIZATION-LABEL arm (audit v0.13.0: authorization-bearing annotations
// AND labels are content). A child write that changes only an authorization label must be recorded;
// one that changes only an ordinary label is metadata_only and must not be. Both through the real
// path: a chart served from this test renders a ConfigMap whose labels come from the composition's
// values, and the cdc's own install, Observe and Update drive it on kind.
//
// The control is what makes the arm mean something. Without it, "the change was recorded" is also
// what a recorder that records EVERY label change would produce; the plain-label change must
// produce nothing, and its write must be seen to land, or the silence proves nothing.
func TestAuthorizationLabelOnlyChangeIsAudited(t *testing.T) {
	const (
		alNS        = "authlabel-system"
		alKind      = "AuthLabel"
		alResource  = "authlabels"
		alVersion   = "v0-1-0"
		alRelease   = "authlabel-1"
		alCM        = "authlabel-1-cm"
		principalLb = "authn.krateo.io/principal"
		plainLb     = "example.krateo.io/plain"
	)
	f := features.New("AuthorizationLabel").Assess("an authorization-label-only change is recorded; a plain-label-only change is not", func(ctx context.Context, t *testing.T, cfg *envconf.Config) context.Context {
		admin := cfg.Client().RESTConfig()
		dyn := dynamic.NewForConfigOrDie(admin)
		kube := kubernetes.NewForConfigOrDie(admin)
		ignoreExists := func(err error, what string) {
			t.Helper()
			if err != nil && !apierrors.IsAlreadyExists(err) {
				t.Fatalf("%s: %v", what, err)
			}
		}

		_, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: alNS}}, metav1.CreateOptions{})
		ignoreExists(err, "namespace")

		// The CompositionDefinition CRD, as the suite's Setup applies it (this test may run alone).
		var cdCRD unstructured.Unstructured
		if err := decoder.DecodeFile(os.DirFS(filepath.Join(testdataPath, "crds", "core")), "core.krateo.io_compositiondefinitions.yaml", &cdCRD); err != nil {
			t.Fatal(err)
		}
		crdGVR := schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
		_, err = dyn.Resource(crdGVR).Create(ctx, &cdCRD, metav1.CreateOptions{})
		ignoreExists(err, "CompositionDefinition CRD")

		// 1. A chart repository served from this test: one ConfigMap, labels from values.
		chartDir, repoDir := t.TempDir(), t.TempDir()
		files := map[string]string{
			"Chart.yaml":  "apiVersion: v2\nname: authlabel\nversion: 0.1.0\n",
			"values.yaml": "principal: alice\nplain: one\n",
			"templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}-cm\n" +
				"  labels:\n    " + principalLb + ": {{ .Values.principal | quote }}\n    " + plainLb + ": {{ .Values.plain | quote }}\n" +
				"data:\n  fixed: \"unchanged\"\n",
		}
		for name, body := range files {
			p := filepath.Join(chartDir, name)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		chartSrv := httptest.NewServer(http.FileServer(http.Dir(repoDir)))
		t.Cleanup(chartSrv.Close)
		ch, err := loader.LoadDir(chartDir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := chartutil.Save(ch, repoDir); err != nil {
			t.Fatal(err)
		}
		idx, err := repo.IndexDirectory(repoDir, chartSrv.URL)
		if err != nil {
			t.Fatal(err)
		}
		if err := idx.WriteFile(filepath.Join(repoDir, "index.yaml"), 0o644); err != nil {
			t.Fatal(err)
		}

		// 2. chart-inspector: Observe derives the composition's RBAC from it, so it must answer.
		ciMux := http.NewServeMux()
		ciMux.HandleFunc("/resources", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]string{{"group": "", "version": "v1", "resource": "configmaps", "name": alCM, "namespace": alNS}})
		})
		ciSrv := httptest.NewServer(ciMux)
		t.Cleanup(ciSrv.Close)

		// 3. The composition kind's CRD (open schema; the generated one is core-provider's job).
		compCRD := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "apiextensions.k8s.io/v1", "kind": "CustomResourceDefinition",
			"metadata": map[string]any{"name": alResource + ".composition.krateo.io"},
			"spec": map[string]any{
				"group": "composition.krateo.io", "scope": "Namespaced",
				"names": map[string]any{"kind": alKind, "plural": alResource, "singular": "authlabel", "listKind": alKind + "List"},
				"versions": []any{map[string]any{
					"name": alVersion, "served": true, "storage": true,
					"subresources": map[string]any{"status": map[string]any{}},
					"schema": map[string]any{"openAPIV3Schema": map[string]any{
						"type": "object", "x-kubernetes-preserve-unknown-fields": true}},
				}},
			}}}
		_, err = dyn.Resource(crdGVR).Create(ctx, compCRD, metav1.CreateOptions{})
		ignoreExists(err, "composition CRD")
		compGVR := schema.GroupVersionResource{Group: "composition.krateo.io", Version: alVersion, Resource: alResource}
		waitFor(t, 60*time.Second, "the composition CRD to serve", func() bool {
			_, err := dyn.Resource(compGVR).Namespace(alNS).List(ctx, metav1.ListOptions{})
			return err == nil
		})

		// 4. The CompositionDefinition, pointing at the served chart.
		cdGVR := schema.GroupVersionResource{Group: "core.krateo.io", Version: "v1alpha1", Resource: "compositiondefinitions"}
		cd := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "core.krateo.io/v1alpha1", "kind": "CompositionDefinition",
			"metadata": map[string]any{"name": "authlabel", "namespace": alNS},
			"spec":     map[string]any{"chart": map[string]any{"url": chartSrv.URL, "repo": "authlabel", "version": "0.1.0"}},
		}}
		waitFor(t, 60*time.Second, "the CompositionDefinition CRD to serve", func() bool {
			_, err := dyn.Resource(cdGVR).Namespace(alNS).Create(ctx, cd, metav1.CreateOptions{})
			return err == nil || apierrors.IsAlreadyExists(err)
		})
		if _, err := dyn.Resource(cdGVR).Namespace(alNS).Patch(ctx, "authlabel", types.MergePatchType,
			[]byte(`{"status":{"apiVersion":"composition.krateo.io/`+alVersion+`","kind":"`+alKind+`"}}`), metav1.PatchOptions{}, "status"); err != nil {
			t.Fatalf("CompositionDefinition status: %v", err)
		}

		// 5. The cdc's handler for this kind, recording into an in-memory audit sink.
		sink := audittest.NewRecorder(t)
		rec, err := eventrecorder.Create(ctx, admin, "test-authlabel", nil)
		if err != nil {
			t.Fatal(err)
		}
		mp, err := mapper.NewRESTMapper(admin)
		if err != nil {
			t.Fatal(err)
		}
		pig, err := archive.Dynamic(admin, FakePluralizer{})
		if err != nil {
			t.Fatal(err)
		}
		h := NewHandler(&HandlerOptions{
			Kubeconfig: admin, PackageInfoGetter: pig, EventRecorder: *event.NewAPIRecorder(rec),
			Pluralizer: FakePluralizer{}, ChartInspectorUrl: ciSrv.URL, SaName: "test-sa", SaNamespace: altNamespace,
			SafeReleaseName: true, Mapper: mp, Audit: newTestAudit(t, sink, admin, alResource),
		})

		// 6. The composition, installed.
		comp := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "composition.krateo.io/" + alVersion, "kind": alKind,
			"metadata": map[string]any{"name": "authlabel", "namespace": alNS, "labels": map[string]any{
				"krateo.io/release-name":                            alRelease,
				"krateo.io/composition-version":                     alVersion,
				compositionMeta.CompositionDefinitionGroupLabel:     "core.krateo.io",
				compositionMeta.CompositionDefinitionVersionLabel:   "v1alpha1",
				compositionMeta.CompositionDefinitionResourceLabel:  "compositiondefinitions",
				compositionMeta.CompositionDefinitionNameLabel:      "authlabel",
				compositionMeta.CompositionDefinitionNamespaceLabel: alNS,
			}},
			"spec": map[string]any{"principal": "alice", "plain": "one"},
		}}
		cli := dyn.Resource(compGVR).Namespace(alNS)
		if _, err := cli.Create(ctx, comp, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatalf("composition: %v", err)
		}
		converge := func() {
			t.Helper()
			for i := 0; i < 6; i++ {
				u, err := cli.Get(ctx, "authlabel", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				obs, err := h.Observe(ctx, u)
				if err != nil {
					t.Fatalf("observe: %v", err)
				}
				if obs.ResourceExists && obs.ResourceUpToDate {
					return
				}
				// Observe writes the object; act on the current version, as the runtime does after a
				// requeue, and treat a lost optimistic-lock race as the requeue it is.
				if u, err = cli.Get(ctx, "authlabel", metav1.GetOptions{}); err != nil {
					t.Fatal(err)
				}
				if _, err := handleObservation(t, ctx, h, obs, u); err != nil {
					if apierrors.IsConflict(err) {
						continue
					}
					t.Fatalf("handling the observation: %v", err)
				}
			}
			t.Fatal("the composition did not converge in 6 passes")
		}
		converge()
		cmCli := kube.CoreV1().ConfigMaps(alNS)
		cm, err := cmCli.Get(ctx, alCM, metav1.GetOptions{})
		if err != nil {
			t.Fatalf("precondition: the install must create %s: %v", alCM, err)
		}
		if cm.Labels[principalLb] != "alice" || cm.Labels[plainLb] != "one" {
			t.Fatalf("precondition: labels %v, want principal=alice plain=one", cm.Labels)
		}
		u, err := cli.Get(ctx, "authlabel", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		uid := u.GetUID()
		cmRows := func() []auditRow {
			var out []auditRow
			for _, r := range auditRowsFor(t, sink, uid) {
				if r.resource == "configmaps" && r.name == alCM {
					out = append(out, r)
				}
			}
			return out
		}
		setSpec := func(key, value string) {
			t.Helper()
			if _, err := cli.Patch(ctx, "authlabel", types.MergePatchType, []byte(`{"spec":{"`+key+`":"`+value+`"}}`), metav1.PatchOptions{}); err != nil {
				t.Fatalf("patching spec.%s: %v", key, err)
			}
		}

		// 7. THE ARM: only the authorization label changes. One recorded write, success, live uid.
		sink.Reset()
		setSpec("principal", "bob")
		converge()
		cm, err = cmCli.Get(ctx, alCM, metav1.GetOptions{})
		if err != nil || cm.Labels[principalLb] != "bob" {
			t.Fatalf("the principal label did not change: %v %v", err, cm.Labels)
		}
		if rows := cmRows(); len(rows) != 1 || rows[0].outcome != audit.OutcomeSuccess || rows[0].uid != string(cm.UID) ||
			(rows[0].verb != "patch" && rows[0].verb != "update") {
			t.Errorf("#423: an authorization-label-only change must be ONE recorded write (patch|update, success, uid %s), got [%s]", cm.UID, rowNames(rows))
		}

		// 8. THE CONTROL: only an ordinary label changes. The write lands, and nothing is recorded.
		sink.Reset()
		setSpec("plain", "two")
		converge()
		cm, err = cmCli.Get(ctx, alCM, metav1.GetOptions{})
		if err != nil || cm.Labels[plainLb] != "two" {
			t.Fatalf("control: the plain label did not change, so its silence would prove nothing: %v %v", err, cm.Labels)
		}
		if rows := cmRows(); len(rows) != 0 {
			t.Errorf("#423 control: a plain-label-only change is metadata_only and must record nothing, got [%s]", rowNames(rows))
		}

		// Cleanup: uninstall through the handler, then release the objects.
		if u, err := cli.Get(ctx, "authlabel", metav1.GetOptions{}); err == nil {
			_ = h.Delete(ctx, u)
			u, _ = cli.Get(ctx, "authlabel", metav1.GetOptions{})
			if u != nil {
				u.SetFinalizers(nil)
				_, _ = cli.Update(ctx, u, metav1.UpdateOptions{})
			}
			_ = cli.Delete(ctx, "authlabel", metav1.DeleteOptions{})
		}
		_ = dyn.Resource(cdGVR).Namespace(alNS).Delete(ctx, "authlabel", metav1.DeleteOptions{})
		return ctx
	}).Feature()
	testenv.Test(t, f)
}

// waitFor polls cond until it holds or timeout passes.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
