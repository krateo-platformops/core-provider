package writtenby

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestSet(t *testing.T) {
	stored := map[string]any{"name": Name, "version": "2.13.134", "commit": "old"}
	obj := func() *unstructured.Unstructured {
		return &unstructured.Unstructured{Object: map[string]any{"status": map[string]any{"writtenBy": stored, "digest": "d"}}}
	}

	t.Run("a stamp replaces the stored one and touches nothing else", func(t *testing.T) {
		mg := obj()
		if err := (Stamp{Version: "2.13.135", Commit: "new"}).Set(mg); err != nil {
			t.Fatal(err)
		}
		want := map[string]any{"status": map[string]any{"digest": "d",
			"writtenBy": map[string]any{"name": "composition-dynamic-controller", "version": "2.13.135", "commit": "new"}}}
		if !reflect.DeepEqual(mg.Object, want) {
			t.Errorf("object = %v, want %v", mg.Object, want)
		}
	})

	t.Run("a zero stamp leaves it as it is", func(t *testing.T) {
		mg := obj()
		if err := (Stamp{}).Set(mg); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(mg.Object, obj().Object) {
			t.Errorf("a zero stamp changed the object: %v", mg.Object)
		}
	})
}

func TestContext(t *testing.T) {
	if got := FromContext(context.Background()); !got.IsZero() {
		t.Errorf("a context with no stamp carries %+v", got)
	}
	s := Stamp{Version: "2.13.135", Commit: "c"}
	if got := FromContext(NewContext(context.Background(), s)); got != s {
		t.Errorf("FromContext = %+v, want %+v", got, s)
	}
}
