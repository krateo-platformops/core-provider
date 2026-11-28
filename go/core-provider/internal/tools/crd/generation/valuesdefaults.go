package generation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiservervalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	k8sjson "k8s.io/apimachinery/pkg/util/json"
)

// DefaultDifference is one place where the chart's values.schema.json and its values.yaml disagree
// about a default (#686), or where values.yaml's value could not become a CRD default.
type DefaultDifference struct {
	// Path is the dotted values path ("restdefinitions.quota.enabled"); "[]" marks an array's items
	// and ".*" a map's values.
	Path string
	// Schema is the schema's default as JSON, "" when it declares none.
	Schema string
	// Values is values.yaml's value as JSON, "" when it has no key at Path.
	Values string
	// Invalid is set when values.yaml has a value at Path that the schema's own constraints refuse,
	// so it was NOT made a default (the apiserver would refuse the whole CRD).
	Invalid bool
}

// String is the form the condition message uses.
func (d DefaultDifference) String() string {
	switch {
	case d.Invalid:
		return fmt.Sprintf("%s: values.yaml=%s is not valid against the schema, so it is not a default", d.Path, d.Values)
	case d.Values == "":
		return d.Path + ": schema default, no values.yaml key"
	default:
		return fmt.Sprintf("%s: schema=%s values.yaml=%s", d.Path, d.Schema, d.Values)
	}
}

// ApplyValuesDefaults makes the generated CRD default from the chart's values.yaml instead of from
// its values.schema.json (#686). It rewrites every `default` in the spec schema of every version of
// crd, in place, and returns the values.yaml values it could not use as a default.
//
// Why. helm never applies a values.schema.json default: a `helm install` of the chart takes its
// defaults from values.yaml. crdgen puts the schema's defaults in the CRD, and `default: {}` on every
// object without `required`, so the apiserver materializes an omitted object and with it every nested
// default. Where a schema default disagrees with values.yaml, a Composition and a plain `helm
// install` of the same chart behaved differently, silently (nova-kog 0.1.10
// restdefinitions.quota.enabled: schema false, values.yaml true). The decision (Diego, 2026-11-28) is
// that the CRD follows values.yaml, so the two install paths agree by construction.
//
// The rules, per schema property at a path:
//  1. values.yaml has a scalar, list or map at the path: the CRD default is that value, whatever the
//     schema's `default` says.
//  2. values.yaml has no key (or null, which helm treats as unset): there is NO default, a schema
//     `default` included, because helm would not apply it either. This holds below arrays and maps
//     too: the defaults inside `items` and `additionalProperties` come from no values.yaml path.
//  3. A values.yaml key with no schema property is left alone: the CRD cannot default what it does
//     not declare.
//  4. An object that declares properties keeps crdgen's `default: {}` ONLY when a descendant
//     received a default from values.yaml: the `{}` exists to materialize those nested defaults, so
//     an object with none gets no `default`. This function never adds a `default: {}`: where crdgen
//     withheld it (an object with `required`), it stays withheld.
//  5. CRD defaulting applies to omitted fields only, so values a composition already stores do not
//     change. (The apiserver also applies defaults when it READS, so a served schema is not rewritten
//     in place: see specdrift.go, which keeps a served version whose defaults differ.)
//
// A values.yaml value the schema's constraints refuse (a type mismatch, an enum, a bound) is not made
// a default and is returned with Invalid set: the apiserver validates defaults, and one bad default
// would refuse the whole CRD.
func ApplyValuesDefaults(crd *apiextensionsv1.CustomResourceDefinition, values map[string]any) []DefaultDifference {
	var invalid []DefaultDifference
	if crd == nil {
		return nil
	}
	for i := range crd.Spec.Versions {
		v := &crd.Spec.Versions[i]
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		spec, ok := v.Schema.OpenAPIV3Schema.Properties["spec"]
		if !ok {
			continue
		}
		var root any
		if values != nil {
			root = values
		}
		applyNode(&spec, root, values != nil, "", &invalid)
		v.Schema.OpenAPIV3Schema.Properties["spec"] = spec
	}
	sort.Slice(invalid, func(i, j int) bool { return invalid[i].Path < invalid[j].Path })
	return invalid
}

// applyNode rewrites n's default (and its descendants') from val, the values.yaml value at path, and
// reports whether n or a descendant received one.
func applyNode(n *apiextensionsv1.JSONSchemaProps, val any, has bool, path string, invalid *[]DefaultDifference) bool {
	if has && val == nil {
		has = false // `key: null` unsets the key in helm
	}
	if len(n.Properties) > 0 {
		hadDefault := n.Default != nil
		m, _ := val.(map[string]any)
		anyDefault := false
		for name, child := range n.Properties {
			cv, ch := m[name]
			if applyNode(&child, cv, has && ch, joinPath(path, name), invalid) {
				anyDefault = true
			}
			n.Properties[name] = child
		}
		n.Default = nil
		if anyDefault && hadDefault && len(n.Required) == 0 {
			n.Default = &apiextensionsv1.JSON{Raw: []byte("{}")}
		}
		return anyDefault
	}
	// A leaf: scalar, list, or a map with no declared properties. Nothing below it has a values path.
	stripDefaults(n.Items)
	stripDefaults(n.AdditionalProperties)
	n.Default = nil
	if !has {
		return false
	}
	if err := validAgainst(n, val); err != nil {
		*invalid = append(*invalid, DefaultDifference{Path: path, Values: jsonOf(val), Invalid: true})
		return false
	}
	raw, err := json.Marshal(val)
	if err != nil {
		*invalid = append(*invalid, DefaultDifference{Path: path, Values: jsonOf(val), Invalid: true})
		return false
	}
	n.Default = &apiextensionsv1.JSON{Raw: raw}
	return true
}

