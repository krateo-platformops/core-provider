// Package fieldloss answers one question: if this object were written through the endpoint of
// a given CRD version, what would the apiserver silently throw away?
//
// # Why this is needed at all (#188)
//
// The krateo.io/composition-version label is written by the cluster-wide
// krateo-composition-version MutatingAdmissionPolicy, which fires on CREATE and UPDATE and
// unconditionally sets it to request.requestKind.version. The label therefore MEANS "the served
// version this object was last written through", and the only way to move an instance's label to
// version X is to write the instance through X's endpoint.
//
// When X's schema is narrower than the one the instance was authored at — a rollback to an older
// chart, or an upgrade whose chart dropped a values property — the apiserver prunes every spec
// field X does not declare, on the way in. The migration asked only to set a label and silently
// deleted data.
//
// # Why the apiserver's own pruner and not a hand-rolled diff
//
// Pruning has edge cases (x-kubernetes-preserve-unknown-fields at any depth, embedded resources,
// additionalProperties, arrays of objects) and a reimplementation that disagrees with the
// apiserver in any of them would report a loss that does not happen or miss one that does. This
// calls structuralpruning.PruneWithOptions — the same function the apiserver uses — so the
// prediction agrees with the write by construction.
//
// # One measured, counter-intuitive fact
//
// READS through a narrow endpoint are pruned too, not only writes. Verified against kind
// v1.36.4: an object created at a wide version and fetched through a narrower served version
// comes back already missing the undeclared fields, even though the data is still in storage
// (the "vacuum" storage version preserves it) and still visible through the wide endpoint.
//
// The consequence is load-bearing for callers: the object the migration already holds — listed
// or fetched through the TARGET endpoint — is ALREADY pruned, so diffing it against anything
// finds nothing. Detection must re-read through the SOURCE version's endpoint, which is still
// served at migration time because the served-version prune runs later in the same reconcile.
package fieldloss

import (
	"encoding/json"
	"fmt"
	"sort"

	"k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	structuralpruning "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apimachinery/pkg/runtime"
)

// Report is what a transition would cost one object.
type Report struct {
	// Paths are the dotted field paths the apiserver would drop, as its own pruner names them
	// (array elements appear as parent[i]). Sorted, so log lines and events are stable.
	Paths []string
	// Removed is the subtree that would be lost, shaped like the object it came from, so it can
	// be read back and re-applied by hand. Nil when nothing would be dropped.
	Removed map[string]any
}

// Empty reports whether the transition is lossless for this object.
func (r Report) Empty() bool { return len(r.Paths) == 0 && len(r.Removed) == 0 }

// JSON renders Removed compactly for an annotation or a log field.
func (r Report) JSON() (string, error) {
	if len(r.Removed) == 0 {
		return "", nil
	}
	b, err := json.Marshal(r.Removed)
	if err != nil {
		return "", fmt.Errorf("marshalling pruned fields: %w", err)
	}
	return string(b), nil
}

// Structural converts a CRD version's v1 OpenAPI schema into the structural form the pruner
// needs. Build it once per transition and reuse it across every instance.
func Structural(s *apiextensionsv1.JSONSchemaProps) (*structuralschema.Structural, error) {
	if s == nil {
		return nil, fmt.Errorf("version schema is nil")
	}
	internal := &apiextensions.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(s, internal, nil); err != nil {
		return nil, fmt.Errorf("converting schema: %w", err)
	}
	st, err := structuralschema.NewStructural(internal)
	if err != nil {
		return nil, fmt.Errorf("building structural schema: %w", err)
	}
	return st, nil
}

// Predict reports what writing obj through the version described by target would discard.
//
// obj must be the object as it exists at the SOURCE version — see the package comment: an object
// read through the target endpoint is already pruned and will always predict no loss.
//
// obj is not modified.
func Predict(obj map[string]any, target *structuralschema.Structural) (Report, error) {
	if target == nil {
		return Report{}, fmt.Errorf("target schema is nil")
	}
	if len(obj) == 0 {
		return Report{}, nil
	}

	// runtime.DeepCopyJSON PANICS on a value outside the JSON type set rather than erroring.
	// Unstructured content from the apiserver is always within it, but a panic here would take
	// down the reconcile over a diagnostic — and this whole function exists to make a migration
	// safer, so it must not be the thing that breaks it.
	pruned, err := deepCopyJSON(obj)
	if err != nil {
		return Report{}, err
	}

	opts := structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true}
	paths := structuralpruning.PruneWithOptions(pruned, target, true, opts)
	if len(paths) == 0 {
		return Report{}, nil
	}
	sort.Strings(paths)

	removed, _ := diff(obj, pruned).(map[string]any)
	return Report{Paths: paths, Removed: removed}, nil
}

func deepCopyJSON(obj map[string]any) (out map[string]any, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("object is not deep-copyable JSON: %v", r)
		}
	}()
	return runtime.DeepCopyJSON(obj), nil
}

// diff returns the part of before that survives nowhere in after, shaped like before. It is
// driven by the pruner's own output rather than by a schema walk, so it cannot disagree with it
// about WHAT was dropped — only describe it.
//
// Values that merely changed are not possible here: pruning only ever deletes.
func diff(before, after any) any {
	switch b := before.(type) {
	case map[string]any:
		a, ok := after.(map[string]any)
		if !ok {
			// The whole node is gone.
			return b
		}
		out := map[string]any{}
		for k, bv := range b {
			av, present := a[k]
			if !present {
				out[k] = bv
				continue
			}
			if sub := diff(bv, av); sub != nil {
				out[k] = sub
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		a, ok := after.([]any)
		if !ok {
			return b
		}
		out := make([]any, 0, len(b))
		any_ := false
		for i, bv := range b {
			if i >= len(a) {
				out = append(out, bv)
				any_ = true
				continue
			}
			sub := diff(bv, a[i])
			out = append(out, sub)
			if sub != nil {
				any_ = true
			}
		}
		if !any_ {
			return nil
		}
		return out
	default:
		// Scalars are never partially pruned: if the key survived, the value is unchanged.
		return nil
	}
}
