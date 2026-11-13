//go:build envtest

package deploy

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/objects"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
)

// #485 against a REAL apiserver's ConfigMap validation: a schema over 1 MiB pretty-printed (the
// size installer 0.3.635 has) is refused as the chart's file and accepted compact, and what the
// apiserver stores is the compact text. The refusal of the pretty form is the control: without it
// "accepted" could mean the fixture was simply small.
func TestJSONSchemaConfigMapOverTheLimitPrettyIsAcceptedCompactEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Fatal("KUBEBUILDER_ASSETS is not set: this test exists to run against a real apiserver, and a skip would read as a pass")
	}
	env := &envtest.Environment{}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	cl, err := client.New(cfg, client.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	const ns = "schema-485"
	if err := cl.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}); err != nil {
		t.Fatal(err)
	}
	tmpl := shippedJSONSchemaTemplate(t)
	in := prettySchema(1059606)

	// Control: the chart's file as it was stored before #485.
	pretty := corev1.ConfigMap{}
	if err := objects.CreateK8sObject(&pretty, schemaCMGVR, types.NamespacedName{Name: "pretty", Namespace: ns}, tmpl, "schema", string(in)); err != nil {
		t.Fatal(err)
	}
	err = cl.Create(ctx, &pretty)
	if err == nil || !strings.Contains(err.Error(), "Too long") {
		t.Fatalf("control: the pretty-printed %d B schema must be refused as Too long by the apiserver, got %v", len(in), err)
	}

	cm, err := renderJSONSchemaConfigMap(schemaCMGVR, types.NamespacedName{Name: "compact", Namespace: ns}, tmpl, in)
	if err != nil {
		t.Fatal(err)
	}
	if err := cl.Create(ctx, &cm); err != nil {
		t.Fatalf("the compact schema was refused: %v", err)
	}
	stored := &corev1.ConfigMap{}
	// The shipped template names the ConfigMap itself (<resource>-<version>-jsonschema-configmap), so
	// read back the object the render produced, by its own name.
	if err := cl.Get(ctx, types.NamespacedName{Name: cm.Name, Namespace: cm.Namespace}, stored); err != nil {
		t.Fatal(err)
	}
	if got, want := stored.Data["values.schema.json"], cm.Data["values.schema.json"]; got != want || strings.Contains(got, "\n") {
		t.Errorf("stored schema (%d B) differs from the compact render (%d B) or has newlines", len(got), len(want))
	}
}
