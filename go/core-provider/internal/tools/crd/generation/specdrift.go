package generation

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// SpecDrift compares the spec schema a CRD SERVES for one version with the spec schema just
// generated for it (core-provider#534).
//
// WHY. core-provider writes a version's spec schema only when it APPENDS the version; afterwards
// ProjectWrite refreshed the status schema alone. So a generator fix (plumbing#186: an untyped node
// no longer typed `object`) never reached a version already served: on krateo-057 after 2.13.103,
// six definitions kept refusing strings and lists at untyped nodes until their chart version moved.
// Freezing has a real purpose, a regenerated schema could start refusing values already stored, so
// the refresh is allowed only when it is PROVABLY a loosening (Diego, 2026-11-15).
//
// "PROVABLY" IS AN ALLOWLIST. Every difference must be one of the moves listed in looserMoves; any
// other difference, including one this code has never seen, is reported and keeps the version
// frozen. crdtightening answers the opposite question (the ways a schema got stricter that it knows
// of), so its silence is not proof of a loosening and it is deliberately not reused here.
type SpecDrift struct {
	// Version is the CRD version compared.
	Version string
	// Equal: the served spec schema is the generated one. Nothing to do.
	Equal bool
	// NotLooser lists each difference that is not a known loosening, as "path: why", sorted. Empty
	// with Equal false means every difference is a loosening and the refresh is safe.
	NotLooser []string
}

// Looser reports whether the generated spec schema differs from the served one only by loosenings.
func (d SpecDrift) Looser() bool { return !d.Equal && len(d.NotLooser) == 0 }

// CompareSpec compares live's spec schema for generated's version with generated's. A live CRD
// that does not serve that version compares Equal: there is nothing served to refresh, and adding
// the version is AppendVersion's job.
func CompareSpec(live, generated *apiextensionsv1.CustomResourceDefinition) (SpecDrift, error) {
	if generated == nil || len(generated.Spec.Versions) == 0 {
		return SpecDrift{}, fmt.Errorf("the generated CRD has no version")
	}
	version := generated.Spec.Versions[0].Name
	d := SpecDrift{Version: version, Equal: true}
	served, ok := specSchemaOf(live, version)
	if !ok {
		return d, nil
	}
	gen, ok := specSchemaOf(generated, version)
	if !ok {
		return d, fmt.Errorf("the generated CRD has no spec schema for %s", version)
	}
	o, err := toMap(served)
	if err != nil {
		return d, err
	}
	n, err := toMap(gen)
	if err != nil {
		return d, err
	}
	if reflect.DeepEqual(o, n) {
		return d, nil
	}
	d.Equal = false
	var out []string
	notLooser("spec", o, n, &out)
	sort.Strings(out)
	d.NotLooser = out
	return d, nil
}

// ReplaceSpec writes generated's spec schema into live's matching version, leaving every other
// part of the CRD untouched. Callers decide with CompareSpec first; it returns false when live does
// not serve the version.
func ReplaceSpec(live, generated *apiextensionsv1.CustomResourceDefinition) bool {
	if live == nil || generated == nil || len(generated.Spec.Versions) == 0 {
		return false
	}
	version := generated.Spec.Versions[0].Name
	gen, ok := specSchemaOf(generated, version)
	if !ok {
		return false
	}
	for i := range live.Spec.Versions {
		v := &live.Spec.Versions[i]
		if v.Name != version || v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		if v.Schema.OpenAPIV3Schema.Properties == nil {
			return false
		}
		v.Schema.OpenAPIV3Schema.Properties["spec"] = *gen.DeepCopy()
		return true
	}
	return false
}

func specSchemaOf(crd *apiextensionsv1.CustomResourceDefinition, version string) (*apiextensionsv1.JSONSchemaProps, bool) {
	if crd == nil {
		return nil, false
	}
	for i := range crd.Spec.Versions {
		v := crd.Spec.Versions[i]
		if v.Name != version || v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		s, ok := v.Schema.OpenAPIV3Schema.Properties["spec"]
		if !ok {
			return nil, false
		}
		return &s, true
	}
	return nil, false
}

func toMap(s *apiextensionsv1.JSONSchemaProps) (map[string]any, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("encoding a spec schema: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decoding a spec schema: %w", err)
	}
	return m, nil
}

