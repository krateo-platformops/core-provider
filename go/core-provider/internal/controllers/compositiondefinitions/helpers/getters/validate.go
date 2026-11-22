package getters

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// core-provider#614. A version migration rewrites only an instance's version label, and the apiserver
// RATCHETS CRD validation on update: a write that leaves an invalid field unchanged is admitted. So an
// instance can migrate onto a version whose schema refuses a value it already holds (a boolean leaf
// that became integer-or-string, for example), and nothing says so until a user edit that touches the
// field is refused (#568 child 8, inferredmigration_568_envtest_test.go).
//
// ValidateOwnedInstances checks each instance against the served version WITHOUT ratcheting, by
// asking the apiserver to CREATE a copy as a server-side dry run: a create has no old object, so
// every field is validated, and dryRun=All persists nothing.
//
// THE COPY GETS A GENERATED NAME. A dry-run create under the instance's own name reaches storage's
// existence check after validation, so a VALID instance answers AlreadyExists, which says nothing
// about its values (measured in validate_614_envtest_test.go). generateName keeps every outcome
// about the values. The prefix is short so a CREATE-time name-length policy (core-provider's
// compositions-max-name-length-policy allows 44) cannot be what refuses the copy.

// ValidationNamePrefix is the generateName of the dry-run copies.
const ValidationNamePrefix = "krateo-validate-"

// RefusedInstance is an instance the served version refuses, with the spec fields it names.
type RefusedInstance struct {
	Namespace string
	Name      string
	// Fields are the apiserver's Invalid causes under spec, sorted and de-duplicated.
	Fields []string
}

// UncheckedInstance is an instance whose values could not be judged: the dry run failed for a reason
// that is not a spec validation error (an admission policy, a webhook, the apiserver unreachable).
// It is never counted as valid.
type UncheckedInstance struct {
	Namespace string
	Name      string
	Err       string
}

// ValidationResult is one check of every instance an owner has on the served version.
type ValidationResult struct {
	Version   string
	Checked   int
	Refused   []RefusedInstance
	Unchecked []UncheckedInstance
}

// ValidateOwnedInstances dry-run creates a copy of every instance OWNED BY owner and labelled with
// gvr.Version, through gvr, and reports which ones that version refuses. A list failure is returned
// as an error: with no list there is nothing to judge, and "zero refused" would be a false all-clear.
func ValidateOwnedInstances(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, owner DefinitionRef) (ValidationResult, error) {
	res := ValidationResult{Version: gvr.Version}
	ul, err := GetOwnedCompositionsByVersionLabel(ctx, dyn, gvr, gvr.Version, owner)
	if err != nil {
		return res, fmt.Errorf("listing instances on %s: %w", gvr.Version, err)
	}
	for i := range ul.Items {
		u := &ul.Items[i]
		res.Checked++
		_, err := dyn.Resource(gvr).Namespace(u.GetNamespace()).Create(ctx, validationCopy(u), metav1.CreateOptions{DryRun: []string{metav1.DryRunAll}})
		if err == nil {
			continue
		}
		if fields := specCauses(err); len(fields) > 0 {
			res.Refused = append(res.Refused, RefusedInstance{Namespace: u.GetNamespace(), Name: u.GetName(), Fields: fields})
			continue
		}
		res.Unchecked = append(res.Unchecked, UncheckedInstance{Namespace: u.GetNamespace(), Name: u.GetName(), Err: err.Error()})
	}
	return res, nil
}

// validationCopy is the instance as a fresh create: its apiVersion, kind, labels, annotations and
// spec, under a generated name in its namespace. Identity, ownership, finalizers, managed fields and
// status are not carried: a create would refuse or ignore them, and they are not the values in
// question.
func validationCopy(u *unstructured.Unstructured) *unstructured.Unstructured {
	c := &unstructured.Unstructured{Object: map[string]any{}}
	c.SetAPIVersion(u.GetAPIVersion())
	c.SetKind(u.GetKind())
	c.SetNamespace(u.GetNamespace())
	c.SetGenerateName(ValidationNamePrefix)
	c.SetLabels(u.GetLabels())
	c.SetAnnotations(u.GetAnnotations())
	if spec, ok := u.Object["spec"]; ok {
		c.Object["spec"] = runtimeDeepCopy(spec)
	}
	return c
}

func runtimeDeepCopy(v any) any {
	w := &unstructured.Unstructured{Object: map[string]any{"v": v}}
	return w.DeepCopy().Object["v"]
}

// specCauses returns the spec field paths an Invalid error names. Any other error, and an Invalid one
// naming no spec field (an admission policy's denial, say), returns nil: those are not a verdict on
// the values.
func specCauses(err error) []string {
	if !apierrors.IsInvalid(err) {
		return nil
	}
	var se *apierrors.StatusError
	if !errors.As(err, &se) {
		return nil
	}
	details := se.ErrStatus.Details
	if details == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range details.Causes {
		f := c.Field
		if f != "spec" && !strings.HasPrefix(f, "spec.") && !strings.HasPrefix(f, "spec[") {
			continue
		}
		if !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}
