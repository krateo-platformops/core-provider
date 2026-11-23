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
		c, _ := migratedValuesReport(getters.ValidationResult{Version: "v2-0-0"}, errors.New("boom"))
		if !cdreason.MigratedValuesUnchecked.Is(c) || !strings.Contains(c.Message, "could not be listed") {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("nothing refused or unchecked is True", func(t *testing.T) {
		c, _ := migratedValuesReport(getters.ValidationResult{Version: "v2-0-0", Checked: 3}, nil)
		if !cdreason.MigratedValuesAllValid.Is(c) || c.Message != "all 3 instance(s) on version v2-0-0 satisfy its schema" {
			t.Fatalf("got %+v", c)
		}
	})
	t.Run("refused wins over unchecked, and the message counts both", func(t *testing.T) {
		c, _ := migratedValuesReport(getters.ValidationResult{Version: "v2-0-0", Checked: 3,
			Refused:   []getters.RefusedInstance{{Namespace: "ns", Name: "a", Fields: []string{"spec.x"}}},
			Unchecked: []getters.UncheckedInstance{{Namespace: "ns", Name: "b", Err: "e"}}}, nil)
		want := "1 of 3 instance(s) on version v2-0-0 hold values that version refuses; a write that changes those fields is refused until they satisfy it: ns/a (spec.x); 1 more could not be checked"
		if !cdreason.MigratedValuesRefused.Is(c) || c.Message != want {
			t.Fatalf("got %+v\nwant %q", c, want)
		}
	})
	t.Run("only unchecked is Unknown, naming them without their error text", func(t *testing.T) {
		c, _ := migratedValuesReport(getters.ValidationResult{Version: "v2-0-0", Checked: 2,
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
		c, _ := migratedValuesReport(getters.ValidationResult{Version: "v2-0-0", Checked: 12, Refused: rs}, nil)
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

// core-provider#623: status.migratedValues and the message come from one builder. The expectations
// here are built from the check result in the test, independently of the code under test, and both
// renderings are compared to them, so neither rendering vouches for the other.
func TestMigratedValuesReport_623(t *testing.T) {
	const (
		maxI = compositiondefinitionsv1alpha1.MigratedValuesMaxInstances
		maxF = compositiondefinitionsv1alpha1.MigratedValuesMaxFields
	)
	var refused []getters.RefusedInstance
	for i := 0; i < maxI+2; i++ { // two beyond the instance cap
		fields := []string{"spec.a"}
		if i == 0 {
			fields = []string{"spec.a", "spec.b", "spec.c", "spec.d", "spec.e", "spec.f", "spec.g"} // beyond the field cap
		}
		refused = append(refused, getters.RefusedInstance{Namespace: "ns", Name: fmt.Sprintf("r%02d", i), Fields: fields})
	}
	unchecked := []getters.UncheckedInstance{{Namespace: "ns", Name: "u0", Err: "webhook said no"}}
	res := getters.ValidationResult{Version: "v2-0-0", Checked: len(refused) + len(unchecked), Refused: refused, Unchecked: unchecked}

	c, v := migratedValuesReport(res, nil)
	if !cdreason.MigratedValuesRefused.Is(c) || v == nil {
		t.Fatalf("condition %+v, values %+v", c, v)
	}
	if v.Version != "v2-0-0" || v.RefusedCount != maxI+2 || v.UncheckedCount != 1 {
		t.Fatalf("counts must be uncapped: %+v", v)
	}
	if len(v.Refused) != maxI || len(v.Unchecked) != 1 {
		t.Fatalf("lists must be capped at %d: refused=%d unchecked=%d", maxI, len(v.Refused), len(v.Unchecked))
	}
	// Expected entries, in check order, built here.
	for i := 0; i < maxI; i++ {
		want := refused[i]
		got := v.Refused[i]
		wantFields := want.Fields
		if len(wantFields) > maxF {
			wantFields = wantFields[:maxF]
		}
		if got.Namespace != want.Namespace || got.Name != want.Name || strings.Join(got.Fields, ",") != strings.Join(wantFields, ",") || got.FieldCount != len(want.Fields) {
			t.Fatalf("refused[%d]=%+v, want %s/%s fields=%v fieldCount=%d", i, got, want.Namespace, want.Name, wantFields, len(want.Fields))
		}
		entry := fmt.Sprintf("%s/%s (%s", want.Namespace, want.Name, strings.Join(wantFields, ", "))
		if !strings.Contains(c.Message, entry) {
			t.Fatalf("the message does not name refused[%d] as %q: %q", i, entry, c.Message)
		}
	}
	// Nothing beyond the cap in either rendering.
	for i := maxI; i < len(refused); i++ {
		if strings.Contains(c.Message, refused[i].Name) {
			t.Fatalf("the message names %s, beyond the cap", refused[i].Name)
		}
	}
	if strings.Contains(c.Message, "spec.f") || !strings.Contains(c.Message, "spec.e, ...)") {
		t.Fatalf("the message must cap fields at %d and mark it: %q", maxF, c.Message)
	}
	// Same order: the message's entries appear in the field's order.
	last := -1
	for _, r := range v.Refused {
		at := strings.Index(c.Message, r.Namespace+"/"+r.Name+" (")
		if at <= last {
			t.Fatalf("the message's order differs from the field's at %s/%s", r.Namespace, r.Name)
		}
		last = at
	}

	t.Run("only unchecked: Unknown with the field naming them", func(t *testing.T) {
		c, v := migratedValuesReport(getters.ValidationResult{Version: "v2-0-0", Checked: 2, Unchecked: unchecked}, nil)
		if !cdreason.MigratedValuesUnchecked.Is(c) || v == nil || v.RefusedCount != 0 || len(v.Refused) != 0 ||
			v.UncheckedCount != 1 || len(v.Unchecked) != 1 || v.Unchecked[0].Name != "u0" || !strings.Contains(c.Message, "ns/u0") {
			t.Fatalf("condition %+v, values %+v", c, v)
		}
	})
	t.Run("all valid clears the field", func(t *testing.T) {
		c, v := migratedValuesReport(getters.ValidationResult{Version: "v2-0-0", Checked: 3}, nil)
		if !cdreason.MigratedValuesAllValid.Is(c) || v != nil {
			t.Fatalf("condition %+v, values %+v", c, v)
		}
	})
	t.Run("a list failure has no field: zero counts would read as an all-clear", func(t *testing.T) {
		c, v := migratedValuesReport(getters.ValidationResult{Version: "v2-0-0"}, errors.New("boom"))
		if !cdreason.MigratedValuesUnchecked.Is(c) || v != nil {
			t.Fatalf("condition %+v, values %+v", c, v)
		}
	})
}
