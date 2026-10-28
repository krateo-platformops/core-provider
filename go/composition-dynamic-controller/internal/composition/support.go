package composition

import (
	"context"
	"fmt"
	"strings"

	compositionCondition "github.com/krateo-platformops/composition-dynamic-controller/internal/condition"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/dynamic"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/processor"
	"github.com/krateo-platformops/plumbing/maps"

	xcontext "github.com/krateo-platformops/unstructured-runtime/pkg/context"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/statusprojection"
	unstructuredtools "github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured"
	"github.com/krateo-platformops/unstructured-runtime/pkg/tools/unstructured/condition"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicclient "k8s.io/client-go/dynamic"
)

type ManagedResource struct {
	APIVersion string `json:"apiVersion"`
	Resource   string `json:"resource"`
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	Path       string `json:"path"`
}

// setReadyCondition writes cond unless an EQUIVALENT one is already stored, where equivalence
// includes Status (krateo-core-provider#119).
//
// The subtlety that caused the bug: unstructuredtools.GetCondition matches on Type + Reason ONLY.
// So looking up (Ready, Available) returns a condition that carries reason=Available regardless of
// whether its stored status is True or False. The old checks then compared just the Message, which
// means a Ready condition stored as
//
//	{status: False, reason: Available, message: "Composition is up-to-date"}
//
// looked "already current" to setAvaibleStatus and was never rewritten. The contradiction was then
// self-perpetuating: every later reconcile short-circuited on the same comparison, so Ready froze
// while Synced kept advancing. Observed on krateo-057's Mongodb/sample-mongodb, stuck for 2+ hours
// across many reconciles.
//
// Comparing Status closes that: a stored condition that disagrees with the one being written is
// always repaired. Message is still compared so a genuinely unchanged condition is not rewritten
// every reconcile (which would churn lastTransitionTime and the resourceVersion).
func setReadyCondition(mg *unstructured.Unstructured, cond metav1.Condition, message string, force bool) error {
	cond.Message = message
	if !force {
		if cur := unstructuredtools.GetCondition(mg, cond.Type, cond.Reason); cur != nil &&
			cur.Message == message && cur.Status == cond.Status {
			return nil
		}
	}
	if err := unstructuredtools.SetConditions(mg, cond); err != nil {
		return fmt.Errorf("setting condition: %w", err)
	}
	return nil
}

func setAvaibleStatus(mg *unstructured.Unstructured, message string, force bool) error {
	return setReadyCondition(mg, condition.Available(), message, force)
}

// Also Type=Ready (reason ReconcileGracefullyPaused), so it could hold the same contradiction.
func setGracefullyPausedCondition(mg *unstructured.Unstructured, force bool) error {
	return setReadyCondition(mg, compositionCondition.ReconcileGracefullyPaused(), "Composition is gracefully paused.", force)
}

// Creating and Unavailable carried the identical defect — #119 names only setAvaibleStatus, but all
// three shared the same Type+Reason+Message comparison, so any of them could leave a contradictory
// status in place. Routed through the same helper rather than fixing one and leaving two.
func setCreatingStatus(mg *unstructured.Unstructured, message string, force bool) error {
	return setReadyCondition(mg, condition.Creating(), message, force)
}

func setUnavailableStatus(mg *unstructured.Unstructured, message string, force bool) error {
	return setReadyCondition(mg, condition.Unavailable(), message, force)
}

// setZeroEvidenceStatus stamps Ready=Unknown/ZeroEvidence: the composition's health could not be
// assessed at all (#165). Routed through the same helper as the other three so it cannot reintroduce
// #119's contradictory-status comparison.
func setZeroEvidenceStatus(mg *unstructured.Unstructured, message string, force bool) error {
	return setReadyCondition(mg, condition.ZeroEvidence(), message, force)
}

type ConditionType string

const (
	ConditionTypeAvailable                 ConditionType = "Available"
	ConditionTypeCreating                  ConditionType = "Creating"
	ConditionTypeUnavailable               ConditionType = "Unavailable"
	ConditionTypeReconcileGracefullyPaused ConditionType = "ReconcileGracefullyPaused"
)

