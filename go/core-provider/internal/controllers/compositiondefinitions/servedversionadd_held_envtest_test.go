//go:build envtest

package compositiondefinitions

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	crdclient "github.com/krateo-platformops/core-provider/internal/tools/crd"
	crdutils "github.com/krateo-platformops/core-provider/internal/tools/crd/generation"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// bigSchema is a values schema whose generated CRD version is roughly targetBytes: the krateo-057
// installers size (~600 KB a version), where two versions fit etcd's 1.5 MiB request and three do not.
func bigSchema(targetBytes int) []byte {
	var b strings.Builder
	b.WriteString(`{"type":"object","properties":{`)
	for i := 0; b.Len() < targetBytes; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `"field%06d":{"type":"string","description":"%s"}`, i, strings.Repeat("d", 80))
	}
	b.WriteString(`}}`)
	return []byte(b.String())
}

// heldAddFixture builds a CRD serving v1-0-0 (the definition's own version, one instance) and v2-0-0,
// each generated from schemaOf, with a deployed helm revision declaring the Kind at v2-0-0 (which
// holds it, #486). It returns the external, the definition, the generated v3-0-0 CRD and the CRD name.
func heldAddFixture(t *testing.T, ctx context.Context, cl client.Client, dyn dynamic.Interface, schemaOf func(string) []byte) (*external, *compositiondefinitionsv1alpha1.CompositionDefinition, *apiextensionsv1.CustomResourceDefinition, string, string) {
	t.Helper()
	kind := fmt.Sprintf("Held%d", time.Now().UnixNano()%1000000)
	gen := func(v, field string) *apiextensionsv1.CustomResourceDefinition {
		t.Helper()
		c, err := crdutils.GenerateCRD(schemaOf(field), schema.GroupVersionKind{Group: compositionGroup, Version: v, Kind: kind})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	v1, v2, v3 := gen("v1-0-0", "foo"), gen("v2-0-0", "bar"), gen("v3-0-0", "baz")
	t.Cleanup(func() {
		_ = cl.Delete(context.Background(), &apiextensionsv1.CustomResourceDefinition{ObjectMeta: metav1.ObjectMeta{Name: v1.Name}})
	})
	gvrV1, err := crdclient.ApplyOrUpdateCRD(ctx, cl, dyn, v1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crdclient.ApplyOrUpdateCRD(ctx, cl, dyn, v2); err != nil {
		t.Fatal(err)
	}
	waitEstablished(t, cl, v1.Name)
	createInstance(t, ctx, dyn, schema.GroupVersionKind{Group: compositionGroup, Version: "v1-0-0", Kind: kind}, gvrV1, "on-v1", "v1-0-0")
	release := fmt.Sprintf("parent-%d", time.Now().UnixNano()%100000)
	storedHelmRelease(t, ctx, cl, "default", release, 3, "deployed",
		"apiVersion: "+compositionGroup+"/v2-0-0\nkind: "+kind+"\nmetadata:\n  name: child\n")
	return newExternal(cl, dyn), definitionOn("v1-0-0", kind), v3, v1.Name, release
}

// #494, on a real apiserver. v2-0-0 is held by a deployed helm revision, so the add of v3-0-0 cannot
// drop it, and three ~560 KB versions do not fit etcd's request limit. The add is REFUSED before any
// write: no generation change, and the error names the held version and the release that holds it.
// Update and Create stop on it as a success. Once the holder lets go, v2-0-0 takes the ordinary path
// (its grace, then the drop in the add's own write), so the refusal is not a dead end.
func TestEnvtest_AddRefusedWhenAHeldVersionWouldNotFit(t *testing.T) {
	cl, dyn := startAddEnv(t)
	ctx := context.Background()
	e, cr, crdV3, crdName, release := heldAddFixture(t, ctx, cl, dyn, func(string) []byte { return bigSchema(560000) })
	gvkV3 := schema.GroupVersionKind{Group: compositionGroup, Version: "v3-0-0", Kind: cr.Status.Kind}

	_, gen0 := servedOf(t, cl, crdName)
	_, err := e.applyCompositionCRD(ctx, cr, gvkV3, crdV3.DeepCopy())
	r, ok := asServedVersionAddTooLarge(err)
	if !ok {
		t.Fatalf("the add returned %v, want the too-large refusal (v2-0-0 is held and three versions do not fit)", err)
	}
	if len(r.held) != 1 || r.held[0].Version != "v2-0-0" || !strings.Contains(r.held[0].Reason, release) {
		t.Errorf("held = %+v, want v2-0-0 held by the release %s", r.held, release)
	}
	if r.bytes <= servedVersionAddBudget || !strings.Contains(r.Error(), release) {
		t.Errorf("refusal %d bytes (budget %d), message %q: want over budget and naming %s", r.bytes, servedVersionAddBudget, r.Error(), release)
	}
	if served, gen := servedOf(t, cl, crdName); fmt.Sprint(served) != "[v1-0-0 v2-0-0]" || gen != gen0 {
		t.Fatalf("a refused add wrote the CRD: served=%v generation %d -> %d", served, gen0, gen)
	}
	if stop, out := settleAddWait(logging.NewNopLogger(), err); !stop || out != nil {
		t.Errorf("settleAddWait(refusal) = %v, %v; want a successful stop", stop, out)
	}

	// The holder lets go: v2-0-0 is now only vacated, so the add waits out its grace and then drops it
	// in the same write.
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: fmt.Sprintf("sh.helm.release.v1.%s.v3", release)}}
	if err := cl.Delete(ctx, sec); err != nil {
		t.Fatalf("removing the holding release: %v", err)
	}
	if _, err := e.applyCompositionCRD(ctx, cr, gvkV3, crdV3.DeepCopy()); func() bool { _, ok := asServedVersionAddWaits(err); return !ok }() {
		t.Fatalf("after the release is gone the add returned %v, want the grace wait", err)
	}
	cr.Status.ServedVersionsPendingPrune[0].EligibleSince = metav1.NewTime(time.Now().Add(-servedVersionPruneGrace - time.Second))
	if _, err := e.applyCompositionCRD(ctx, cr, gvkV3, crdV3.DeepCopy()); err != nil {
		t.Fatalf("the add after the grace failed: %v", err)
	}
	if served, _ := servedOf(t, cl, crdName); fmt.Sprint(served) != "[v1-0-0 v3-0-0]" {
		t.Errorf("served = %v, want [v1-0-0 v3-0-0]", served)
	}
}

// The size gate's control: the same held version with SMALL schemas. Three small versions fit, so the
// add goes ahead and keeps v2-0-0 beside it. A parent/child bump, where the parent's release holds the
// child's old version until it re-renders, must not be refused.
func TestEnvtest_AddKeepsAHeldVersionWhenTheWriteFits(t *testing.T) {
	cl, dyn := startAddEnv(t)
	ctx := context.Background()
	e, cr, crdV3, crdName, _ := heldAddFixture(t, ctx, cl, dyn, specSchema)
	gvkV3 := schema.GroupVersionKind{Group: compositionGroup, Version: "v3-0-0", Kind: cr.Status.Kind}
	if _, err := e.applyCompositionCRD(ctx, cr, gvkV3, crdV3.DeepCopy()); err != nil {
		t.Fatalf("a small add with a held version returned %v, want it written", err)
	}
	if served, _ := servedOf(t, cl, crdName); fmt.Sprint(served) != "[v1-0-0 v2-0-0 v3-0-0]" {
		t.Errorf("served = %v, want [v1-0-0 v2-0-0 v3-0-0]: v2-0-0 held, v3-0-0 added", served)
	}
}
