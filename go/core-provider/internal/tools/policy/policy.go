// Package policy projects the cluster-wide composition-version MutatingAdmissionPolicy
// into the cluster where composition CRDs live.
//
// core-provider hosts no admission webhooks: the krateo.io/composition-version label
// (which per-version listing/migration and safe deletion rely on) is stamped onto
// composition instances by an in-apiserver MutatingAdmissionPolicy. Because instances are
// created in the TARGET cluster, that policy must exist there. For local targets the
// management chart ships it; for remote targets core-provider projects it during
// bootstrap so the label is reliably present without a manual onboarding step.
package policy

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// policyAPIVersion is the GA MutatingAdmissionPolicy API, on by default since
	// Kubernetes 1.36 (the floor for remote targets).
	policyAPIVersion = "admissionregistration.k8s.io/v1"

	// PolicyName and BindingName are the cluster-wide singletons projected into every
	// target cluster that hosts composition CRDs.
	PolicyName  = "krateo-composition-version"
	BindingName = "krateo-composition-version"

	// compositionGroup is the API group of all generated composition CRDs.
	compositionGroup = "composition.krateo.io"

	// versionLabel must match deploy.CompositionVersionLabel. The policy stamps the
	// request's served version onto this label so per-version listing and migration keep
	// working after the vacuum storage version erases the apiVersion.
	//
	// What the value MEANS, recorded here because it is routinely read as something stronger
	// than it is. matchConstraints below matches CREATE *and* UPDATE, so the label is rewritten
	// on every write: it records the served version an instance was LAST WRITTEN THROUGH. It is
	// not the version the instance was created at, not the version it "belongs to", and not
	// evidence of which controller owns it. A CR created at v1-8-70 and touched once after the
	// CRD was re-pointed reads v1-8-71, and nothing on the object distinguishes it from one that
	// was always v1-8-71. That reading cost real time during the krateo-057 incident, where a
	// terminating CR was taken to have been "written at v1-8-71" when it had been written at
	// v1-8-70 and re-stamped.
	//
	// The UPDATE half is load-bearing, not an oversight, so this is not a policy to narrow
	// casually. Every composition-dynamic-controller ListWatcher carries an EXACT-EQUALITY
	// requirement on this label against its own --resource-version flag (cdc main.go,
	// labels.NewRequirement(CompositionVersionLabel, selection.Equals, ...) feeding the
	// ListWatcher's LabelSelector), so an instance whose label does not match exactly is
	// never delivered to that controller at all. Without the re-stamp, a CR pruned during a
	// version bump keeps the old value, no running controller is watching it, and it wedges on
	// its finalizer with nobody to run its Delete.
	//
	// It therefore cannot double as an ORIGIN version. Code that needs to know which version an
	// object was admitted at — core-provider#168, where a delete arriving mid-bump must be told
	// apart from a genuine teardown — needs a separate key written by a CREATE-only sibling
	// policy; this one cannot serve both.
	//
	// Not to be confused with krateo.io/composition-installed-version, a different label that
	// plumbing's LabelsPostRender (helm/utils/postrenderer.go) puts on the rendered CHILDREN of a
	// release from CompositionGVR.Version. That one means "rendered during a reconcile at that
	// GVR"; it is not ownership or liveness either, and core-provider reads it nowhere.
	versionLabel = "krateo.io/composition-version"
)