// convergedConditionType picks the conditionType for a reconcile that has just converged, from
// whether the composition is gracefully paused.
//
// Extracted as a named function rather than written inline in Update for one reason: Update drives
// helm and a live dynamic client and cannot be unit-tested, so an inline conditional there is only
// reachable by a source-level guard — and a guard of that kind asserts that the paused constant is
// MENTIONED, not that it can be produced. Proven, not assumed: an earlier version of this change
// kept the conditional inline and a mutation that made it `if paused && false` SURVIVED the guard,
// because the identifier was still sitting there in dead code. This function is the smallest unit
// that makes the decision itself falsifiable.
//
// Both effects of a pause hang off this return value: setStatus routes
// ConditionTypeReconcileGracefullyPaused to setGracefullyPausedCondition AND uses it to skip the
// statusDataTemplate projection. Before #232 nothing ever passed it, so neither happened.
func convergedConditionType(paused bool) ConditionType {
	if paused {
		return ConditionTypeReconcileGracefullyPaused
	}
	return ConditionTypeAvailable
}

type statusManagerOpts struct {
	force           bool
	chartURL        string
	chartVersion    string
	releaseStatus   string
	releaseRevision int
	releaseName     string
	resources       []processor.MinimalMetadata
	previousDigest  string
	digest          string
	message         string
	conditionType   ConditionType
}

func (h *handler) setStatus(ctx context.Context, mg *unstructured.Unstructured, opts *statusManagerOpts) error {
	if opts == nil {
		return fmt.Errorf("status manager options are nil")
	}

	if len(opts.resources) > 0 {
		managed, err := h.populateManagedResources(opts.resources, mg.GetNamespace())
		if err != nil {
			return fmt.Errorf("populating managed resources: %w", err)
		}

		setManagedResources(mg, managed)
	}

	err := maps.SetNestedField(mg.Object, opts.previousDigest, "status", "previousDigest")
	if err != nil {
		return fmt.Errorf("setting previous digest in status: %w", err)
	}

	err = maps.SetNestedField(mg.Object, opts.digest, "status", "digest")
	if err != nil {
		return fmt.Errorf("setting digest in status: %w", err)
	}

	err = maps.SetNestedField(mg.Object, opts.chartURL, "status", "helmChartUrl")
	if err != nil {
		return fmt.Errorf("setting chart URL in status: %w", err)
	}

	err = maps.SetNestedField(mg.Object, opts.chartVersion, "status", "helmChartVersion")
	if err != nil {
		return fmt.Errorf("setting chart version in status: %w", err)
	}

	// Project the declarative status fields (statusDataTemplate) and observedGeneration.
	// Built-in sources self/spec/status come from mg; helm is built here. Projection is
	// degrade-only: a bad mapping affects just its field, never the baseline status.
	if len(h.statusDataTemplate) > 0 && opts.conditionType != ConditionTypeReconcileGracefullyPaused {
		resolved := map[string]any{
			"helm": map[string]any{
				"url":      opts.chartURL,
				"version":  opts.chartVersion,
				"status":   opts.releaseStatus,
				"revision": int64(opts.releaseRevision),
				"name":     opts.releaseName,
			},
		}
		// Resolve the apiRef (RESTAction via snowplow) into the ".api" source. Degrade-only:
		// a resolution failure leaves ".api" absent so api-dependent mappings skip, while
		// built-in and helm-sourced fields still project.
		if h.apiResolver != nil {
			if api, aerr := h.apiResolver.Resolve(ctx, mg); aerr != nil {
				xcontext.Logger(ctx).Info("status projection: apiRef resolution failed; .api source unavailable", "error", aerr.Error())
			} else if api != nil {
				resolved["api"] = api
			}
		}
		if perr := statusprojection.Project(ctx, mg, resolved, h.statusDataTemplate); perr != nil {
			xcontext.Logger(ctx).Info("status projection: some fields could not be set", "error", perr.Error())
		}
	}
	if gerr := statusprojection.SetObservedGeneration(mg); gerr != nil {
		return fmt.Errorf("setting observedGeneration: %w", gerr)
	}

	switch opts.conditionType {
	case ConditionTypeReconcileGracefullyPaused:
		return setGracefullyPausedCondition(mg, opts.force)
	case ConditionTypeAvailable:
		return h.deriveReadyFromHealth(ctx, mg, opts)
	case ConditionTypeCreating:
		return setCreatingStatus(mg, opts.message, opts.force)
	case ConditionTypeUnavailable:
		return setUnavailableStatus(mg, opts.message, opts.force)
	}
	return fmt.Errorf("unknown condition type: %s", opts.conditionType)
}

