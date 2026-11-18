package compositiondefinitions

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	rtv1 "github.com/krateo-platformops/provider-runtime/apis/common/v1"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"github.com/krateo-platformops/provider-runtime/pkg/reconciler"
	dynfake "k8s.io/client-go/dynamic/fake"
	k8sfake "k8s.io/client-go/kubernetes/fake"
	clientfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// versionedChartTGZ is a minimal chart "widget" (plural "widgets") at Chart.yaml version, with a
// valid root values.schema.json.
func versionedChartTGZ(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{
		"widget/Chart.yaml":         "apiVersion: v2\nname: widget\nversion: " + version + "\n",
		"widget/values.yaml":        "{}\n",
		"widget/values.schema.json": `{"type":"object","properties":{"size":{"type":"string"}}}`,
		"widget/templates/cm.yaml":  "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: {{ .Release.Name }}\n",
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

// observeVersionedChart runs Observe on a NEW definition of the "widget" chart at version, optionally
// in a pool. Nothing else exists: no CRD, no controller.
func observeVersionedChart(t *testing.T, version, pool string) (*compositiondefinitionsv1alpha1.CompositionDefinition, reconciler.ExternalObservation, error) {
	t.Helper()
	tgz := versionedChartTGZ(t, version)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(tgz) }))
	t.Cleanup(srv.Close)

	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	cd.SetName("widget")
	cd.SetNamespace("krateo-system")
	cd.Spec.Chart = &compositiondefinitionsv1alpha1.ChartInfo{Url: srv.URL + "/widget.tgz", Version: version}
	if pool != "" {
		cd.Spec.Controller = &compositiondefinitionsv1alpha1.ControllerConfig{Pool: &pool}
	}
	kc := clientfake.NewClientBuilder().WithScheme(scheme(t)).WithObjects(cd).WithStatusSubresource(cd).Build()
	e := &external{
		kube: kc, mgmt: kc, apiReader: kc,
		mgmtDynamic: dynfake.NewSimpleDynamicClient(scheme(t)),
		client:      k8sfake.NewSimpleClientset(),
		log:         logging.NewNopLogger(),
		pluralizer:  notFoundPluralizer{},
	}
	obs, err := e.Observe(context.Background(), cd)
	return cd, obs, err
}

// #588 against Observe: a NEW definition whose derived names do not fit is Ready=False/
// DerivedNameTooLong naming the name, with a ReadyAssessed error, and stops before the CRD-not-found
// return that hands it to Create. "widgets" + a 44-character version is 51 characters against a
// 50-character limit (the Deployment name and its selector label).
//
// CONTROL: aruba's 24-character content-addressed version (refused by the old 20-byte cap) reaches the
// CRD-not-found return, so the refusal cases cannot pass on an earlier, unrelated failure.
func TestObserveRefusesDerivedNamesThatDoNotFit(t *testing.T) {
	long := "0.0.0-" + strings.Repeat("a", 38)

	t.Run("a dedicated controller's name over 63: refused before any write", func(t *testing.T) {
		cd, obs, err := observeVersionedChart(t, long, "")
		got := cd.GetCondition(rtv1.TypeReady)
		if !cdreason.DerivedNameTooLong.Is(got) {
			t.Fatalf("Ready = %s/%s (%q), want False/DerivedNameTooLong (Observe err: %v)", got.Status, got.Reason, got.Message, err)
		}
		if want := "widgets-v0-0-0-" + strings.Repeat("a", 38) + "-controller"; !strings.Contains(got.Message, want) || !strings.Contains(got.Message, "(64 characters)") {
			t.Errorf("Ready message %q does not name %q and its length", got.Message, want)
		}
		if err == nil || !reconciler.IsReadyAssessed(err) {
			t.Errorf("Observe err = %v, want a ReadyAssessed error", err)
		}
		if obs.ResourceExists {
			t.Errorf("Observe says the resource exists")
		}
	})

	t.Run("the same chart in a pool: the dedicated names do not apply", func(t *testing.T) {
		cd, _, err := observeVersionedChart(t, long, "pages")
		if got := cd.GetCondition(rtv1.TypeReady); cdreason.DerivedNameTooLong.Is(got) {
			t.Fatalf("a pool member was refused on a dedicated controller's name: %q (err %v)", got.Message, err)
		}
	})

	// 1.0.0-RC1 and 1.0.0+build are NOT refused: crdgen normalizes them (v1-0-0-rc1, v1-0-0-build),
	// and that normalized GVR is the one the names derive from. Asserted, because an earlier draft
	// of this check claimed they were refused, which would have turned working charts away.
	t.Run("a version crdgen normalizes is not refused", func(t *testing.T) {
		for _, version := range []string{"1.0.0-RC1", "1.0.0+build"} {
			cd, obs, err := observeVersionedChart(t, version, "")
			got := cd.GetCondition(rtv1.TypeReady)
			if err != nil || obs.ResourceExists || !strings.Contains(got.Message, "does not exists yet") {
				t.Errorf("%s: Observe = exists %v, err %v, Ready %s/%s (%q): want the CRD-not-found return", version, obs.ResourceExists, err, got.Status, got.Reason, got.Message)
			}
		}
	})

	t.Run("control: aruba's 24-character version reaches CRD-not-found", func(t *testing.T) {
		cd, obs, err := observeVersionedChart(t, "0.0.0-probe-35fc2441c975", "")
		got := cd.GetCondition(rtv1.TypeReady)
		if err != nil || obs.ResourceExists || !strings.Contains(got.Message, "does not exists yet") {
			t.Fatalf("Observe = exists %v, err %v, Ready %s/%s (%q): the control did not reach the CRD-not-found return, so the cases above prove nothing",
				obs.ResourceExists, err, got.Status, got.Reason, got.Message)
		}
	})
}
