package compositiondefinitions

import (
	"fmt"
	"strings"
	"testing"

	"github.com/krateo-platformops/plumbing/crdgen"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/events"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
)

var (
	wDropped  = crdgen.Warning{Path: "spec.o", Kind: crdgen.KindConstraintDropped, Message: "not (non-trivial subschema) unsupported -> dropped"}
	wLoosened = crdgen.Warning{Path: "spec.a", Kind: crdgen.KindStructureLoosened, Message: "cycle at #/$defs/n -> preserve-unknown"}
	wStamp    = crdgen.Warning{Path: "spec.img", Kind: crdgen.KindParentDefaulted, Message: "given default {} so its nested defaults apply"}
	wNarrowed = crdgen.Warning{Path: "spec.u", Kind: crdgen.KindAcceptanceNarrowed, Message: "type [string boolean] is a union"}
)

// #362. Only a loss of enforcement turns the condition False. Each arm, including True.
func TestSchemaEnforcedCondition(t *testing.T) {
	for _, tc := range []struct {
		name       string
		ws         []crdgen.Warning
		wantStatus metav1.ConditionStatus
		inMsg      []string
		notInMsg   []string
	}{
		{"no warnings", nil, metav1.ConditionTrue, nil, nil},
		// Since plumbing v1.27.0 these are common and lose nothing; they must not read False.
		{"only informational and narrowing warnings", []crdgen.Warning{wStamp, wNarrowed}, metav1.ConditionTrue, nil, []string{"spec.img", "spec.u"}},
		{"a dropped constraint and a loosened structure, beside a note", []crdgen.Warning{wStamp, wLoosened, wDropped}, metav1.ConditionFalse,
			[]string{"2 constraint(s)", "spec.a: cycle", "spec.o: not"}, []string{"spec.img"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := schemaEnforcedCondition(tc.ws)
			if c.Type != conditionTypeSchemaFullyEnforced || c.Status != tc.wantStatus {
				t.Fatalf("got %s=%s, want %s", c.Type, c.Status, tc.wantStatus)
			}
			for _, s := range tc.inMsg {
				if !strings.Contains(c.Message, s) {
					t.Errorf("message %q does not contain %q", c.Message, s)
				}
			}
			for _, s := range tc.notInMsg {
				if strings.Contains(c.Message, s) {
					t.Errorf("message %q names %q, which loses nothing", c.Message, s)
				}
			}
		})
	}
}

// Sorted, so an unchanged render never churns status; capped, so a large schema cannot produce an
// unbounded condition message.
func TestSchemaEnforcedCondition_SortedAndCapped(t *testing.T) {
	var ws []crdgen.Warning
	for i := 14; i >= 0; i-- {
		ws = append(ws, crdgen.Warning{Path: fmt.Sprintf("spec.f%02d", i), Kind: crdgen.KindConstraintDropped, Message: "x"})
	}
	reversed := make([]crdgen.Warning, len(ws))
	for i := range ws {
		reversed[len(ws)-1-i] = ws[i]
	}
	a, b := schemaEnforcedCondition(ws), schemaEnforcedCondition(reversed)
	if a.Message != b.Message {
		t.Fatalf("the message depends on warning order:\n%s\n%s", a.Message, b.Message)
	}
	if !strings.Contains(a.Message, "spec.f00: x") || strings.Contains(a.Message, "spec.f10") || !strings.Contains(a.Message, "and 5 more") {
		t.Errorf("want the first 10 sorted and 'and 5 more', got %q", a.Message)
	}
}

func TestSchemaOwnedElsewhereCondition_IsUnknownAndNamesTheOwner(t *testing.T) {
	c := schemaOwnedElsewhereCondition("krateo-system", "owner-cd")
	if c.Status != metav1.ConditionUnknown || !strings.Contains(c.Message, "krateo-system/owner-cd") {
		t.Errorf("a follower must report Unknown naming its owner, got %s %q", c.Status, c.Message)
	}
}

// Events fire only when crdgen actually ran; a cache-hit pass emits nothing, while the condition
// still carries the fact.
func TestEmitSchemaEvents_OnlyOnAFreshRender(t *testing.T) {
	obj := &compositiondefinitionsv1alpha1.CompositionDefinition{}
	ws := []crdgen.Warning{wDropped, wStamp}

	rec := events.NewFakeRecorder(10)
	emitSchemaEvents(rec, obj, ws, false)
	if n := len(rec.Events); n != 0 {
		t.Fatalf("a cache-hit pass emitted %d event(s); only a fresh render should", n)
	}

	emitSchemaEvents(rec, obj, ws, true)
	var got []string
	for len(rec.Events) > 0 {
		got = append(got, <-rec.Events)
	}
	if len(got) != 2 {
		t.Fatalf("want one Warning (lost constraints) and one Normal (notes), got %v", got)
	}
	joined := strings.Join(got, "\n")
	for _, want := range []string{"Warning SchemaConstraintsNotEnforced", "spec.o", "Normal SchemaGenerationNotes", "spec.img"} {
		if !strings.Contains(joined, want) {
			t.Errorf("events %q do not contain %q", joined, want)
		}
	}

	clean := events.NewFakeRecorder(10)
	emitSchemaEvents(clean, obj, nil, true)
	if n := len(clean.Events); n != 0 {
		t.Errorf("a fresh render with no warnings emitted %d event(s)", n)
	}
}
