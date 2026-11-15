package metrics

import (
	"context"
	"testing"

	"github.com/krateo-platformops/unstructured-runtime/pkg/logging"
	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const gvrKey = attribute.Key("krateo.composition.gvr")

// docs/design/cdc-pooling.md step 3: two controllers in one process record distinctly attributed
// streams. Each controller's view rides in ctx (NewContext), and the helm timers read it
// (FromContext), so a helm install is attributed to the controller whose pass ran it. The process
// identity (service.instance.id) stays on every point.
func TestViews_HelmTimersRecordTheControllerInCtx(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	root, err := newMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"), logging.NewNopLogger(), "fireworksapps-v1-0-0-controller")
	if err != nil {
		t.Fatal(err)
	}
	a := NewContext(context.Background(), root.With(gvrKey.String("composition.krateo.io/v1-0-0/fireworksapps")))
	b := NewContext(context.Background(), root.With(gvrKey.String("composition.krateo.io/v0-3-0/portals")))
	NewHelmMetrics(a).RecordInstall(0.1, nil)
	NewHelmMetrics(a).RecordInstall(0.1, nil)
	NewHelmMetrics(b).RecordInstall(0.1, nil)

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	by := map[string]int64{}
	instance := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "composition_helm_install_total" {
				continue
			}
			for _, p := range m.Data.(metricdata.Sum[int64]).DataPoints {
				g, _ := p.Attributes.Value(gvrKey)
				id, _ := p.Attributes.Value("service.instance.id")
				by[g.AsString()] += p.Value
				instance[id.AsString()] = true
			}
		}
	}
	if by["composition.krateo.io/v1-0-0/fireworksapps"] != 2 || by["composition.krateo.io/v0-3-0/portals"] != 1 {
		t.Fatalf("helm installs by %s = %v, want fireworksapps 2, portals 1", gvrKey, by)
	}
	if !instance["fireworksapps-v1-0-0-controller"] || len(instance) != 1 {
		t.Fatalf("every point must keep the process identity, got service.instance.id values %v", instance)
	}
}

// A ctx without a view records as the process (the global instance), and nil is safe throughout.
func TestViews_FromContextFallsBackAndNilIsSafe(t *testing.T) {
	if FromContext(context.Background()) != GetInstance() {
		t.Fatal("a ctx carrying no view must return the process instance")
	}
	var nilM *Metrics
	if nilM.With(gvrKey.String("x")) != nil {
		t.Fatal("With on nil must return nil")
	}
	if ctx := NewContext(context.Background(), nil); FromContext(ctx) != GetInstance() {
		t.Fatal("a nil view must leave ctx unchanged")
	}
	NewHelmMetrics(context.Background()).RecordInstall(0.1, nil) // metrics off: a no-op
}

// The attribute is a contract with observability (obs-reconcile-perf, the audit-to-reconcile join):
// pin the key and the value format, which matches the resource attribute krateo.io/composition-gvr.
func TestControllerAttr_IsTheContract(t *testing.T) {
	a := ControllerAttr("composition.krateo.io", "v1-0-0", "fireworksapps")
	if a.Key != "krateo.composition.gvr" || a.Value.AsString() != "composition.krateo.io/v1-0-0/fireworksapps" {
		t.Fatalf("ControllerAttr = %s=%q, want krateo.composition.gvr=composition.krateo.io/v1-0-0/fireworksapps", a.Key, a.Value.AsString())
	}
}
