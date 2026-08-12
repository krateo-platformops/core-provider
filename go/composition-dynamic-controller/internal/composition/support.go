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

func setAvaibleStatus(mg *unstructured.Unstructured, message string, force bool) error {
	if !force {
		currentCondition := unstructuredtools.GetCondition(mg, condition.Available().Type, condition.Available().Reason)

		if currentCondition != nil && currentCondition.Message == message {
			return nil
		}
	}

	cond := condition.Available()
	cond.Message = message
	err := unstructuredtools.SetConditions(mg, cond)
	if err != nil {
		return fmt.Errorf("setting condition: %w", err)
	}
	return nil
}

func setGracefullyPausedCondition(mg *unstructured.Unstructured, force bool) error {
	if !force {
		currentCondition := unstructuredtools.GetCondition(mg, compositionCondition.ReconcileGracefullyPaused().Type, compositionCondition.ReconcileGracefullyPaused().Reason)

		if currentCondition != nil && currentCondition.Message == "Composition is gracefully paused." {
			return nil
		}
	}

	cond := compositionCondition.ReconcileGracefullyPaused()
	cond.Message = "Composition is gracefully paused."
	err := unstructuredtools.SetConditions(mg, cond)
	if err != nil {
		return fmt.Errorf("setting condition: %w", err)
	}
	return nil
}

func setCreatingStatus(mg *unstructured.Unstructured, message string, force bool) error {
	if !force {
		currentCondition := unstructuredtools.GetCondition(mg, condition.Creating().Type, condition.Creating().Reason)
		if currentCondition != nil && currentCondition.Message == message {
			return nil
		}
	}
	cond := condition.Creating()
	cond.Message = message
	if err := unstructuredtools.SetConditions(mg, cond); err != nil {
		return fmt.Errorf("setting condition: %w", err)
	}
	return nil
}

func setUnavailableStatus(mg *unstructured.Unstructured, message string, force bool) error {
	if !force {
		currentCondition := unstructuredtools.GetCondition(mg, condition.Unavailable().Type, condition.Unavailable().Reason)
		if currentCondition != nil && currentCondition.Message == message {
			return nil
		}
	}
	cond := condition.Unavailable()
	cond.Message = message
	if err := unstructuredtools.SetConditions(mg, cond); err != nil {
		return fmt.Errorf("setting condition: %w", err)
	}
	return nil
}

type ConditionType string

const (
	ConditionTypeAvailable                 ConditionType = "Available"
	ConditionTypeCreating                  ConditionType = "Creating"
	ConditionTypeUnavailable               ConditionType = "Unavailable"
	ConditionTypeReconcileGracefullyPaused ConditionType = "ReconcileGracefullyPaused"
)

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

// deriveReadyFromHealth stamps the Ready condition from managed-child health (krateo-core-provider#72).
// Precedence:
//  1. If a blueprint's statusDataTemplate projected .status.health.ready (a health RESTAction verdict),
//     honor it — the author defines what "healthy" means for their children.
//  2. Otherwise, run the generic rollup over .status.managed.
//
// It is fail-safe: if children cannot be read (client build fails, a child GET is Forbidden, etc.) it
// degrades to Available, never Unavailable, so it can only surface positively-observed failures.
func (h *handler) deriveReadyFromHealth(ctx context.Context, mg *unstructured.Unstructured, opts *statusManagerOpts) error {
	if ready, present := readProjectedHealth(mg); present {
		msg, _, _ := unstructured.NestedString(mg.Object, "status", "health", "message")
		if msg == "" {
			msg = opts.message
		}
		if ready {
			return setAvaibleStatus(mg, msg, opts.force)
		}
		return setUnavailableStatus(mg, msg, opts.force)
	}

	if h.kubeconfig == nil {
		// No client to read children with -> keep today's behavior (Available); never regress.
		return setAvaibleStatus(mg, opts.message, opts.force)
	}
	dyn, err := dynamicclient.NewForConfig(h.kubeconfig)
	if err != nil {
		return setAvaibleStatus(mg, opts.message, opts.force)
	}
	v := h.rollupManagedChildren(ctx, dyn, mg)
	switch v.reason {
	case "Unavailable":
		return setUnavailableStatus(mg, v.message, opts.force)
	case "Creating":
		return setCreatingStatus(mg, v.message, opts.force)
	default:
		return setAvaibleStatus(mg, v.message, opts.force)
	}
}

// readProjectedHealth returns (.status.health.ready, present). Only a blueprint's health RESTAction
// projection sets this field (the generic rollup does not persist it), so its presence unambiguously
// means "author-defined health". Accepts bool or string so the statusDataTemplate expression is free.
func readProjectedHealth(mg *unstructured.Unstructured) (bool, bool) {
	if b, found, _ := unstructured.NestedBool(mg.Object, "status", "health", "ready"); found {
		return b, true
	}
	if s, found, _ := unstructured.NestedString(mg.Object, "status", "health", "ready"); found {
		return strings.EqualFold(s, "true"), true
	}
	return false, false
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
