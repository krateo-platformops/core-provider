package generation

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/itchyny/gojq"
	"github.com/krateo-platformops/plumbing/jqutil"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// StatusField is a decoupled description of one declared status property (from a
// CompositionDefinition's statusDataTemplate), used both to extend the generated status
// schema (InjectStatusFields) and to validate the declarations (ValidateStatusFields).
type StatusField struct {
	ForPath               string
	Expression            string
	Type                  string
	Schema                *apiextensionsv1.JSONSchemaProps
	PreserveUnknownFields bool
}

// reservedStatusFields are the baseline status properties the controller owns. A declared
// forPath must not collide with these on its top segment.
// Every entry here must also be declared in statics/status.schema.json, and vice versa: this list
// stops a blueprint colliding with a baseline field, while the schema is what makes the baseline
// field survive a write at all. A field in one and not the other is a silent defect in whichever
// direction it is missing — out of the schema, the apiserver prunes the controller's own write
// (#293, and observedGeneration before it); out of this list, a blueprint can declare a forPath
// that the controller then overwrites. TestReservedStatusFieldsMatchSchema asserts the two agree.
var reservedStatusFields = map[string]struct{}{
	"helmChartUrl":         {},
	"helmChartVersion":     {},
	"digest":               {},
	"previousDigest":       {},
	"managed":              {},
	"managedDigest":        {},
	"previousManagedCount": {},
	"conditions":           {},
	"observedGeneration":   {},
}

// ValidateStatusFields checks the declared status fields: non-empty forPath, no collision
// with a reserved baseline field, no duplicate forPaths, type/schema/preserveUnknownFields
// mutual exclusion, and that ${ jq } expressions parse. Errors are aggregated.
func ValidateStatusFields(fields []StatusField) error {
	seen := make(map[string]struct{}, len(fields))
	var errs []error
	for _, f := range fields {
		if f.ForPath == "" {
			errs = append(errs, fmt.Errorf("forPath is required"))
			continue
		}
		top := strings.SplitN(f.ForPath, ".", 2)[0]
		if _, bad := reservedStatusFields[top]; bad {
			errs = append(errs, fmt.Errorf("forPath %q collides with reserved baseline status field %q", f.ForPath, top))
		}
		if _, dup := seen[f.ForPath]; dup {
			errs = append(errs, fmt.Errorf("duplicate forPath %q", f.ForPath))
		}
		seen[f.ForPath] = struct{}{}

		if f.Schema != nil && f.PreserveUnknownFields {
			errs = append(errs, fmt.Errorf("forPath %q: schema and preserveUnknownFields are mutually exclusive", f.ForPath))
		}
		if f.Type != "" && (f.Schema != nil || f.PreserveUnknownFields) {
			errs = append(errs, fmt.Errorf("forPath %q: type cannot be combined with schema/preserveUnknownFields", f.ForPath))
		}

		if f.Expression == "" {
			errs = append(errs, fmt.Errorf("forPath %q: expression is required", f.ForPath))
		} else if q, ok := jqutil.MaybeQuery(f.Expression); ok {
			if _, err := gojq.Parse(q); err != nil {
				errs = append(errs, fmt.Errorf("forPath %q: invalid jq expression: %w", f.ForPath, err))
			}
		}
	}
	return errors.Join(errs...)
}

// InjectStatusFields adds each declared field as a (possibly nested) property under the
// status schema of every version of crd, so the controller's writes survive status
// subresource pruning. It is additive and deterministic; call ValidateStatusFields first.
func InjectStatusFields(crd *apiextensionsv1.CustomResourceDefinition, fields []StatusField) error {
	if len(fields) == 0 {
		return nil
	}
	for i := range crd.Spec.Versions {
		v := &crd.Spec.Versions[i]
		if v.Schema == nil || v.Schema.OpenAPIV3Schema == nil {
			continue
		}
		if v.Schema.OpenAPIV3Schema.Properties == nil {
			v.Schema.OpenAPIV3Schema.Properties = map[string]apiextensionsv1.JSONSchemaProps{}
		}
		status := v.Schema.OpenAPIV3Schema.Properties["status"]
		if status.Type == "" {
			status.Type = "object"
		}
		for _, f := range fields {
			updated, err := setPath(status, strings.Split(f.ForPath, "."), leafSchema(f))
			if err != nil {
				return fmt.Errorf("status field %q: %w", f.ForPath, err)
			}
			status = updated
		}
		v.Schema.OpenAPIV3Schema.Properties["status"] = status
	}
	return nil
}

// setPath sets leaf at the dotted path under node, creating intermediate object properties.
// JSONSchemaProps.Properties is a map of values, so the chain is rebuilt and re-assigned.
func setPath(node apiextensionsv1.JSONSchemaProps, segs []string, leaf apiextensionsv1.JSONSchemaProps) (apiextensionsv1.JSONSchemaProps, error) {
	if node.Properties == nil {
		node.Properties = map[string]apiextensionsv1.JSONSchemaProps{}
	}
	if node.Type == "" {
		node.Type = "object"
	}
	seg := segs[0]
	if len(segs) == 1 {
		node.Properties[seg] = leaf
		return node, nil
	}
	child, ok := node.Properties[seg]
	if !ok {
		child = apiextensionsv1.JSONSchemaProps{Type: "object", Properties: map[string]apiextensionsv1.JSONSchemaProps{}}
	}
	if child.Type != "" && child.Type != "object" {
		return node, fmt.Errorf("intermediate segment %q is not an object", seg)
	}
	updated, err := setPath(child, segs[1:], leaf)
	if err != nil {
		return node, err
	}
	node.Properties[seg] = updated
	return node, nil
}

// leafSchema builds the JSON schema for one field, in precedence order:
// Schema → preserve-unknown (explicit, or object/array without a Schema) → scalar Type →
// string fallback.
func leafSchema(f StatusField) apiextensionsv1.JSONSchemaProps {
	if f.Schema != nil {
		return *f.Schema
	}
	if f.PreserveUnknownFields || f.Type == "object" || f.Type == "array" {
		t := true
		return apiextensionsv1.JSONSchemaProps{XPreserveUnknownFields: &t}
	}
	if f.Type != "" {
		return apiextensionsv1.JSONSchemaProps{Type: f.Type}
	}
	return apiextensionsv1.JSONSchemaProps{Type: "string"}
}

// BaselineStatusProperties returns the status property names the baseline schema declares — the
// fields the CONTROLLER owns, as opposed to a blueprint's statusDataTemplate declarations.
//
// Exported so a test can assert the two hand-maintained lists agree: a field the cdc writes but
// this schema does not declare is PRUNED by the apiserver on every write, silently (#293, and
// observedGeneration before it). Nothing else makes them agree.
func BaselineStatusProperties() ([]string, error) {
	var doc struct {
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal([]byte(statusJsonSchema), &doc); err != nil {
		return nil, fmt.Errorf("parsing the baseline status schema: %w", err)
	}
	names := make([]string, 0, len(doc.Properties))
	for n := range doc.Properties {
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// IsReservedStatusField reports whether name is a controller-owned baseline status field, i.e. one
// a blueprint's statusDataTemplate forPath must not collide with.
func IsReservedStatusField(name string) bool {
	_, ok := reservedStatusFields[name]
	return ok
}
