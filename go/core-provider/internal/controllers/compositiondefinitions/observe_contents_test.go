package compositiondefinitions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// contentsChartTGZ is a chart "contents" at version with one template file.
func contentsChartTGZ(t *testing.T, version, template string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"contents/Chart.yaml":         "apiVersion: v2\nname: contents\nversion: " + version + "\n",
		"contents/values.yaml":        "{}\n",
		"contents/values.schema.json": `{"type":"object"}`,
		"contents/templates/t.yaml":   template,
	} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const twoConfigMaps = "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: a\n---\napiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: b\n"

// observeContents runs the real Observe on a definition whose chart is served at url (a 404 when
// tgz is nil), starting from status.contents prev.
func observeContents(t *testing.T, tgz []byte, version string, prev *compositiondefinitionsv1alpha1.ChartContents, deleting bool) *compositiondefinitionsv1alpha1.CompositionDefinition {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if tgz == nil {
			http.NotFound(w, nil)
			return
		}
		_, _ = w.Write(tgz)
	}))
	t.Cleanup(srv.Close)
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	cd.SetName("contents")
	cd.SetNamespace("krateo-system")
	cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: srv.URL + "/contents-" + version + ".tgz", Version: version}
	cd.Status.Contents = prev
	if deleting {
		now := metav1.Now()
		cd.SetDeletionTimestamp(&now)
		cd.SetFinalizers([]string{"finalizer.managedresource.krateo.io"})
	}
	kc := clientfake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cd).WithStatusSubresource(cd).Build()
	e := &external{
		kube: kc, mgmt: kc, apiReader: kc,
		mgmtDynamic: dynfake.NewSimpleDynamicClient(scheme(t)),
		client:      k8sfake.NewSimpleClientset(),
		log:         logging.NewNopLogger(),
		pluralizer:  notFoundPluralizer{},
	}
	_, _ = e.Observe(context.Background(), cd)
	return cd
}

func TestObserve_RecordsChartContents(t *testing.T) {
	cd := observeContents(t, contentsChartTGZ(t, "0.2.0", twoConfigMaps), "0.2.0", nil, false)
	c := cd.Status.Contents
	if c == nil {
		t.Fatal("status.contents not recorded")
	}
	if c.ChartVersion != "0.2.0" || c.Values != "default" || len(c.Kinds) != 1 || c.Kinds[0].Kind != "ConfigMap" || c.Kinds[0].Count != 2 {
		t.Errorf("contents = %+v, want chart 0.2.0, ConfigMap x2", *c)
	}
}

// On a chart version change, the previous contents stay until the new chart is readable and its
// render replaces them: never cleared to empty in between.
func TestObserve_ChartContentsKeptUntilTheNewRenderLands(t *testing.T) {
	prev := &compositiondefinitionsv1alpha1.ChartContents{ChartVersion: "0.1.0", Values: "default",
		Kinds: []compositiondefinitionsv1alpha1.ChartContentsKind{{APIVersion: "v1", Kind: "Secret", Count: 1}}}
	unreadable := observeContents(t, nil, "0.2.0", prev.DeepCopy(), false)
	if c := unreadable.Status.Contents; c == nil || c.ChartVersion != "0.1.0" || len(c.Kinds) != 1 || c.Kinds[0].Kind != "Secret" {
		t.Fatalf("the new chart is not readable yet, so the previous contents must stay; got %+v", c)
	}
	landed := observeContents(t, contentsChartTGZ(t, "0.2.0", twoConfigMaps), "0.2.0", prev.DeepCopy(), false)
	if c := landed.Status.Contents; c == nil || c.ChartVersion != "0.2.0" || c.Kinds[0].Kind != "ConfigMap" {
		t.Fatalf("the new chart is readable, so its render must replace the previous one; got %+v", c)
	}
}

func TestObserve_ChartContentsNotWrittenWhileDeleting(t *testing.T) {
	cd := observeContents(t, contentsChartTGZ(t, "0.2.0", twoConfigMaps), "0.2.0", nil, true)
	if cd.Status.Contents != nil {
		t.Errorf("a definition being deleted recorded contents %+v", *cd.Status.Contents)
	}
}

// Contents never affect Ready. CONTROL arm of the comparison: a chart that renders and the same chart
// whose template cannot render at its defaults give the same Ready, while their contents differ.
func TestObserve_ChartContentsNeverAffectReady(t *testing.T) {
	ok := observeContents(t, contentsChartTGZ(t, "0.2.0", twoConfigMaps), "0.2.0", nil, false)
	bad := observeContents(t, contentsChartTGZ(t, "0.2.0", "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ required \"n\" .Values.n }}\n"), "0.2.0", nil, false)
	if bad.Status.Contents == nil || bad.Status.Contents.Unknown == "" || ok.Status.Contents == nil || ok.Status.Contents.Unknown != "" {
		t.Fatalf("premise: one render known, one unknown; got ok=%+v bad=%+v", ok.Status.Contents, bad.Status.Contents)
	}
	a, b := ok.GetCondition(rtv1.TypeReady), bad.GetCondition(rtv1.TypeReady)
	// Each arm serves its chart from its own test server, so the URL (port) in a message differs.
	urlRE := regexp.MustCompile(`http://127\.0\.0\.1:[0-9]+`)
	am, bm := urlRE.ReplaceAllString(a.Message, "URL"), urlRE.ReplaceAllString(b.Message, "URL")
	if a.Reason == "" || a.Reason == "ValuesSchemaMissing" {
		t.Fatalf("premise: Observe must get past the schema read to compare a meaningful Ready; got %s/%s %q", a.Status, a.Reason, a.Message)
	}
	if a.Status != b.Status || a.Reason != b.Reason || am != bm {
		t.Errorf("Ready differs with the contents: %s/%s %q vs %s/%s %q", a.Status, a.Reason, am, b.Status, b.Reason, bm)
	}
}
