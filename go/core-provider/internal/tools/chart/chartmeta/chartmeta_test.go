package chartmeta

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v2"
	sigsyaml "sigs.k8s.io/yaml"
)

// THE CASE THE FIX EXISTS FOR. Every one of these is a VALID Helm chart name by Helm's own rule
// ([a-z0-9]([-a-z0-9]*[a-z0-9])?), and every one parses as a bool under YAML 1.1 — which is what
// both parsers this repository pins resolve. The obvious spelling, res["name"].(string), PANICS on
// each, and a panic inside a reconcile loop is a crash loop that takes down every composition the
// controller serves, not one failed reconcile.
//
// Parameterised over BOTH parsers rather than one, because the two call sites use different
// libraries (chartfs/gvr.go uses gopkg.in/yaml.v2, chart/chart.go uses sigs.k8s.io/yaml) and
// "we are on the other library so we are fine" is exactly the assumption worth disproving.
func TestStringField_BooleanishChartNamesError(t *testing.T) {
	parsers := map[string]func([]byte, any) error{
		"gopkg.in/yaml.v2": func(b []byte, v any) error { return yaml.Unmarshal(b, v) },
		"sigs.k8s.io/yaml": func(b []byte, v any) error { return sigsyaml.Unmarshal(b, v) },
	}

	for pname, unmarshal := range parsers {
		for _, name := range []string{"y", "yes", "no", "on", "off", "NO", "Yes", "n"} {
			res := map[string]any{}
			if err := unmarshal([]byte("name: "+name+"\nversion: 1.0.0\n"), &res); err != nil {
				t.Fatalf("%s: parsing %q: %v", pname, name, err)
			}

			// CALIBRATION. If this parser left the value a string, the case proves nothing about the
			// fix — it never reached the branch under test. Printing that is the point: a silently
			// inapplicable case is how a guard ends up asserting its own mention.
			if s, isString := res["name"].(string); isString {
				t.Logf("%s: name: %s stayed a string (%q) — this parser does not convert it, "+
					"case not exercised", pname, name, s)
				continue
			}

			got, err := StringField(res, "name", "chart/Chart.yaml")
			if err == nil {
				t.Errorf("%s: name: %s returned %q with no error; a non-string must be refused, "+
					"not coerced", pname, name, got)
				continue
			}
			// The message must say what to do. The author is looking at a chart that is valid by
			// Helm's rules being refused, so "must be a string" alone does not get them anywhere.
			if !strings.Contains(err.Error(), "quote it") {
				t.Errorf("%s: name: %s error does not say to quote it: %v", pname, name, err)
			}
			if !strings.Contains(err.Error(), "chart/Chart.yaml") {
				t.Errorf("%s: name: %s error does not name the file: %v", pname, name, err)
			}
		}
	}
}

// An unquoted decimal version is the same failure with a different type. `version: 1.0` is a float64
// and `.(string)` panics identically.
func TestStringField_UnquotedDecimalVersionErrors(t *testing.T) {
	res := map[string]any{}
	if err := yaml.Unmarshal([]byte("name: chart\nversion: 1.0\n"), &res); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, isString := res["version"].(string); isString {
		t.Skip("this parser kept version a string; case not exercised")
	}
	if _, err := StringField(res, "version", "chart/Chart.yaml"); err == nil {
		t.Error("version: 1.0 parses as float64 and must be refused rather than coerced")
	}
}

// THE CONTROL, and it is what stops the fix from being "refuse everything". An ordinary chart must
// still work — without this arm, a StringField that always errored would pass every test above.
func TestStringField_OrdinaryChartStillWorks(t *testing.T) {
	for _, src := range []string{
		"name: fireworksapp\nversion: 1.0.0\n",
		"name: \"y\"\nversion: \"1.0\"\n", // quoted: the documented remedy must actually work
		"name: my-chart-2\nversion: 0.13.8\n",
	} {
		res := map[string]any{}
		if err := yaml.Unmarshal([]byte(src), &res); err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		name, err := StringField(res, "name", "chart/Chart.yaml")
		if err != nil {
			t.Errorf("%q: name rejected: %v", src, err)
		}
		if name == "" {
			t.Errorf("%q: name came back empty", src)
		}
		if _, err := StringField(res, "version", "chart/Chart.yaml"); err != nil {
			t.Errorf("%q: version rejected: %v", src, err)
		}
	}
}

// Absent and empty are distinct from wrong-type and must each say so, since the remedy differs:
// a missing field is an incomplete chart, an empty one is a chart with a blank name, and a
// boolean-ish one is a quoting problem.
func TestStringField_AbsentAndEmptyAreDistinguished(t *testing.T) {
	if _, err := StringField(map[string]any{}, "name", "c/Chart.yaml"); err == nil {
		t.Error("an absent field must error")
	} else if !strings.Contains(err.Error(), "missing") {
		t.Errorf("an absent field should say missing, got: %v", err)
	}

	if _, err := StringField(map[string]any{"name": ""}, "name", "c/Chart.yaml"); err == nil {
		t.Error("an empty field must error")
	} else if !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty field should say empty, got: %v", err)
	}
}