// objects returns the MutatingAdmissionPolicy and its binding as unstructured objects.
func objects() (*unstructured.Unstructured, *unstructured.Unstructured) {
	p := &unstructured.Unstructured{}
	p.SetAPIVersion(policyAPIVersion)
	p.SetKind("MutatingAdmissionPolicy")
	p.SetName(PolicyName)
	p.Object["spec"] = map[string]any{
		"matchConstraints": map[string]any{
			"matchPolicy": "Exact",
			"resourceRules": []any{map[string]any{
				"apiGroups":   []any{compositionGroup},
				"apiVersions": []any{"*"},
				"operations":  []any{"CREATE", "UPDATE"},
				"resources":   []any{"*"},
			}},
		},
		"failurePolicy":      "Fail",
		"reinvocationPolicy": "Never",
		// JSONPatch, not ApplyConfiguration (#66).
		//
		// ApplyConfiguration performs a structured merge, which requires converting the WHOLE
		// incoming object to its typed form first. That conversion fails whenever any unrelated
		// field holds a value its schema does not accept — and it fails for a value Kubernetes
		// itself considers valid. A composition whose schema types spec.resources.requests.cpu as
		// numeric (because the chart's values.schema.json said `type: number`) but whose value is a
		// Quantity string like "200m" produced:
		//
		//   error applying patch: failed to convert original object to typed object: errors:
		//     .spec.resources.requests.cpu: expected numeric (int or float), got string
		//
		// failurePolicy is Fail, so the apply was DENIED. And because the installer's self-heal
		// re-applies its whole manifest every cycle, one composition with a string cpu wedged the
		// entire umbrella reconcile: Pass B never progressed and the downstream agent fleet never
		// deployed. A label stamp took out an install.
		//
		// A JSON Patch touches only the path it names. Nothing else in the object is parsed,
		// converted or validated by this mutation, so an unrelated field's representation cannot
		// deny the request. Adding a label should never have depended on the rest of the object
		// being typed-convertible.
		//
		// The two-branch expression is required: JSON Patch `add` to /metadata/labels/<key> fails
		// when /metadata/labels does not exist, so an object with no labels at all needs the map
		// created in one step instead.
		//
		// jsonpatch.escapeKey handles the RFC 6901 escaping of "/" in the label key (it becomes
		// ~1). Hand-writing that escape is exactly the sort of thing that works until someone
		// renames the label.
		"mutations": []any{map[string]any{
			"patchType": "JSONPatch",
			"jsonPatch": map[string]any{
				"expression": `has(object.metadata.labels)` +
					` ? [JSONPatch{op: "add", path: "/metadata/labels/" + jsonpatch.escapeKey("` + versionLabel + `"), value: request.requestKind.version}]` +
					` : [JSONPatch{op: "add", path: "/metadata/labels", value: {"` + versionLabel + `": request.requestKind.version}}]`,
			},
		}},
	}

	b := &unstructured.Unstructured{}
	b.SetAPIVersion(policyAPIVersion)
	b.SetKind("MutatingAdmissionPolicyBinding")
	b.SetName(BindingName)
	b.Object["spec"] = map[string]any{"policyName": PolicyName}

	return p, b
}

// EnsureCompositionVersionPolicy guarantees the composition-version policy (and its
// binding) exist in the cluster reached by kube. It is create-if-absent and idempotent:
// an existing policy (e.g. one shipped by a chart or installed by an operator) is left
// untouched, so this never fights another field manager.
//
// The policy is a cluster singleton shared by every composition CRD, so it is
// intentionally never removed on CompositionDefinition deletion.
//
// For REMOTE targets only. On the management cluster the chart owns this object, and
// creating it from here would leave it without helm's ownership metadata — which makes the
// next `helm upgrade` fail with "invalid ownership metadata", wedging the umbrella for the
// sake of an object the chart was going to install anyway. Local callers want
// UpgradeLegacyCompositionVersionPolicy, which never creates.
//
// Requires the GA MutatingAdmissionPolicy API (admissionregistration.k8s.io/v1),
// i.e. Kubernetes >= 1.36 on the target.
func EnsureCompositionVersionPolicy(ctx context.Context, kube client.Client) error {
	p, b := objects()
	// Create the policy before the binding: the binding references it by name.
	for _, o := range []*unstructured.Unstructured{p, b} {
		if err := kube.Create(ctx, o); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
	}

	return upgradeLegacyPolicy(ctx, kube)
}

// UpgradeLegacyCompositionVersionPolicy migrates an existing pre-#66 policy onto the JSON Patch
// form WITHOUT creating anything. It is the local-target entry point.
//
// The management cluster's policy is helm-owned (on krateo-057: release-name `installer`,
// namespace `krateo-system`, via the core-provider subchart of the umbrella), so a chart upgrade
// is what properly repairs it and this is the stopgap for clusters that will not take one soon.
// Creating is deliberately not on this path and core-provider's ClusterRole grants only get and
// update on mutatingadmissionpolicies, so the "never create locally" rule is enforced by RBAC
// rather than only by this comment.
//
// Absent policy is not an error. On a local target the chart installs it; if it is missing, that
// is an incomplete install to be fixed by helm, not by minting an unowned object here.
func UpgradeLegacyCompositionVersionPolicy(ctx context.Context, kube client.Client) error {
	return upgradeLegacyPolicy(ctx, kube)
}

