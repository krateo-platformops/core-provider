package writtenby

import (
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

func TestSet(t *testing.T) {
	stored := &compositiondefinitionsv1alpha1.WrittenBy{Name: Name, Version: "2.13.134", Commit: "old"}

	t.Run("a stamp replaces the stored one", func(t *testing.T) {
		cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
		cd.Status.WrittenBy = stored.DeepCopy()
		w := New("2.13.135", "new")
		Set(cd, w)
		if cd.Status.WrittenBy == nil || *cd.Status.WrittenBy != (compositiondefinitionsv1alpha1.WrittenBy{Name: "core-provider", Version: "2.13.135", Commit: "new"}) {
			t.Fatalf("status.writtenBy = %+v", cd.Status.WrittenBy)
		}
		// A copy, so a later change to the caller's stamp cannot reach an object already stamped.
		w.Version = "changed"
		if cd.Status.WrittenBy.Version != "2.13.135" {
			t.Errorf("the stamp aliases the caller's value: %+v", cd.Status.WrittenBy)
		}
	})

	t.Run("a zero stamp leaves it as it is", func(t *testing.T) {
		cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
		cd.Status.WrittenBy = stored.DeepCopy()
		Set(cd, compositiondefinitionsv1alpha1.WrittenBy{})
		if cd.Status.WrittenBy == nil || *cd.Status.WrittenBy != *stored {
			t.Errorf("a zero stamp changed status.writtenBy to %+v", cd.Status.WrittenBy)
		}
		absent := &compositiondefinitionsv1alpha1.CompositionDefinition{}
		Set(absent, compositiondefinitionsv1alpha1.WrittenBy{})
		if absent.Status.WrittenBy != nil {
			t.Errorf("a zero stamp created status.writtenBy: %+v", absent.Status.WrittenBy)
		}
	})
}
