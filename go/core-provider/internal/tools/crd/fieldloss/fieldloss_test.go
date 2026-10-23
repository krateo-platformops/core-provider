package fieldloss

import (
	"encoding/json"
	"reflect"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func b(v bool) *bool { return &v }

// versionSchema builds a root CRD version schema whose spec declares props.
func versionSchema(props map[string]apiextensionsv1.JSONSchemaProps, preserveSpec bool) *apiextensionsv1.JSONSchemaProps {
	spec := apiextensionsv1.JSONSchemaProps{Type: "object", Properties: props}
	if preserveSpec {
		spec = apiextensionsv1.JSONSchemaProps{Type: "object", XPreserveUnknownFields: b(true)}
	}
	return &apiextensionsv1.JSONSchemaProps{
		Type: "object",
		Properties: map[string]apiextensionsv1.JSONSchemaProps{
			"apiVersion": {Type: "string"},
			"kind":       {Type: "string"},
			"metadata":   {Type: "object"},
			"spec":       spec,
			"status":     {Type: "object", XPreserveUnknownFields: b(true)},
		},
	}
}

func TestPredict(t *testing.T) {
	narrow := versionSchema(map[string]apiextensionsv1.JSONSchemaProps{
		"replicas": {Type: "integer"},
	}, false)

	wideValue := func() map[string]any {
		return map[string]any{
			"apiVersion": "composition.krateo.io/v1-0-0",
			"kind":       "Widget",
			"metadata":   map[string]any{"name": "w1", "labels": map[string]any{"a": "b"}},
			"spec": map[string]any{
				"replicas": int64(3),
				"ingress":  map[string]any{"host": "app.example.com", "tls": true},
			},
		}
	}

	t.Run("drops undeclared spec fields and reports their values", func(t *testing.T) {
		st, _ := Structural(narrow)
		obj := wideValue()

		rep, err := Predict(obj, st)
		if err != nil {
			t.Fatalf("Predict: %v", err)
		}
		if rep.Empty() {
			t.Fatal("expected a loss to be predicted")
		}

		want := []string{"spec.ingress"}
		if !reflect.DeepEqual(rep.Paths, want) {
			t.Errorf("paths = %v, want %v", rep.Paths, want)
		}

		// The values must come back, not just the names — that is what makes the loss
		// recoverable by hand rather than merely announced.
		got, _ := rep.JSON()
		var back map[string]any
		if err := json.Unmarshal([]byte(got), &back); err != nil {
			t.Fatalf("JSON: %v", err)
		}
		spec, _ := back["spec"].(map[string]any)
		ing, _ := spec["ingress"].(map[string]any)
		if ing["host"] != "app.example.com" || ing["tls"] != true {
			t.Errorf("removed subtree lost its values: %s", got)
		}

		// Predict must not mutate its input: the caller writes this object next.
		if !reflect.DeepEqual(obj, wideValue()) {
			t.Error("Predict mutated the object it was given")
		}
	})

	t.Run("declared fields are never reported", func(t *testing.T) {
		st, _ := Structural(narrow)
		rep, err := Predict(map[string]any{
			"apiVersion": "composition.krateo.io/v1-0-0",
			"kind":       "Widget",
			"metadata":   map[string]any{"name": "w1"},
			"spec":       map[string]any{"replicas": int64(3)},
		}, st)
		if err != nil {
			t.Fatalf("Predict: %v", err)
		}
		if !rep.Empty() {
			t.Errorf("expected no loss, got paths=%v removed=%v", rep.Paths, rep.Removed)
		}
	})

	t.Run("metadata is never reported as lost", func(t *testing.T) {
		// The pruner skips ObjectMeta at the resource root. If that ever changed, the migration
		// would report its own label write as a loss on every single instance, which would make
		// the feature worse than useless.
		st, _ := Structural(narrow)
		rep, err := Predict(map[string]any{
			"apiVersion": "composition.krateo.io/v1-0-0",
			"kind":       "Widget",
			"metadata": map[string]any{
				"name":        "w1",
				"labels":      map[string]any{"krateo.io/composition-version": "v1-0-0"},
				"annotations": map[string]any{"anything": "at all"},
			},
			"spec": map[string]any{"replicas": int64(3)},
		}, st)
		if err != nil {
			t.Fatalf("Predict: %v", err)
		}
		if !rep.Empty() {
			t.Errorf("metadata reported as pruned: paths=%v", rep.Paths)
		}
	})

	t.Run("a preserve-unknown spec loses nothing", func(t *testing.T) {
		// This is the shape of the "vacuum" storage version, and of option 4 in #188. It is the
		// control arm: if this ever reports a loss, the detector is wrong, not the schema.
		st, _ := Structural(versionSchema(nil, true))
		rep, err := Predict(wideValue(), st)
		if err != nil {
			t.Fatalf("Predict: %v", err)
		}
		if !rep.Empty() {
			t.Errorf("preserve-unknown spec reported a loss: paths=%v", rep.Paths)
		}
	})

	t.Run("nested and array losses are reported with values", func(t *testing.T) {
		st, _ := Structural(versionSchema(map[string]apiextensionsv1.JSONSchemaProps{
			"ports": {Type: "array", Items: &apiextensionsv1.JSONSchemaPropsOrArray{
				Schema: &apiextensionsv1.JSONSchemaProps{
					Type:       "object",
					Properties: map[string]apiextensionsv1.JSONSchemaProps{"port": {Type: "integer"}},
				},
			}},
		}, false))

		rep, err := Predict(map[string]any{
			"apiVersion": "composition.krateo.io/v1-0-0",
			"kind":       "Widget",
			"metadata":   map[string]any{"name": "w1"},
			"spec": map[string]any{
				"ports": []any{
					map[string]any{"port": int64(80)},
					map[string]any{"port": int64(443), "name": "https"},
				},
			},
		}, st)
		if err != nil {
			t.Fatalf("Predict: %v", err)
		}
		if rep.Empty() {
			t.Fatal("expected the undeclared array-element field to be reported")
		}
		js, _ := rep.JSON()
		if !jsonContains(t, js, "https") {
			t.Errorf("array element's dropped value missing from the report: %s", js)
		}
	})

	t.Run("nil schema is an error, not a silent pass", func(t *testing.T) {
		if _, err := Predict(map[string]any{"spec": map[string]any{"x": 1}}, nil); err == nil {
			t.Error("expected an error for a nil target schema")
		}
		if _, err := Structural(nil); err == nil {
			t.Error("expected an error for a nil version schema")
		}
	})
}

func jsonContains(t *testing.T, js, want string) bool {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(js), &v); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	return len(js) > 0 && contains(js, want)
}

func contains(h, n string) bool {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return true
		}
	}
	return false
}