// legacyApplyConfigurationExpression is the exact mutation this package shipped before #66. It is
// reconstructed rather than hardcoded so a rename of versionLabel cannot leave this matching a
// string nobody writes any more.
func legacyApplyConfigurationExpression() string {
	return `Object{ metadata: Object.metadata{ labels: {"` + versionLabel + `": request.requestKind.version} } }`
}

// upgradeLegacyPolicy migrates a policy still carrying the pre-#66 ApplyConfiguration mutation onto
// the JSON Patch form.
//
// Without this the fix reaches nobody. EnsureCompositionVersionPolicy is create-if-absent, and
// every cluster that has ever run core-provider already HAS the policy — so IsAlreadyExists was
// swallowed and the broken mutation stayed forever. Verified on krateo-057: the live policy still
// carried patchType ApplyConfiguration and the legacy expression verbatim, so a composition with a
// Quantity-string cpu would still have been denied on a cluster running the "fixed" version.
//
// Deliberately narrow. It rewrites the mutation ONLY when the existing policy carries exactly one
// mutation that is exactly what we previously shipped. Anything else — a chart-managed policy, an
// operator's edit, an already-migrated policy, extra mutations — is left untouched, which preserves
// the original intent of never fighting another field manager. The equality check is the consent:
// we only overwrite what we can prove is our own previous output.
//
// Note precisely what that consent covers: it is consent to replace THE MUTATION, not to take
// ownership of the object. Only spec.mutations is read and only spec.mutations is written, so on a
// cluster where someone has edited matchConstraints, failurePolicy or reinvocationPolicy on the
// live policy, the result is deliberately a HYBRID — their constraints with our mutation. That is
// the right trade for a stopgap whose job is to stop the admission denials and nothing else; the
// chart is what makes the whole object ours again on the next upgrade. Widening this to replace the
// whole spec would silently revert an operator's deliberate edit.
//
// A conflict (409) is not retried. The write is idempotent and reconcile-driven, so the next pass
// re-reads and re-applies; on the local path the caller logs and moves on, and on the remote path
// the reconcile fails and is retried. Adding RetryOnConflict here would buy nothing a retry does
// not already provide.
func upgradeLegacyPolicy(ctx context.Context, kube client.Client) error {
	existing := &unstructured.Unstructured{}
	existing.SetAPIVersion(policyAPIVersion)
	existing.SetKind("MutatingAdmissionPolicy")

	if err := kube.Get(ctx, client.ObjectKey{Name: PolicyName}, existing); err != nil {
		if apierrors.IsNotFound(err) {
			// Just created by the remote path, removed concurrently, or — on a local target —
			// never installed by the chart. Nothing to migrate in any of those cases.
			return nil
		}
		return err
	}

	muts, found, err := unstructured.NestedSlice(existing.Object, "spec", "mutations")
	if err != nil {
		// A malformed spec.mutations is NOT "nothing to migrate". This function exists to recognise
		// one specific legacy shape, so an object it cannot parse is exactly the case worth hearing
		// about — and returning nil here would make it indistinguishable from a clean no-op, with no
		// log line on the local path either. Report it; both callers decide what it costs.
		return fmt.Errorf("reading spec.mutations of %s: %w", PolicyName, err)
	}
	if !found || len(muts) != 1 {
		return nil
	}
	m, ok := muts[0].(map[string]any)
	if !ok {
		return nil
	}

	patchType, _, _ := unstructured.NestedString(m, "patchType")
	expr, _, _ := unstructured.NestedString(m, "applyConfiguration", "expression")
	if patchType != "ApplyConfiguration" || expr != legacyApplyConfigurationExpression() {
		return nil
	}

	desired, _ := objects()
	desiredMuts, _, err := unstructured.NestedSlice(desired.Object, "spec", "mutations")
	if err != nil {
		return err
	}
	if err := unstructured.SetNestedSlice(existing.Object, desiredMuts, "spec", "mutations"); err != nil {
		return err
	}

	return kube.Update(ctx, existing)
}
