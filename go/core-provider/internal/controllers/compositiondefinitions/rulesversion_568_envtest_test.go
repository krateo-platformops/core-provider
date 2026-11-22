//go:build envtest

package compositiondefinitions

// core-provider#568 child 8 (stability across RULES versions): for the SAME chart version, a
// re-rendered inferred schema reaches a served CRD version only through #534. A render that is
// provably looser is written; anything else keeps the served schema and is reported as
// SpecSchemaCurrent=False/GeneratedSchemaNotLooser.
//
// HOW A RULES-V2 RENDER IS PRODUCED. plumbing valuesschema has one rules version ("1"). The served
// schema is the real rules-v1 output of the production seam (chart.SpecSchema). Each "v2" render is
// those same bytes with ONE edit of the kind a rules change would make at a leaf, applied to the
// inferred JSON (the library is not forked). Every render then goes through the production
// generator (generation.GenerateCRD), the production decision (specDriftDecision) and the
// production write (crd.ApplyOrUpdateCRD -> ProjectWrite), against a real apiserver, and the served
// spec schema is read back.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/krateo-platformops/core-provider/internal/tools/chart"
	crdtools "github.com/krateo-platformops/core-provider/internal/tools/crd"
	"github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

const rules568Values = `replicas: 2
enabled: true
image:
  tag: v1.2.3
`

// rulesV2 returns the rules-v1 inferred bytes with edit applied to the named top-level property.
func rulesV2(t *testing.T, v1 []byte, prop string, edit func(node map[string]any)) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(v1, &root); err != nil {
		t.Fatal(err)
	}
	props, _ := root["properties"].(map[string]any)
	node, ok := props[prop].(map[string]any)
	if !ok {
		t.Fatalf("premise: rules v1 infers %q as a schema node", prop)
	}
	edit(node)
	b, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func servedSpec568(t *testing.T, cl client.Client, name, version string) apiextensionsv1.JSONSchemaProps {
	t.Helper()
	var c apiextensionsv1.CustomResourceDefinition
	if err := cl.Get(context.Background(), client.ObjectKey{Name: name}, &c); err != nil {
		t.Fatal(err)
	}
	for _, v := range c.Spec.Versions {
		if v.Name == version {
			return v.Schema.OpenAPIV3Schema.Properties["spec"]
		}
	}
	t.Fatalf("CRD %s serves no %s", name, version)
	return apiextensionsv1.JSONSchemaProps{}
}

