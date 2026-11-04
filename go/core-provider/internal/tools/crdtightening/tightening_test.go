package crdtightening

// The comparison in tightening.go, each clause fired and each NON-finding pinned.
//
// WHY THIS IS SEPARATE FROM THE GATE. The gate needs the published baseline, so it needs the
// network, so it sits behind a build tag (tightening_baseline_test.go). A clause exercised only
// when a registry answers is a clause nobody controls — and the whole argument for this gate is
// that an unexercised check is indistinguishable from a working one. These cases run offline in
// the ordinary `go test ./...` that crd-drift already executes.
//
// THE NEGATIVES CARRY AS MUCH WEIGHT AS THE POSITIVES. This comparison is deliberately
// one-directional: a LOOSENING is not a finding. Every tightening case below therefore has its
// mirror, and the bound cases are pinned in both directions specifically because the direction
// table is the one place an inverted comparison would hide — it would pass the real tightening
// while failing every harmless widening, and the four positives alone cannot tell those apart.

import (
	"strings"
	"testing"
)

func TestTightenings(t *testing.T) {
	obj := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	props := func(kv ...any) map[string]any { return obj("properties", obj(kv...)) }

	cases := []struct {
		name   string
		old    map[string]any
		new    map[string]any
		expect int
		path   string // asserted when non-empty
	}{
		// --- required ---
		{"a new required name", props("a", obj("type", "string")),
			obj("required", []any{"a"}, "properties", obj("a", obj("type", "string"))), 1, "root"},
		{"a required name dropped is a loosening",
			obj("required", []any{"a"}, "properties", obj("a", obj("type", "string"))),
			props("a", obj("type", "string")), 0, ""},

		// --- enum ---
		{"an enum value removed", obj("enum", []any{"x", "y", "z"}), obj("enum", []any{"x", "y"}), 1, "root"},
		{"an enum value added is a loosening", obj("enum", []any{"x"}), obj("enum", []any{"x", "y"}), 0, ""},
		{"an enum introduced where nothing constrained",
			obj("type", "string"), obj("type", "string", "enum", []any{"x"}), 1, "root"},
		{"an enum removed entirely is a loosening",
			obj("type", "string", "enum", []any{"x"}), obj("type", "string"), 0, ""},

		// --- type and format ---
		{"a type change", obj("type", "string"), obj("type", "integer"), 1, "root"},
		{"a type ABSENT on one side is not a change — an untyped node is not a retype",
			props("a", obj()), props("a", obj("type", "string")), 0, ""},
		{"a format introduced", obj("type", "string"), obj("type", "string", "format", "date-time"), 1, "root"},
		{"a format changed", obj("type", "string", "format", "date"),
			obj("type", "string", "format", "date-time"), 1, "root"},
		{"a format removed is a loosening", obj("type", "string", "format", "date-time"),
			obj("type", "string"), 0, ""},

		// --- preserve-unknown. The clause no chart in this repo can exercise through the gate,
		//     because widget CRDs are strict by rule — so these two cases are its only cover. ---
		{"preserve-unknown dropped (true -> absent)",
			obj("type", "object", preserveUnknown, true), obj("type", "object"), 1, "root"},
		{"preserve-unknown dropped (true -> false)",
			obj("type", "object", preserveUnknown, true),
			obj("type", "object", preserveUnknown, false), 1, "root"},
		{"preserve-unknown gained is a loosening",
			obj("type", "object"), obj("type", "object", preserveUnknown, true), 0, ""},

		// --- bounds, every key in BOTH directions ---
		{"minimum raised is a tightening", obj("type", "integer", "minimum", 1),
			obj("type", "integer", "minimum", 5), 1, "root"},
		{"minimum LOWERED is a loosening", obj("type", "integer", "minimum", 5),
			obj("type", "integer", "minimum", 1), 0, ""},
		{"maximum lowered is a tightening", obj("type", "integer", "maximum", 100),
			obj("type", "integer", "maximum", 10), 1, "root"},
		{"maximum RAISED is a loosening", obj("type", "integer", "maximum", 10),
			obj("type", "integer", "maximum", 100), 0, ""},
		{"minLength raised is a tightening", obj("type", "string", "minLength", 0),
			obj("type", "string", "minLength", 1), 1, "root"},
		{"minLength LOWERED is a loosening", obj("type", "string", "minLength", 1),
			obj("type", "string", "minLength", 0), 0, ""},
		{"maxLength lowered is a tightening", obj("type", "string", "maxLength", 255),
			obj("type", "string", "maxLength", 64), 1, "root"},
		{"maxLength RAISED is a loosening", obj("type", "string", "maxLength", 64),
			obj("type", "string", "maxLength", 255), 0, ""},
		{"a bound introduced where none existed is a tightening",
			obj("type", "string"), obj("type", "string", "maxLength", 64), 1, "root"},
		{"a bound REMOVED is a loosening",
			obj("type", "string", "maxLength", 64), obj("type", "string"), 0, ""},
		{"an unchanged bound says nothing", obj("type", "integer", "minimum", 1, "maximum", 10),
			obj("type", "integer", "minimum", 1, "maximum", 10), 0, ""},
		{"a non-numeric bound is malformed, not a bound of 1",
			obj("type", "string"), obj("type", "string", "maxLength", true), 0, ""},

		// --- pattern ---
		{"a pattern introduced is a tightening",
			obj("type", "string"), obj("type", "string", "pattern", "^a+$"), 1, "root"},
		{"a pattern CHANGED is reported, even when it may be a widening",
			obj("type", "string", "pattern", "^a$"), obj("type", "string", "pattern", "^a+$"), 1, "root"},
		{"a pattern removed is a loosening",
			obj("type", "string", "pattern", "^a+$"), obj("type", "string"), 0, ""},

		// --- a removed property: silent data loss, not a refused write ---
		{"a property removed is a tightening",
			obj("type", "object", "properties", obj("a", obj("type", "string"), "b", obj("type", "string"))),
			obj("type", "object", "properties", obj("a", obj("type", "string"))), 1, "root"},
		{"a property removed under preserve-unknown is NOT a finding",
			obj("type", "object", preserveUnknown, true, "properties", obj("a", obj("type", "string"))),
			obj("type", "object", preserveUnknown, true, "properties", obj()), 0, ""},
		{"a property removed nested two levels down is found, with its path",
			props("a", obj("type", "object", "properties",
				obj("b", obj("type", "string"), "c", obj("type", "string")))),
			props("a", obj("type", "object", "properties", obj("b", obj("type", "string")))), 1, "root.a"},
		{"a NEW property is not a finding — nothing stored uses it",
			obj("type", "object", "properties", obj("a", obj("type", "string"))),
			obj("type", "object", "properties", obj("a", obj("type", "string"), "b", obj("type", "string"))),
			0, ""},

		// --- recursion ---
		{"a tightening nested two levels down is found, with its path",
			props("a", props("b", obj("type", "string"))),
			props("a", props("b", obj("type", "integer"))), 1, "root.a.b"},
		{"a tightening inside array items is found",
			obj("type", "array", "items", obj("type", "object", "properties", obj("a", obj("type", "string")))),
			obj("type", "array", "items", obj("type", "object", "required", []any{"a"},
				"properties", obj("a", obj("type", "string")))), 1, "root[]"},
		{"a tightening inside additionalProperties is found",
			obj("type", "object", "additionalProperties", obj("type", "string")),
			obj("type", "object", "additionalProperties", obj("type", "integer")), 1, "root{*}"},

		// --- the 1.8.20 defect in the shape it shipped ---
		{"the 1.8.20 shape — items gains required",
			props("resourcesRefsTemplate", obj("type", "array", "items",
				obj("type", "object", "properties",
					obj("iterator", obj("type", "string"), "template", obj("type", "object"))))),
			props("resourcesRefsTemplate", obj("type", "array", "items",
				obj("type", "object", "required", []any{"id"}, "properties",
					obj("iterator", obj("type", "string"), "template", obj("type", "object"))))),
			1, "root.resourcesRefsTemplate[]"},

		{"identical schemas say nothing",
			obj("type", "object", "required", []any{"a"}, "properties",
				obj("a", obj("type", "string", "enum", []any{"x"}))),
			obj("type", "object", "required", []any{"a"}, "properties",
				obj("a", obj("type", "string", "enum", []any{"x"}))), 0, ""},
	}

	for _, c := range cases {
		got := Tightenings(c.old, c.new, "root")
		if len(got) != c.expect {
			t.Errorf("%s: expected %d finding(s), got %d: %v", c.name, c.expect, len(got), got)
			continue
		}
		if c.path != "" && len(got) > 0 && got[0].Path != c.path {
			t.Errorf("%s: finding is at %q, expected %q", c.name, got[0].Path, c.path)
		}
	}
	t.Logf("%d cases", len(cases))
}

