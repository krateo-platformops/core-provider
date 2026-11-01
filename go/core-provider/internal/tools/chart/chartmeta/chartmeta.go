// Package chartmeta reads the two Chart.yaml fields that name a composition's generated CRD.
package chartmeta

import (
	"fmt"
)

// StringField returns a Chart.yaml field that MUST be a string, or an error naming what arrived
// instead.
//
// It exists because the obvious spelling — res["name"].(string) — PANICS rather than erroring, and
// the input that triggers it is an ordinary chart rather than a corrupt one. Both parsers this
// repository pins resolve YAML 1.1, measured:
//
//	gopkg.in/yaml.v2 v2.4.0   name: y    -> true (bool)
//	sigs.k8s.io/yaml v1.6.0   name: y    -> true (bool)
//	                          name: no   -> false (bool)
//	                          name: on   -> true (bool)
//	                          name: NO   -> false (bool)
//	                          version: 1.0 -> 1 (float64)
//
// Helm's own chart-name rule is `[a-z0-9]([-a-z0-9]*[a-z0-9])?`, so `y`, `no`, `on` and `off` are
// all VALID chart names. A blueprint author shipping one of them crashed the controller — a panic in
// a reconcile loop is a crash loop, not a failed reconcile, and it takes every other composition
// that controller serves down with it.
//
// An error here instead means the one CompositionDefinition is rejected with a message that says
// what to do, and nothing else is affected.
func StringField(res map[string]any, field, chartPath string) (string, error) {
	v, ok := res[field]
	if !ok {
		return "", fmt.Errorf("%s: %q is missing", chartPath, field)
	}
	s, ok := v.(string)
	if !ok {
		// The remedy is in the message because it is not guessable from the symptom: the author sees
		// a chart that is valid by Helm's rules being refused.
		return "", fmt.Errorf("%s: %q must be a string but parsed as %T (%v) — YAML 1.1 reads "+
			"y/yes/no/on/off/NO as booleans and unquoted decimals as numbers, so quote it: %s: %q",
			chartPath, field, v, v, field, fmt.Sprintf("%v", v))
	}
	if s == "" {
		return "", fmt.Errorf("%s: %q is empty", chartPath, field)
	}
	return s, nil
}