func TestEnvtest_568_RulesVersionChangeGoesThrough534(t *testing.T) {
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	dyn := dynamic.NewForConfigOrDie(cfg)
	ctx := context.Background()

	r, err := chart.SpecSchema(fstest.MapFS{
		"c/Chart.yaml":  {Data: []byte("apiVersion: v2\nname: rules568\nversion: 1.0.0\n")},
		"c/values.yaml": {Data: []byte(rules568Values)},
	}, "c", true)
	if err != nil || r.Source != chart.SchemaInferred {
		t.Fatalf("premise: an inferred schema; source=%v err=%v", r.Source, err)
	}
	v1 := r.Bytes

	// setup serves the rules-v1 render under its own Kind, so every subtest decides against a fresh
	// served schema and none depends on another's write.
	setup := func(t *testing.T, kind string) (schema.GroupVersionKind, string) {
		t.Helper()
		gvk := schema.GroupVersionKind{Group: "composition.krateo.io", Version: "v1-0-0", Kind: kind}
		served, err := generation.GenerateCRD(v1, gvk)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := crdtools.ApplyOrUpdateCRD(ctx, cl, dyn, served.DeepCopy()); err != nil {
			t.Fatal(err)
		}
		gvr := schema.GroupVersionResource{Group: gvk.Group, Version: gvk.Version, Resource: served.Spec.Names.Plural}
		if err := wait.PollUntilContextTimeout(ctx, 200*time.Millisecond, 20*time.Second, true, func(ctx context.Context) (bool, error) {
			_, err := dyn.Resource(gvr).Namespace("default").List(ctx, metav1.ListOptions{})
			return err == nil, nil
		}); err != nil {
			t.Fatal("CRD never served")
		}
		return gvk, served.Name
	}

	// decide serves rules v1 under kind, renders v2, decides against what the apiserver SERVES, then
	// performs the production write and returns the decision, the served spec schema before and after,
	// and the rendered spec schema.
	decide := func(t *testing.T, kind string, v2 []byte) (reason, message string, refresh bool, before, after, rendered apiextensionsv1.JSONSchemaProps) {
		t.Helper()
		gvk, name := setup(t, kind)
		gen, err := generation.GenerateCRD(v2, gvk)
		if err != nil {
			t.Fatal(err)
		}
		rendered = gen.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
		var live apiextensionsv1.CustomResourceDefinition
		if err := cl.Get(ctx, client.ObjectKey{Name: name}, &live); err != nil {
			t.Fatal(err)
		}
		cond, refresh, _, err := specDriftDecision(&live, gen)
		if err != nil {
			t.Fatal(err)
		}
		before = servedSpec568(t, cl, name, gvk.Version)
		if _, err := crdtools.ApplyOrUpdateCRD(ctx, cl, dyn, gen.DeepCopy()); err != nil {
			t.Fatal(err)
		}
		after = servedSpec568(t, cl, name, gvk.Version)
		return string(cond.Reason), cond.Message, refresh, before, after, rendered
	}
	leaf := func(s apiextensionsv1.JSONSchemaProps, k string) apiextensionsv1.JSONSchemaProps {
		return s.Properties[k]
	}

	t.Run("control: the same rules re-rendered is Current and writes nothing", func(t *testing.T) {
		reason, _, refresh, before, after, _ := decide(t, "Rules568Same", v1)
		if reason != "Current" || refresh {
			t.Fatalf("an identical render: reason=%s refresh=%v, want Current, false", reason, refresh)
		}
		if mustJSON(t, before) != mustJSON(t, after) {
			t.Fatal("an identical render changed the served schema")
		}
	})

	t.Run("a looser rules-v2 render (a boolean leaf now also admits null) is written", func(t *testing.T) {
		v2 := rulesV2(t, v1, "enabled", func(n map[string]any) { n["type"] = []any{"boolean", "null"} })
		reason, _, refresh, before, after, _ := decide(t, "Rules568Nullable", v2)
		if reason != "LooserSchemaPending" || !refresh {
			t.Fatalf("reason=%s refresh=%v, want LooserSchemaPending, true", reason, refresh)
		}
		if leaf(before, "enabled").Nullable || !leaf(after, "enabled").Nullable {
			t.Fatalf("served enabled.nullable before=%v after=%v, want false then true (the looser render written)",
				leaf(before, "enabled").Nullable, leaf(after, "enabled").Nullable)
		}
	})

	t.Run("a tighter rules-v2 render (an int-or-string leaf now string-only) is reported and not written", func(t *testing.T) {
		v2 := rulesV2(t, v1, "replicas", func(n map[string]any) {
			delete(n, "x-kubernetes-int-or-string")
			n["type"] = "string"
			n["default"] = "2"
		})
		reason, message, refresh, before, after, _ := decide(t, "Rules568Tighter", v2)
		if reason != "GeneratedSchemaNotLooser" || refresh {
			t.Fatalf("reason=%s refresh=%v, want GeneratedSchemaNotLooser, false", reason, refresh)
		}
		if !strings.Contains(message, ".replicas") {
			t.Errorf("the message must name the path that is not looser; got %q", message)
		}
		if mustJSON(t, before) != mustJSON(t, after) {
			t.Fatal("a render that is not provably looser changed the served schema")
		}
		if !leaf(after, "replicas").XIntOrString {
			t.Error("served replicas must still be int-or-string")
		}
	})

	// MEASURED: a rules change that turns an int-or-string leaf into "any value" does not reach a
	// served version, because #534's allowlist lets x-kubernetes-int-or-string be ADDED, never
	// removed. Since plumbing v1.105.0 (core-provider#613) crdgen KEEPS an untyped leaf's default
	// ({default: 2, x-kubernetes-preserve-unknown-fields: true}); before it, the default was dropped
	// too, and the message reported "replicas.default: removed" as a second difference. Now the
	// int-or-string removal is the only one. Recorded so a rules v2 that wants this knows it reaches
	// only new chart versions.
	t.Run("an int-or-string leaf loosened to any value is not recognised as looser, and stays frozen", func(t *testing.T) {
		v2 := rulesV2(t, v1, "replicas", func(n map[string]any) { delete(n, "x-kubernetes-int-or-string") })
		reason, message, refresh, before, after, rendered := decide(t, "Rules568AnyValue", v2)
		t.Logf("served replicas: %s | rendered replicas: %s | decision %s: %s", mustJSON(t, leaf(before, "replicas")), mustJSON(t, leaf(rendered, "replicas")), reason, message)
		if reason != "GeneratedSchemaNotLooser" || refresh {
			t.Fatalf("reason=%s refresh=%v, want GeneratedSchemaNotLooser, false (measured)", reason, refresh)
		}
		if r := leaf(rendered, "replicas"); r.Default == nil || string(r.Default.Raw) != "2" || r.XPreserveUnknownFields == nil {
			t.Fatalf("premise: crdgen renders the untyped leaf as preserve-unknown KEEPING its default 2 (plumbing v1.105.0, #613); got %s", mustJSON(t, r))
		}
		if !strings.Contains(message, "spec.properties.replicas.x-kubernetes-int-or-string: removed") {
			t.Errorf("the message must report the int-or-string removal; got %q", message)
		}
		if strings.Contains(message, "replicas.default") {
			t.Errorf("the default is kept since plumbing v1.105.0, so the message must not report it changed; got %q", message)
		}
		if strings.Contains(message, "enabled") {
			t.Errorf("the message reports a path this render did not change (subtests must not share state): %q", message)
		}
		if mustJSON(t, before) != mustJSON(t, after) {
			t.Fatal("the served schema changed")
		}
	})
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
