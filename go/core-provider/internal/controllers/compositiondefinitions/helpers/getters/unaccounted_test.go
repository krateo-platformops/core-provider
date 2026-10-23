package getters

import (
	"context"
	"testing"

	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/fake"
)

var unaccountedGVR = schema.GroupVersionResource{
	Group: "composition.krateo.io", Version: "v2-0-0", Resource: "widgets",
}

func widget(name string, labels map[string]string) *unstructured.Unstructured {
	meta := map[string]any{"name": name, "namespace": "default"}
	if labels != nil {
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		meta["labels"] = l
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "composition.krateo.io/v2-0-0",
		"kind":       "Widget",
		"metadata":   meta,
	}}
}

// An instance that no per-version check can see must be reported, because the served-version
// prune's safety argument rests entirely on those checks (#189).
func TestGetUnaccountedCompositions(t *testing.T) {
	known := []string{"v1-0-0", "v2-0-0", "vacuum"}

	cases := []struct {
		name  string
		objs  []runtime.Object
		want  int
		wants []string
	}{
		{
			name: "every instance carries a known version label",
			objs: []runtime.Object{
				widget("a", map[string]string{deploy.CompositionVersionLabel: "v2-0-0"}),
				widget("b", map[string]string{deploy.CompositionVersionLabel: "v1-0-0"}),
			},
			want: 0,
		},
		{
			name:  "an instance with NO version label (the policy-absence gap)",
			objs:  []runtime.Object{widget("a", map[string]string{deploy.CompositionVersionLabel: "v2-0-0"}), widget("naked", nil)},
			want:  1,
			wants: []string{"naked"},
		},
		{
			name:  "an instance with no labels at all",
			objs:  []runtime.Object{widget("naked", map[string]string{"unrelated": "x"})},
			want:  1,
			wants: []string{"naked"},
		},
		{
			name:  "an instance labelled with a version the CRD no longer carries",
			objs:  []runtime.Object{widget("stale", map[string]string{deploy.CompositionVersionLabel: "v0-9-0"})},
			want:  1,
			wants: []string{"stale"},
		},
		{
			name: "no instances at all",
			objs: nil,
			want: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dyn := fake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{unaccountedGVR: "WidgetList"}, tc.objs...)

			ul, err := GetUnaccountedCompositions(context.Background(), dyn, unaccountedGVR, known)
			if err != nil {
				t.Fatalf("GetUnaccountedCompositions: %v", err)
			}
			if got := len(ul.Items); got != tc.want {
				var names []string
				for i := range ul.Items {
					names = append(names, ul.Items[i].GetName())
				}
				t.Fatalf("found %d unaccounted instances, want %d (%v)", got, tc.want, names)
			}
			for _, w := range tc.wants {
				found := false
				for i := range ul.Items {
					if ul.Items[i].GetName() == w {
						found = true
					}
				}
				if !found {
					t.Errorf("expected %q to be reported as unaccounted", w)
				}
			}
		})
	}
}

// The selectors must be well-formed strings the apiserver will accept; a malformed one would
// make the List fail, and a prune that errors is a prune that never runs.
func TestUnaccountedSelectorsAreWellFormed(t *testing.T) {
	for _, known := range [][]string{nil, {"v1-0-0"}, {"v1-0-0", "v2-0-0", "vacuum"}} {
		sels := unaccountedSelectors(known)
		if len(sels) == 0 {
			t.Fatalf("no selectors produced for known=%v", known)
		}
		if sels[0] != "!"+deploy.CompositionVersionLabel {
			t.Errorf("first selector = %q, want the does-not-exist form", sels[0])
		}
		if len(known) > 0 && len(sels) != 2 {
			t.Errorf("known=%v produced %d selectors, want 2", known, len(sels))
		}
		if len(known) == 0 && len(sels) != 1 {
			t.Errorf("empty known set produced %d selectors, want 1", len(sels))
		}
	}
}