// The moves that only ever admit MORE. Each is safe on its own; a node is looser when every one of
// its differences is one of these and every child it shares is looser too.
var (
	// A constraint the generated schema no longer carries: whatever the served one admitted, the
	// generated one admits too.
	droppable = map[string]bool{
		"minLength": true, "maxLength": true, "minimum": true, "maximum": true,
		"exclusiveMinimum": true, "exclusiveMaximum": true, "minItems": true, "maxItems": true,
		"minProperties": true, "maxProperties": true, "pattern": true, "format": true, "enum": true,
		"required": true, "x-kubernetes-validations": true, "not": true, "oneOf": true, "anyOf": true,
		"allOf": true, "uniqueItems": true, "x-kubernetes-list-type": true, "x-kubernetes-list-map-keys": true,
	}
	// A marker the generated schema adds that widens what is admitted or kept, only as `true`.
	addableTrue = map[string]bool{
		"x-kubernetes-preserve-unknown-fields": true, "nullable": true, "x-kubernetes-int-or-string": true,
	}
	// Annotations that admit nothing more or less.
	inert = map[string]bool{"description": true, "title": true, "example": true}
	// Bounds that loosen by FALLING, and by RISING.
	lowerBounds = map[string]bool{"minLength": true, "minimum": true, "minItems": true, "minProperties": true, "exclusiveMinimum": true}
	upperBounds = map[string]bool{"maxLength": true, "maximum": true, "maxItems": true, "maxProperties": true, "exclusiveMaximum": true}
)

// notLooser appends each difference between served (o) and generated (n) at and below path that is
// not a known loosening.
func notLooser(path string, o, n map[string]any, out *[]string) {
	keys := map[string]bool{}
	for k := range o {
		keys[k] = true
	}
	for k := range n {
		keys[k] = true
	}
	for k := range keys {
		if inert[k] {
			continue
		}
		a, inO := o[k]
		b, inN := n[k]
		if inO && inN && reflect.DeepEqual(a, b) {
			continue
		}
		at := path + "." + k
		switch {
		case !inN:
			if droppable[k] {
				continue
			}
			// `type` may go only where the node still admits anything structurally: the #186 shape,
			// `{type: object, preserve}` becoming `{preserve}`, or an int-or-string node.
			if k == "type" && (n["x-kubernetes-preserve-unknown-fields"] == true || n["x-kubernetes-int-or-string"] == true) {
				continue
			}
			*out = append(*out, at+": removed, and not a known loosening")
		case !inO:
			if addableTrue[k] && b == true {
				continue
			}
			// `default` lands here deliberately: a new default changes what an existing object READS
			// as (structural defaulting applies on read), which is not "admits more".
			*out = append(*out, at+": added, and not a known loosening")
		case k == "properties":
			ap, bp := asMap(a), asMap(b)
			for pk := range ap {
				if _, ok := bp[pk]; !ok {
					*out = append(*out, at+"."+pk+": no longer declared (pruning would drop stored values)")
				}
			}
			for pk := range bp {
				if _, ok := ap[pk]; !ok {
					*out = append(*out, at+"."+pk+": newly declared (its values are now validated)")
				}
			}
			for pk, av := range ap {
				if bv, ok := bp[pk]; ok {
					notLooser(at+"."+pk, asMap(av), asMap(bv), out)
				}
			}
		case k == "items" || k == "additionalProperties":
			am, aok := a.(map[string]any)
			bm, bok := b.(map[string]any)
			if aok && bok {
				notLooser(at, am, bm, out)
				continue
			}
			*out = append(*out, at+": changed")
		case k == "enum" && isSubset(asList(a), asList(b)):
		case k == "required" && isSubset(asList(b), asList(a)):
		case lowerBounds[k] && isNum(a) && isNum(b) && num(b) <= num(a):
		case upperBounds[k] && isNum(a) && isNum(b) && num(b) >= num(a):
		default:
			*out = append(*out, at+": changed")
		}
	}
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func isSubset(small, big []any) bool {
	if small == nil || big == nil {
		return false
	}
	for _, s := range small {
		found := false
		for _, b := range big {
			if reflect.DeepEqual(s, b) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func isNum(v any) bool  { _, ok := v.(float64); return ok }
func num(v any) float64 { f, _ := v.(float64); return f }