// deriveReadyFromHealth stamps the Ready condition from managed-child health (krateo-core-provider#72,
// #96). It folds two signals via resolveReady:
//  1. A blueprint's statusDataTemplate projection .status.health.ready (a health RESTAction verdict),
//     when present — the author's own view of health.
//  2. The generic rollup over .status.managed (child workload/Job/nested-composition readiness).
//
// A projected ready=false is honored, but a projected ready=true can NOT override a positively-
// observed sick managed workload: a Deployment/StatefulSet/DaemonSet with unready replicas keeps the
// composition Ready=False so `deps:` ordering waits for it to actually serve, not just be applied
// (#96). It is fail-safe: if children cannot be read (client build fails, a child GET is Forbidden,
// etc.) the rollup degrades to Available, so it can only surface positively-observed failures.
func (h *handler) deriveReadyFromHealth(ctx context.Context, mg *unstructured.Unstructured, opts *statusManagerOpts) error {
	// Default the rollup to healthy so a missing/unusable client never regresses a working
	// composition (fail-safe toward Available).
	v := healthVerdict{ready: true, reason: "Available", message: opts.message}
	if h.kubeconfig != nil {
		if dyn, err := dynamicclient.NewForConfig(h.kubeconfig); err == nil {
			v = h.rollupManagedChildren(ctx, dyn, mg)
		}
	}

	return stampReady(mg, resolveReady(readProjectedHealth(mg), opts.message, v), opts.force)
}

// stampReady writes the Ready condition a readyOutcome calls for.
//
// Named and tested rather than inlined in deriveReadyFromHealth, because that function needs a
// kubeconfig to reach the rollup and so could not be exercised in a unit test — which meant the
// ROUTING was untested while every setter it routes to was tested individually. Dropping the
// ZeroEvidence arm failed nothing (found by mutation while shipping #165).
//
// That gap is dangerous here specifically because the default arm stamps AVAILABLE. A reason added
// to resolveReady without a case here does not fail loudly or stamp nothing; it reports healthy.
// Which is the exact failure #165 is about, arriving in the fix for it.
func stampReady(mg *unstructured.Unstructured, out readyOutcome, force bool) error {
	switch out.reason {
	case "Unavailable":
		return setUnavailableStatus(mg, out.message, force)
	case "Creating":
		return setCreatingStatus(mg, out.message, force)
	case "ZeroEvidence":
		return setZeroEvidenceStatus(mg, out.message, force)
	case "Available":
		return setAvaibleStatus(mg, out.message, force)
	}
	// Available is an EXPLICIT case above and this is an error, not a fallback. The arm used to be
	// `default: setAvaibleStatus`, which meant a reason added to resolveReady without a case here
	// reported the composition HEALTHY — silently, and in the one direction nobody audits. That is
	// the failure #165 exists to fix, reachable through the routing that implements it.
	//
	// "Not in a recognised bad branch" is not evidence of good. Erroring matches this file's own
	// sibling, setStatus, which ends `return fmt.Errorf("unknown condition type: %s", ...)` rather
	// than assuming. The cost of being wrong here is asymmetric: a loud error on an unknown reason
	// is a bug report, a quiet Available is an outage nobody is told about.
	return fmt.Errorf("unknown ready reason: %q", out.reason)
}