// stripDefaults removes every `default` below an items / additionalProperties schema.
func stripDefaults(x any) {
	switch s := x.(type) {
	case *apiextensionsv1.JSONSchemaPropsOrArray:
		if s == nil {
			return
		}
		stripDefaults(s.Schema)
		for i := range s.JSONSchemas {
			stripDefaults(&s.JSONSchemas[i])
		}
	case *apiextensionsv1.JSONSchemaPropsOrBool:
		if s == nil {
			return
		}
		stripDefaults(s.Schema)
	case *apiextensionsv1.JSONSchemaProps:
		if s == nil {
			return
		}
		s.Default = nil
		for k, p := range s.Properties {
			stripDefaults(&p)
			s.Properties[k] = p
		}
		stripDefaults(s.Items)
		stripDefaults(s.AdditionalProperties)
	}
}

// validAgainst checks val against n's own constraints (n's default is already cleared).
func validAgainst(n *apiextensionsv1.JSONSchemaProps, val any) error {
	var internal apiextensions.JSONSchemaProps
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(n, &internal, nil); err != nil {
		return err
	}
	validator, _, err := apiservervalidation.NewSchemaValidator(&internal)
	if err != nil {
		return err
	}
	// The validator wants the apiserver's number types (int64, float64), not json.Number.
	b, err := json.Marshal(val)
	if err != nil {
		return err
	}
	var native any
	if err := k8sjson.Unmarshal(b, &native); err != nil {
		return err
	}
	if errs := apiservervalidation.ValidateCustomResource(nil, native, validator); len(errs) > 0 {
		return errs.ToAggregate()
	}
	return nil
}

// SchemaDefaultDifferences lists where the chart's values.schema.json declares a `default` that
// values.yaml does not agree with (#686): a different value, or no key at all. It reads the AUTHORED
// schema, not the CRD, because crdgen's own `default: {}` on objects is not something the author
// wrote. Sorted by path.
func SchemaDefaultDifferences(specSchema []byte, values map[string]any) []DefaultDifference {
	dec := json.NewDecoder(bytes.NewReader(specSchema))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil
	}
	var out []DefaultDifference
	var vroot any
	if values != nil {
		vroot = values
	}
	walkAuthored(root, vroot, values != nil, "", &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func walkAuthored(node map[string]any, val any, has bool, path string, out *[]DefaultDifference) {
	if has && val == nil {
		has = false
	}
	if d, ok := node["default"]; ok && path != "" {
		switch {
		case !has:
			*out = append(*out, DefaultDifference{Path: path, Schema: jsonOf(d)})
		case !jsonEqual(d, val):
			*out = append(*out, DefaultDifference{Path: path, Schema: jsonOf(d), Values: jsonOf(val)})
		}
	}
	if props, ok := node["properties"].(map[string]any); ok {
		m, _ := val.(map[string]any)
		for name, c := range props {
			if cm, ok := c.(map[string]any); ok {
				cv, ch := m[name]
				walkAuthored(cm, cv, has && ch, joinPath(path, name), out)
			}
		}
	}
	// Below an array or a map there is no values path: any default there is a schema default with no
	// values.yaml key.
	if items, ok := node["items"].(map[string]any); ok {
		walkAuthored(items, nil, false, path+"[]", out)
	}
	if ap, ok := node["additionalProperties"].(map[string]any); ok {
		walkAuthored(ap, nil, false, path+".*", out)
	}
}

func joinPath(parent, name string) string {
	if parent == "" {
		return name
	}
	return parent + "." + name
}

func jsonOf(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

// jsonEqual compares two decoded JSON values, numbers by value (1 equals 1.0).
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, xv := range x {
			yv, ok := y[k]
			if !ok || !jsonEqual(xv, yv) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		xf, e1 := x.Float64()
		yf, e2 := y.Float64()
		return e1 == nil && e2 == nil && xf == yf
	default:
		return a == b
	}
}

// DescribeDifferences renders at most max differences as "; "-joined text with a "+N more" tail.
func DescribeDifferences(diffs []DefaultDifference, max int) string {
	parts := make([]string, 0, len(diffs))
	for _, d := range diffs {
		parts = append(parts, d.String())
	}
	if len(parts) <= max {
		return strings.Join(parts, "; ")
	}
	return strings.Join(parts[:max], "; ") + fmt.Sprintf("; +%d more", len(parts)-max)
}
