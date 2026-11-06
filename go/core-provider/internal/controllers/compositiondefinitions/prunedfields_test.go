package compositiondefinitions

import (
	"strings"
	"testing"
)

// #377. This sentence is the only thing an operator reads at the moment values are lost, and it used
// to send them to a log that never held the values.
func TestPrunedFieldsDetail_DoesNotPromiseWhatWasNotKept(t *testing.T) {
	got := prunedFieldsDetail(false)

	if !strings.Contains(got, "not kept") {
		t.Errorf("it must say the values are gone: %q", got)
	}
	if strings.Contains(got, "log") {
		t.Errorf("it must not point at a log that does not hold them: %q", got)
	}
	if !strings.Contains(got, "source manifest") {
		t.Errorf("it must say where they can be restored from: %q", got)
	}
}

func TestPrunedFieldsDetail_NamesTheAnnotationWhenRecorded(t *testing.T) {
	if got := prunedFieldsDetail(true); !strings.Contains(got, "krateo.io/pruned-fields") {
		t.Errorf("it must name where the values are: %q", got)
	}
}