// readProjectedHealth reads a blueprint's own declared health out of .status.health. Only a
// blueprint's health RESTAction projection writes there (the generic rollup does not persist it),
// so presence unambiguously means "author-defined health".
//
// `ready` accepts bool or string so the statusDataTemplate expression is free to produce either —
// a jq projection naturally yields a string.
//
// `reason` and `message` are read unconditionally and are free-form here; resolveReady decides
// what, if anything, they mean. An absent reason is the pre-#234 world and must stay inert.
func readProjectedHealth(mg *unstructured.Unstructured) projectedHealth {
	var p projectedHealth
	if b, found, _ := unstructured.NestedBool(mg.Object, "status", "health", "ready"); found {
		p.ready, p.present = b, true
	} else if s, found, _ := unstructured.NestedString(mg.Object, "status", "health", "ready"); found {
		p.ready, p.present = strings.EqualFold(s, "true"), true
	}
	p.reason, _, _ = unstructured.NestedString(mg.Object, "status", "health", "reason")
	p.message, _, _ = unstructured.NestedString(mg.Object, "status", "health", "message")
	return p
}

func setManagedResources(mg *unstructured.Unstructured, managed []any) {
	status := mg.Object["status"]
	if status == nil {
		status = map[string]interface{}{}
	}
	mapstatus := status.(map[string]interface{})

	mapstatus["managed"] = managed
	mg.Object["status"] = mapstatus
}

func (h *handler) populateManagedResources(resources []processor.MinimalMetadata, defaultNamespace string) ([]any, error) {
	var managed []interface{}
	for _, ref := range resources {
		gvr, err := h.pluralizer.GVKtoGVR(schema.FromAPIVersionAndKind(ref.GetAPIVersion(), ref.GetKind()))
		if err != nil {
			return nil, fmt.Errorf("getting GVR for %s/%s with name %s and namespace %s: %w", ref.GetAPIVersion(), ref.GetKind(), ref.GetName(), ref.GetNamespace(), err)
		}

		gvk := schema.FromAPIVersionAndKind(ref.GetAPIVersion(), ref.GetKind())
		isNamespaced, err := dynamic.IsNamespaced(h.mapper, gvk)
		if err != nil {
			return nil, fmt.Errorf("getting REST mapping for %s: %w", gvk.String(), err)
		}
		if !isNamespaced {
			ref.SetNamespace("")
		} else if ref.GetNamespace() == "" {
			// A namespaced child rendered without an explicit metadata.namespace inherits the
			// composition's (helm release) namespace. Persist that here so the #74 child-health
			// rollup can GET it namespaced: childHealth (childhealth.go) issues a CLUSTER-SCOPED
			// GET when the stored namespace is empty, which 404s for a namespaced resource -> the
			// child is counted childConverging -> the parent wedges Ready=False forever even though
			// the child is healthy. Observed on krateo-observability, whose chart omits
			// metadata.namespace on its secrets/configmaps/services/deployment, so it never went
			// Ready and its dependent (clickhouse-mcp-server) was never emitted by the umbrella.
			ref.SetNamespace(defaultNamespace)
		}

		buildpath := func() string {
			prefix := "/apis/" + gvr.Group + "/" + gvr.Version
			// Core group resources
			if len(gvr.Group) == 0 {
				prefix = "/api/" + gvr.Version
			}

			suffix := "/namespaces/" + ref.GetNamespace() + "/" + gvr.Resource + "/" + ref.GetName()
			// Cluster scoped resources
			if len(ref.GetNamespace()) == 0 {
				suffix = "/" + gvr.Resource + "/" + ref.GetName()
			}
			if len(gvr.Group) == 0 {
				return prefix + suffix
			}
			return prefix + suffix
		}

		// Store the managed ref as a JSON-native map, NOT the typed ManagedResource struct.
		// mg.Object["status"] is deep-copied by runtime.DeepCopyJSONValue on other reconcile
		// paths (e.g. the observe/converter path), which panics on any non-JSON type with
		// "cannot deep copy composition.ManagedResource". ToUnstructured honours the struct's
		// json tags, so the serialized status shape is identical. (status_update.go worked
		// around the symptom on the update path only; this fixes the source.)
		u, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&ManagedResource{
			APIVersion: ref.GetAPIVersion(),
			Resource:   gvr.Resource,
			Name:       ref.GetName(),
			Namespace:  ref.GetNamespace(),
			Path:       buildpath(),
		})
		if err != nil {
			return nil, fmt.Errorf("converting managed resource %s to unstructured: %w", ref.GetName(), err)
		}
		managed = append(managed, u)
	}

	return managed, nil
}
