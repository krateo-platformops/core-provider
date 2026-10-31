package composition

import (
	"context"
	"fmt"
	"sort"
	"strings"

	compositionCondition "github.com/krateo-platformops/composition-dynamic-controller/internal/condition"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/dynamic"
	"github.com/krateo-platformops/composition-dynamic-controller/internal/tools/processor"
	"github.com/krateo-platformops/plumbing/kubeutil/event"
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
	// absentChildren, when non-nil, receives the names of managed children that were RECORDED but
	// not found in the cluster. An out-parameter, which is unusual in this file and deliberate: the
	// rollup runs inside setStatus, and Observe needs its result to decide up-to-dateness (#261)
	// without paying for a second pass of child GETs — the alternative was changing setStatus's
	// signature at every caller to return a verdict only one of them wants.
	absentChildren *[]string

	// recordManaged says this reconcile DECODED a release and may therefore write
	// status.managed -- including writing `[]` when the render produced nothing. Observe leaves it
	// false: it passes resources: nil on the assumption that Create/Update already recorded them,
	// so it must not overwrite the list with an empty one (#248).
	recordManaged bool
}

func (h *handler) setStatus(ctx context.Context, mg *unstructured.Unstructured, opts *statusManagerOpts) error {
	if opts == nil {
		return fmt.Errorf("status manager options are nil")
	}

	// Keyed on recordManaged, NOT on len(opts.resources) > 0 (#248). Those differ in exactly one
	// case and it is the case that matters: a reconcile that DID render and found nothing must
	// record `managed: []`, because "evaluated, found nothing" and "never evaluated" are different
	// states and the rollup now treats them differently. Under the old length test an empty render
	// left the key ABSENT, indistinguishable from never having been recorded.
	//
	// It cannot be inferred from nil-versus-empty either: processor.DecodeMinRelease returns a nil
	// slice for an empty render, so the Create/Update paths and Observe would both arrive as nil.
	// Hence an explicit flag — the phases that decode a release set it, Observe does not.
	if opts.recordManaged {
		managed, err := h.populateManagedResources(opts.resources, mg.GetNamespace())
		if err != nil {
			return fmt.Errorf("populating managed resources: %w", err)
		}

		// #268. Read the previous inventory BEFORE overwriting it, so the change can be named.
		previous, _, _ := unstructured.NestedSlice(mg.Object, "status", "managed")
		setManagedResources(mg, managed)
		logManagedSetChange(ctx, previous, mg)

		// #273. Stamp the inventory with the digest of the release it was decoded from. Both callers
		// that set recordManaged take `resources` and `digest` from the SAME DecodeMinRelease call
		// (composition.go:809 in Create, :1013 in Update), so this is that release's digest and not
		// merely a contemporaneous one.
		//
		// Observe compares it against the current release's digest to answer the question nothing
		// else could: is this inventory still the inventory of what is rendered? status.digest tracks
		// the CURRENT release because Observe rewrites it every reconcile; status.managed tracks
		// whatever Create/Update last decoded. Nothing tied the two together, so a render that grew
		// left `managed` a strict subset of reality with every other field agreeing it was fine.
		if err := maps.SetNestedField(mg.Object, opts.digest, "status", "managedDigest"); err != nil {
			return fmt.Errorf("setting managed digest in status: %w", err)
		}
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
		//
		// The degrade is deliberate — a resolver failure must not regress a working composition —
		// but it used to be SILENT: logged at Info, no condition, no event. So "authn was never
		// installed in this target" presented as a composition whose projected status fields were
		// quietly missing, which is indistinguishable from a template that never set them (#251).
		//
		// h.apiResolver is nil exactly when no apiRef is declared, so its non-nilness is the
		// "declared" test. ApiResolved is therefore present whenever an apiRef exists, True or
		// False — never absent-on-success, so a consumer can tell "resolved" from "nobody looked".
		if h.apiResolver != nil {
			api, aerr := h.apiResolver.Resolve(ctx, mg)
			switch {
			case aerr != nil:
				reason, message := apiResolvedFailure(aerr)
				if cerr := setApiResolvedCondition(mg, metav1.ConditionFalse, reason, message); cerr != nil {
					xcontext.Logger(ctx).Info("setting ApiResolved condition", "error", cerr.Error())
				}
				h.eventRecorder.Event(mg, event.Warning(reasonApiRefUnresolved, "Observe", aerr))
				xcontext.Logger(ctx).Info("status projection: apiRef resolution failed; .api source unavailable",
					"reason", reason, "error", aerr.Error())
			case api == nil:
				// The resolver answered without error and without data. Reported rather than
				// treated as success, because an empty .api and a resolved .api project
				// identically — the projection just skips — and that is the ambiguity #251 exists
				// to remove.
				if cerr := setApiResolvedCondition(mg, metav1.ConditionFalse, reasonApiEmpty,
					"apiRef resolved without error but returned no data; .api source is unavailable"); cerr != nil {
					xcontext.Logger(ctx).Info("setting ApiResolved condition", "error", cerr.Error())
				}
			default:
				resolved["api"] = api
				if cerr := setApiResolvedCondition(mg, metav1.ConditionTrue, reasonApiResolved,
					"apiRef resolved; .api projection source is available"); cerr != nil {
					xcontext.Logger(ctx).Info("setting ApiResolved condition", "error", cerr.Error())
				}
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

	if opts.absentChildren != nil {
		*opts.absentChildren = v.absent
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
	// Non-nil by construction, so an empty render serialises as `managed: []` rather than
	// `managed: null` (#248). A nil slice would land as null, which reads back through
	// NestedSlice as found=false — collapsing "evaluated, found nothing" into "never recorded"
	// again, one layer further down than the bug this change fixes.
	managed := []interface{}{}
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
			Path:       managedPath(gvr, isNamespaced, ref.GetNamespace(), ref.GetName()),
		})
		if err != nil {
			return nil, fmt.Errorf("converting managed resource %s to unstructured: %w", ref.GetName(), err)
		}
		managed = append(managed, u)
	}

	return managed, nil
}

// logManagedSetChange emits one Info line when status.managed actually changed, naming what was
// added and removed.
//
// #268. Nothing at default verbosity recorded that the inventory was rewritten. The only line
// near this write is `log.Debug("Composition values updated.")` at composition.go:992, and the cdc
// runs at Info unless --debug is passed — so it is never emitted in production. That cost a real
// misdiagnosis: a peer investigating #261 was told to grep the cdc logs for that string to
// establish whether Update had run, and their counter returned 0 on a composition that had
// demonstrably just healed via Update. They caught it only because the arithmetic contradicted the
// instrument. Their framing: "a blind instrument reporting zero churn is indistinguishable from
// genuinely zero churn."
//
// The event is deliberately narrower than "Update finished", which unstructured-runtime already
// logs as "Successfully requested update of external resource". What nothing reported is that the
// RECORDED SET CHANGED — which is the first question asked about every defect in the
// reports-what-it-recorded-rather-than-what-exists family (#248, #261, #269, #273).
//
// SILENT WHEN UNCHANGED, which is what makes it affordable. Info-level logging on a per-reconcile
// path across 68 controllers is exactly what was trimmed for cost in #175; this fires on a rename,
// a chart bump or a two-phase render catching up, and on nothing else. A composition whose render
// is stable never emits it, however often it reconciles.
func logManagedSetChange(ctx context.Context, previous []any, mg *unstructured.Unstructured) {
	current, _, _ := unstructured.NestedSlice(mg.Object, "status", "managed")

	ids := func(entries []any) map[string]bool {
		out := make(map[string]bool, len(entries))
		for _, e := range entries {
			if ref, ok := e.(map[string]any); ok {
				out[childID(ref)] = true
			}
		}
		return out
	}
	before, after := ids(previous), ids(current)

	var added, removed []string
	for id := range after {
		if !before[id] {
			added = append(added, id)
		}
	}
	for id := range before {
		if !after[id] {
			removed = append(removed, id)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return
	}
	// Sorted so two runs over the same change produce the same line: map iteration order would
	// otherwise make an unchanged situation look like it varied, and a log nobody can diff is
	// nearly as useless as no log.
	sort.Strings(added)
	sort.Strings(removed)

	xcontext.Logger(ctx).Info("Composition managed set changed",
		"added", added, "removed", removed, "total", len(current), "previousTotal", len(previous))
}

// managedPath builds the API path published as status.managed[].path.
//
// KEYED ON isNamespaced, NOT on whether the namespace happens to be non-empty. The normalisation in
// populateManagedResources makes those two equivalent today — it forces the namespace empty for a
// cluster-scoped kind and defaults it for a namespaced one — but only as an invariant established
// twenty lines away and not restated at the point of use. Branching on scope keeps the path correct
// by construction if that normalisation is ever moved, reordered or made conditional, which is how
// the namespaced-child-with-no-namespace bug reached status.managed in the first place (#79).
//
// Designed by the marketing session, which traced the same defect in the 1.x line
// (krateoplatformops/composition-dynamic-controller#189, fixed upstream as #188) and found that
// neither line's fix touched this function — both carried the correct-at-a-distance invariant.
//
// EXTRACTED FROM A CLOSURE so the discriminating case is reachable. As a closure inside
// populateManagedResources it could only be exercised through the normalisation, and no input
// distinguished the old branch from the new one: a cluster-scoped ref has its namespace stripped
// before the path is built, so both agreed everywhere. As a function it can be called with
// isNamespaced=false AND a non-empty namespace — the state the old code got wrong and nothing could
// previously test.
func managedPath(gvr schema.GroupVersionResource, isNamespaced bool, namespace, name string) string {
	prefix := "/apis/" + gvr.Group + "/" + gvr.Version
	if len(gvr.Group) == 0 {
		// Core group resources live under /api, not /apis.
		prefix = "/api/" + gvr.Version
	}
	if !isNamespaced {
		return prefix + "/" + gvr.Resource + "/" + name
	}
	return prefix + "/namespaces/" + namespace + "/" + gvr.Resource + "/" + name
}
