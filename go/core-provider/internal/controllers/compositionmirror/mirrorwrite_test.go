package compositionmirror

import (
	"context"
	"testing"

	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

// spokeWrites counts the create and (non-status) update requests sent to the spoke's portals.
func spokeWrites(spoke *dynamicfake.FakeDynamicClient) (creates, updates *int) {
	creates, updates = new(int), new(int)
	spoke.PrependReactor("*", "portals", func(a clienttesting.Action) (bool, runtime.Object, error) {
		switch a.GetVerb() {
		case "create":
			*creates++
		case "update":
			if a.GetSubresource() == "" {
				*updates++
			}
		}
		return false, nil, nil
	})
	return creates, updates
}

// core-provider#423: mirrorDown sends a write only when the mirror differs from the hub, and says
// which one it sent. A mirror that already matches gets no request.
func TestMirrorDownWritesOnlyWhatDiffers(t *testing.T) {
	ctx := context.Background()
	hubLabels := map[string]string{"krateo.io/composition-version": "v1-0-0"}
	for _, tc := range []struct {
		name        string
		hub         *unstructured.Unstructured
		spoke       []runtime.Object
		want        mirrorWrite
		wantCreates int
		wantUpdates int
	}{
		{
			name:        "no mirror yet: created",
			hub:         withLabels(portal("p", map[string]interface{}{"replicas": int64(2)}, false), hubLabels),
			want:        mirrorCreated,
			wantCreates: 1,
		},
		{
			name: "mirror matches the hub: unchanged, no request",
			hub:  withLabels(portal("p", map[string]interface{}{"replicas": int64(2)}, false), hubLabels),
			spoke: []runtime.Object{withLabels(portal("p", map[string]interface{}{"replicas": int64(2)}, true),
				hubLabels)},
			want: mirrorUnchanged,
		},
		{
			name: "a label the spoke added is kept and is not a difference",
			hub:  withLabels(portal("p", map[string]interface{}{"replicas": int64(2)}, false), hubLabels),
			spoke: []runtime.Object{withLabels(portal("p", map[string]interface{}{"replicas": int64(2)}, true),
				map[string]string{"krateo.io/composition-version": "v1-0-0", "spoke-added": "x"})},
			want: mirrorUnchanged,
		},
		{
			name:        "spec drifted: updated",
			hub:         withLabels(portal("p", map[string]interface{}{"replicas": int64(5)}, false), hubLabels),
			spoke:       []runtime.Object{withLabels(portal("p", map[string]interface{}{"replicas": int64(2)}, true), hubLabels)},
			want:        mirrorUpdated,
			wantUpdates: 1,
		},
		{
			name:        "a hub label changed: updated",
			hub:         withLabels(portal("p", map[string]interface{}{"replicas": int64(2)}, false), map[string]string{"krateo.io/composition-version": "v1-1-0"}),
			spoke:       []runtime.Object{withLabels(portal("p", map[string]interface{}{"replicas": int64(2)}, true), hubLabels)},
			want:        mirrorUpdated,
			wantUpdates: 1,
		},
		{
			name:        "management marker missing: updated",
			hub:         withLabels(portal("p", map[string]interface{}{"replicas": int64(2)}, false), hubLabels),
			spoke:       []runtime.Object{withLabels(portal("p", map[string]interface{}{"replicas": int64(2)}, false), hubLabels)},
			want:        mirrorUpdated,
			wantUpdates: 1,
		},
		{
			name:        "hub has no spec, mirror has one: updated (spec removed)",
			hub:         withLabels(portal("p", nil, false), hubLabels),
			spoke:       []runtime.Object{withLabels(portal("p", map[string]interface{}{"replicas": int64(2)}, true), hubLabels)},
			want:        mirrorUpdated,
			wantUpdates: 1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			spoke := newFakeDyn(tc.spoke...)
			creates, updates := spokeWrites(spoke)
			got, err := mirrorDown(ctx, spoke.Resource(testGVR).Namespace(testNamespace), tc.hub, testAPIVersion, testKind, testNamespace)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want || *creates != tc.wantCreates || *updates != tc.wantUpdates {
				t.Errorf("write=%q creates=%d updates=%d, want %q %d %d", got, *creates, *updates, tc.want, tc.wantCreates, tc.wantUpdates)
			}
		})
	}
}

// The issue's acceptance: a resync with nothing changed sends no write to the spoke. The first pass
// converges the mirror (the control: it does write), the second finds nothing to do.
func TestReflectInstances_ResyncWithNothingChangedSendsNoWrite(t *testing.T) {
	ctx := context.Background()
	hub := newFakeDyn(withLabels(portal("portal-a", map[string]interface{}{"replicas": int64(2)}, false),
		map[string]string{"krateo.io/composition-version": "v1-0-0"}))
	spoke := newFakeDyn()
	creates, updates := spokeWrites(spoke)
	params := reflectParams{
		hub: hub, resolveSpoke: staticResolver(spoke), defaultTarget: "spoke", gvr: testGVR,
		apiVersion: testAPIVersion, kind: testKind, namespace: testNamespace, log: logging.NewNopLogger(),
	}
	if err := reflectInstances(ctx, params); err != nil {
		t.Fatal(err)
	}
	if *creates != 1 {
		t.Fatalf("control: the first pass must create the mirror, creates=%d", *creates)
	}
	for pass := 2; pass <= 3; pass++ {
		if err := reflectInstances(ctx, params); err != nil {
			t.Fatal(err)
		}
	}
	if *creates != 1 || *updates != 0 {
		t.Errorf("two resyncs with nothing changed sent creates=%d updates=%d after the first pass's create, want 1 and 0", *creates, *updates)
	}
}
