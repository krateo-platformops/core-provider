package compositiondefinitions

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/cdreason"
	"github.com/krateo-platformops/core-provider/internal/controllers/compositiondefinitions/helpers/getters"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// core-provider#614: how one validation becomes MigratedValuesValid.
func TestMigratedValuesCondition_614(t *testing.T) {
	t.Run("a list failure is Unknown, never a false all-clear", func(t *testing.T) {
		c := migratedValuesCondition(getters.ValidationResult{Version: "v2-0-0"}, errors.New("boom"))
		if !cdreason.MigratedValuesUnchecked.Is(c) || !strings.Contains(c.Message, "could not be listed") {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("nothing refused or unchecked is True", func(t *testing.T) {
		c := migratedValuesCondition(getters.ValidationResult{Version: "v2-0-0", Checked: 3}, nil)
		if !cdreason.MigratedValuesAllValid.Is(c) || c.Message != "all 3 instance(s) on version v2-0-0 satisfy its schema" {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("refused wins over unchecked, and the message counts both", func(t *testing.T) {
		c := migratedValuesCondition(getters.ValidationResult{Version: "v2-0-0", Checked: 3,
			Refused:   []getters.RefusedInstance{{Namespace: "ns", Name: "a", Fields: []string{"spec.x"}}},
			Unchecked: []getters.UncheckedInstance{{Namespace: "ns", Name: "b", Err: "e"}}}, nil)
		want := "1 of 3 instance(s) on version v2-0-0 hold values that version refuses; a write that changes those fields is refused until they satisfy it: ns/a (spec.x); 1 more could not be checked"
		if !cdreason.MigratedValuesRefused.Is(c) || c.Message != want {
			t.Fatalf("got %+v\nwant %q", c, want)
		}
	})
	t.Run("only unchecked is Unknown, naming them without their error text", func(t *testing.T) {
		c := migratedValuesCondition(getters.ValidationResult{Version: "v2-0-0", Checked: 2,
			Unchecked: []getters.UncheckedInstance{{Namespace: "ns", Name: "b", Err: "webhook said no at 12:00:01"}}}, nil)
		if !cdreason.MigratedValuesUnchecked.Is(c) || !strings.Contains(c.Message, "ns/b") || strings.Contains(c.Message, "12:00:01") {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("instances and fields are capped", func(t *testing.T) {
		var rs []getters.RefusedInstance
		for i := 0; i < 12; i++ {
			rs = append(rs, getters.RefusedInstance{Namespace: "ns", Name: fmt.Sprintf("i%02d", i),
				Fields: []string{"spec.a", "spec.b", "spec.c", "spec.d", "spec.e", "spec.f"}})
		}
		c := migratedValuesCondition(getters.ValidationResult{Version: "v2-0-0", Checked: 12, Refused: rs}, nil)
		if strings.Contains(c.Message, "ns/i10") || !strings.Contains(c.Message, "ns/i09") || !strings.HasSuffix(c.Message, ", ...") {
			t.Fatalf("instance cap: %q", c.Message)
		}
		if strings.Contains(c.Message, "spec.f") || !strings.Contains(c.Message, "spec.e, ...)") {
			t.Fatalf("field cap: %q", c.Message)
		}
	})
}

// An absent condition must not trigger a recheck: GetCondition returns {Type, Unknown} for an absent
// type, which would otherwise read as "unchecked" and re-validate every definition on every Observe.
func TestRecheckMigratedValues_AbsentDoesNothing_614(t *testing.T) {
	cd := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	(&external{}).recheckMigratedValues(t.Context(), cd,
		schema.GroupVersionResource{Group: "composition.krateo.io", Version: "v2-0-0", Resource: "widgets"}, logging.NewNopLogger())
	if _, present := migratedValuesValid(cd); present {
		t.Fatal("an absent condition was set by a recheck that should not have run")
	}
}