// The removed-property finding carries a QUERY, not an answer. A schema fact says what changed; the
// count of live objects still setting the field says whether it matters, and a reviewer cannot tell
// a dead field from a load-bearing one without it — both look identical in a diff. The count needs a
// cluster, which CI does not have and should not require, so this pins the query's shape instead.
func TestLiveCountHint(t *testing.T) {
	cases := []struct {
		name string
		path string
		lost []string
		want string
	}{
		{"a top-level spec property",
			"tickets.servicedesk.krateo.io/v1alpha1.spec", []string{"onBehalfOf"},
			"kubectl get tickets.servicedesk.krateo.io -A -o json | jq '[.items[] | select(.spec.onBehalfOf != null)] | length'"},
		{"nested, with an array on the way — [] becomes jq's []?",
			"buttons.widgets.templates.krateo.io/v1beta1.spec.resourcesRefsTemplate[]", []string{"iterator"},
			"kubectl get buttons.widgets.templates.krateo.io -A -o json | jq '[.items[] | select(.spec.resourcesRefsTemplate[]?.iterator != null)] | length'"},
		{"an object map — {*} also becomes []?, which iterates values in jq",
			"x.example.io/v1.spec.data{*}", []string{"k"},
			"kubectl get x.example.io -A -o json | jq '[.items[] | select(.spec.data[]?.k != null)] | length'"},
	}
	for _, c := range cases {
		got := liveCountHint(c.path, c.lost)
		if !strings.Contains(got, c.want) {
			t.Errorf("%s:\n  want substring: %s\n  got:            %s", c.name, c.want, got)
		}
	}

	// A hint that is wrong is worse than none, because it looks like evidence. Both of these must
	// yield nothing rather than a command that counts the wrong thing.
	if h := liveCountHint("no-slash-in-this-path", []string{"a"}); h != "" {
		t.Errorf("a path with no CRD segment must produce no hint, got %q", h)
	}
	if h := liveCountHint("x.example.io/v1.spec.we'ird", []string{"a"}); h != "" {
		t.Errorf("a path that would break the shell quoting must produce no hint, got %q", h)
	}
}
