package getters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	"github.com/krateo-platformops/core-provider/internal/tools/retry"

	compositiondefinitionsv1alpha1 "github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/crd/fieldloss"
	"github.com/krateo-platformops/core-provider/internal/tools/deploy"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	compositionRetryAttempts     = 5
	compositionRetryInitialDelay = 250 * time.Millisecond
	compositionRetryMaximumDelay = 2 * time.Second
)

const (
	// PrunedFieldsAnnotation carries the spec subtree a version transition discarded, as JSON,
	// written in the SAME update that discards it (#188).
	//
	// An annotation and not only an Event, because Events expire — typically within the hour —
	// and this is the only remaining copy of the user's data. Annotations are safe to use for
	// it: the apiserver's pruner skips ObjectMeta at the resource root, so a value written here
	// survives the very write that prunes the spec.
	PrunedFieldsAnnotation = "krateo.io/pruned-fields"

	// PrunedFieldsTruncatedAnnotation is set to "true" when the dropped subtree did not fit and
	// PrunedFieldsAnnotation holds only the field paths. The full subtree is always logged.
	PrunedFieldsTruncatedAnnotation = "krateo.io/pruned-fields-truncated"

	// maxPrunedAnnotationBytes keeps the stash well inside the apiserver's ~256KB budget for ALL
	// annotations on an object. Exceeding that budget would make the Update fail, which would
	// turn a feature that exists to make migration safer into the thing that breaks it — so an
	// oversized report degrades to paths, and then to nothing, rather than risking the write.
	maxPrunedAnnotationBytes = 16 * 1024
)

var retryWait = retry.Wait

func GetCompositions(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource) (*unstructured.UnstructuredList, error) {
	return GetCompositionsByVersionLabel(ctx, dyn, gvr, gvr.Version)
}

// DefinitionRef identifies the CompositionDefinition that owns a set of composition instances.
// An empty DefinitionRef means "do not scope by owner" (match instances of any definition).
type DefinitionRef struct {
	Name      string
	Namespace string
}

// GetCompositionsByVersionLabel lists compositions served at gvr whose
// krateo.io/composition-version label equals versionLabel, across ALL owning definitions. The
// LABEL (not the served apiVersion) identifies the owning per-version controller, because the
// vacuum storage version erases the apiVersion; so listing through any served endpoint with a
// given label finds those instances regardless of which version they were written through.
func GetCompositionsByVersionLabel(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, versionLabel string) (*unstructured.UnstructuredList, error) {
	return GetOwnedCompositionsByVersionLabel(ctx, dyn, gvr, versionLabel, DefinitionRef{})
}

// GetOwnedCompositionsByVersionLabel lists compositions served at gvr that carry versionLabel
// in composition-version AND (when owner is non-empty) are owned by that CompositionDefinition.
// Owner scoping matters because one CRD/Kind can be shared by multiple CompositionDefinitions at
// different versions; migration must only ever touch its own definition's instances.
func GetOwnedCompositionsByVersionLabel(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, versionLabel string, owner DefinitionRef) (*unstructured.UnstructuredList, error) {
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	selector, err := compositionSelector(versionLabel, owner)
	if err != nil {
		log.Debug("Error creating label selector", "error", err)
		return nil, fmt.Errorf("error creating label selector: %w", err)
	}

	ul, err := dyn.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		log.Debug("Error listing compositions", "error", err)
		return nil, fmt.Errorf("error listing compositions: %w", err)
	}

	return ul, nil
}

// GetUnaccountedCompositions lists compositions served at gvr whose krateo.io/composition-version
// label does NOT name one of knownVersions — either because the label is absent entirely, or
// because it names a version the CRD no longer carries.
//
// Such instances are invisible to every per-version check: the migration selects by label and
// never rewrites them, and the served-version prune's eligibility test selects by label and so
// never sees them as a reason to keep a version. An unlabelled instance can still be ENCODED at
// a version nobody is accounting for, and pruning that version makes the whole Kind unreadable
// (#189). The policy-absence gap is the common cause: on a cluster where the
// krateo-composition-version MutatingAdmissionPolicy is not installed (Kubernetes < 1.36, or the
// policy simply missing), nothing stamps the label at all.
//
// Listed across ALL owning definitions deliberately: an instance this definition does not own
// still blocks the prune, because the prune edits a CRD that every definition of the Kind shares.
func GetUnaccountedCompositions(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, knownVersions []string) (*unstructured.UnstructuredList, error) {
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	out := &unstructured.UnstructuredList{}
	for _, sel := range unaccountedSelectors(knownVersions) {
		ul, err := dyn.Resource(gvr).Namespace(metav1.NamespaceAll).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			log.Debug("Error listing unaccounted compositions", "selector", sel, "error", err)
			return nil, fmt.Errorf("error listing unaccounted compositions: %w", err)
		}
		out.Items = append(out.Items, ul.Items...)
	}
	return out, nil
}

// unaccountedSelectors returns the label selectors matching instances no per-version check can
// see. Two queries rather than one because "label missing" and "label naming something else" are
// different requirement types and cannot be ORed in a single label selector.
func unaccountedSelectors(knownVersions []string) []string {
	missing := labels.NewSelector()
	if req, err := labels.NewRequirement(deploy.CompositionVersionLabel, selection.DoesNotExist, nil); err == nil {
		missing = missing.Add(*req)
	}
	out := []string{missing.String()}

	// NotIn also matches objects WITHOUT the label, so it would double-count against the query
	// above; it is only added when there is a version set to exclude, and duplicates are harmless
	// here because the caller only counts.
	if len(knownVersions) > 0 {
		unknown := labels.NewSelector()
		exists, errE := labels.NewRequirement(deploy.CompositionVersionLabel, selection.Exists, nil)
		notIn, errN := labels.NewRequirement(deploy.CompositionVersionLabel, selection.NotIn, knownVersions)
		if errE == nil && errN == nil {
			unknown = unknown.Add(*exists, *notIn)
			out = append(out, unknown.String())
		}
	}
	return out
}

// compositionSelector builds the label selector for the composition-version label plus, when
// owner is set, the composition-definition-name/-namespace ownership labels.
func compositionSelector(versionLabel string, owner DefinitionRef) (string, error) {
	selector := labels.NewSelector()
	reqs := []struct{ key, val string }{{deploy.CompositionVersionLabel, versionLabel}}
	if owner.Name != "" {
		reqs = append(reqs, struct{ key, val string }{deploy.CompositionDefinitionNameLabel, owner.Name})
	}
	if owner.Namespace != "" {
		reqs = append(reqs, struct{ key, val string }{deploy.CompositionDefinitionNamespaceLabel, owner.Namespace})
	}
	for _, r := range reqs {
		req, err := labels.NewRequirement(r.key, selection.Equals, []string{r.val})
		if err != nil {
			return "", err
		}
		selector = selector.Add(*req)
	}
	return selector.String(), nil
}

// UpdateCompositionsVersion re-stamps every composition OWNED BY owner and currently labelled
// fromVersion to toVersion, performing BOTH the list and the update THROUGH the gvr endpoint —
// whose served version must be toVersion. This is deliberate: the in-apiserver
// composition-version MutatingAdmissionPolicy stamps the label from the REQUEST's served
// version, so a write through the old endpoint would re-stamp the old version and silently undo
// the migration (leaving the composition selected by the retired old-version controller and
// orphaned by the new one). Writing through the toVersion endpoint makes the policy agree with
// the explicit relabel; the explicit label set in updateCompositionWithRetry also covers
// clusters where the policy is not installed. Owner scoping ensures a version bump of one
// definition never re-stamps another definition's instances that legitimately share the CRD.
func UpdateCompositionsVersion(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, fromVersion, toVersion string, owner DefinitionRef, opts MigrationOptions) error {
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	if fromVersion == toVersion {
		return nil
	}

	ul, err := getCompositionsWithRetry(ctx, dyn, gvr, fromVersion, owner, log)
	if err != nil {
		return fmt.Errorf("error getting compositions: %w", err)
	}

	if len(ul.Items) == 0 {
		log.Debug("No compositions found for the specified GVR and version", "fromVersion", fromVersion, "owner", owner.Name)
		return nil
	}

	sourceGVR := gvr
	sourceGVR.Version = fromVersion

	for _, u := range ul.Items {
		rep := predictLoss(ctx, dyn, sourceGVR, u.GetNamespace(), u.GetName(), opts, log)
		if err := updateCompositionWithRetry(ctx, dyn, gvr, u.GetNamespace(), u.GetName(), toVersion, rep, opts, log); err != nil {
			return err
		}
	}

	return nil
}

// MigrationOptions carries the #188 field-loss detection into the migration. The zero value
// disables detection entirely, so the migration behaves exactly as it did before.
type MigrationOptions struct {
	// TargetSchema is the structural schema of the version being migrated TO. Build it once per
	// transition with fieldloss.Structural; nil disables detection.
	TargetSchema *structuralschema.Structural

	// OnLoss, when set, is called once per instance that will lose data, BEFORE the write that
	// loses it. stashed reports whether the full subtree made it into the annotation. It must
	// not block for long: it runs inline in the reconcile.
	OnLoss func(obj *unstructured.Unstructured, rep fieldloss.Report, stashed bool)
}

// predictLoss reads the instance through the SOURCE version's endpoint and reports what the
// migration write is about to discard.
//
// Why a second read, when the caller already listed these objects: reads through a narrow
// endpoint are pruned too, not only writes (measured against kind v1.36.4 — see the fieldloss
// package comment). The listed copies came through the TARGET endpoint and are therefore
// already missing exactly the fields this is trying to find.
//
// Every failure here is swallowed into "no prediction". Detection is a diagnostic bolted onto a
// migration that must still run: it is better to migrate without a report than to refuse to
// migrate because the report could not be produced. The source version is normally still served
// at this point (the served-version prune runs later in the same reconcile), but it is not
// guaranteed to be — another definition may have pruned it, or the policy-absence gap may mean
// the instance was never labelled at it.
func predictLoss(ctx context.Context, dyn dynamic.Interface, sourceGVR schema.GroupVersionResource,
	namespace, name string, opts MigrationOptions, log logging.Logger) fieldloss.Report {
	if opts.TargetSchema == nil {
		return fieldloss.Report{}
	}

	cur, err := dyn.Resource(sourceGVR).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		log.Debug("Field-loss prediction skipped: could not read through the source version",
			"composition", compositionKey(namespace, name), "sourceGVR", sourceGVR.String(), "error", err.Error())
		return fieldloss.Report{}
	}

	rep, err := fieldloss.Predict(cur.Object, opts.TargetSchema)
	if err != nil {
		log.Debug("Field-loss prediction failed",
			"composition", compositionKey(namespace, name), "error", err.Error())
		return fieldloss.Report{}
	}
	return rep
}

// stashPrunedFields records the doomed subtree in annotations on the object about to be written.
// Returns whether the FULL subtree was stored (false means only the paths were, or nothing).
//
// The write itself must never fail because of this, so an oversized payload degrades instead of
// growing: full subtree -> paths only -> nothing.
func stashPrunedFields(u *unstructured.Unstructured, rep fieldloss.Report, log logging.Logger) bool {
	payload, err := rep.JSON()
	if err != nil || payload == "" {
		return false
	}

	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}

	if len(payload) <= maxPrunedAnnotationBytes {
		ann[PrunedFieldsAnnotation] = payload
		delete(ann, PrunedFieldsTruncatedAnnotation)
		u.SetAnnotations(ann)
		return true
	}

	pathsOnly, err := json.Marshal(map[string]any{"paths": rep.Paths})
	if err != nil || len(pathsOnly) > maxPrunedAnnotationBytes {
		log.Warn("Pruned fields too large to record on the object; see the log line above for the full subtree",
			"composition", compositionKey(u.GetNamespace(), u.GetName()), "bytes", len(payload))
		return false
	}
	ann[PrunedFieldsAnnotation] = string(pathsOnly)
	ann[PrunedFieldsTruncatedAnnotation] = "true"
	u.SetAnnotations(ann)
	return false
}

func getCompositionsWithRetry(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, versionLabel string, owner DefinitionRef, log logging.Logger) (*unstructured.UnstructuredList, error) {
	ul, err := retry.Do[*unstructured.UnstructuredList](ctx, retry.Config[*unstructured.UnstructuredList]{
		Attempts:     compositionRetryAttempts,
		InitialDelay: compositionRetryInitialDelay,
		MaximumDelay: compositionRetryMaximumDelay,
		Wait:         retryWait,
		Retryable:    isRetryableCompositionError,
		OnRetry: func(attempt int, nextDelay time.Duration, err error) {
			log.Warn("Retrying composition list", "gvr", gvr.String(), "versionLabel", versionLabel, "attempt", attempt, "next_delay", nextDelay, "error", err)
		},
	}, func(context.Context) (*unstructured.UnstructuredList, error) {
		return GetOwnedCompositionsByVersionLabel(ctx, dyn, gvr, versionLabel, owner)
	})
	if err != nil {
		return nil, fmt.Errorf("error listing compositions after %d attempts: %w", compositionRetryAttempts, err)
	}

	return ul, nil
}

func updateCompositionWithRetry(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, namespace, name, newVersion string, rep fieldloss.Report, opts MigrationOptions, log logging.Logger) error {
	compositionName := compositionKey(namespace, name)
	_, err := retry.Do[struct{}](ctx, retry.Config[struct{}]{
		Attempts:     compositionRetryAttempts,
		InitialDelay: compositionRetryInitialDelay,
		MaximumDelay: compositionRetryMaximumDelay,
		Wait:         retryWait,
		Retryable:    isRetryableCompositionError,
		OnRetry: func(attempt int, nextDelay time.Duration, err error) {
			log.Warn("Retrying composition update", "composition", compositionName, "gvr", gvr.String(), "attempt", attempt, "next_delay", nextDelay, "error", err)
		},
	}, func(context.Context) (struct{}, error) {
		u, err := dyn.Resource(gvr).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			log.Debug("Composition disappeared before version update, skipping", "composition", compositionName, "gvr", gvr.String())
			return struct{}{}, nil
		}
		if err != nil {
			return struct{}{}, fmt.Errorf("error getting composition %s: %w", compositionName, err)
		}

		labelmap, ok, err := unstructured.NestedStringMap(u.Object, "metadata", "labels")
		if err != nil {
			return struct{}{}, fmt.Errorf("error getting labels from composition %s: %w", compositionName, err)
		}
		if !ok {
			labelmap = make(map[string]string)
		}
		if labelmap[deploy.CompositionVersionLabel] == newVersion {
			return struct{}{}, nil
		}

		labelmap[deploy.CompositionVersionLabel] = newVersion
		if err := unstructured.SetNestedStringMap(u.Object, labelmap, "metadata", "labels"); err != nil {
			return struct{}{}, fmt.Errorf("error setting labels on composition %s: %w", compositionName, err)
		}

		// #188: this write is about to discard every spec field the target version's schema does
		// not declare — a narrowing transition, which a rollback usually is and an upgrade can
		// also be. The data cannot be preserved (the version label MEANS "the endpoint this was
		// last written through", so the write has to go through the narrower one), but it can be
		// recorded in the same breath, in metadata the pruner does not touch.
		stashed := false
		if !rep.Empty() {
			stashed = stashPrunedFields(u, rep, log)
			log.Warn("Version transition is discarding spec fields the target version does not declare",
				"composition", compositionName, "toVersion", newVersion, "paths", rep.Paths,
				"recoverFrom", PrunedFieldsAnnotation, "fullSubtreeRecorded", stashed)
			if opts.OnLoss != nil {
				opts.OnLoss(u, rep, stashed)
			}
		}

		if _, err := dyn.Resource(gvr).Namespace(namespace).Update(ctx, u, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsNotFound(err) {
				log.Debug("Composition disappeared during version update, skipping", "composition", compositionName, "gvr", gvr.String())
				return struct{}{}, nil
			}
			return struct{}{}, err
		}

		return struct{}{}, nil
	})
	if err != nil {
		return fmt.Errorf("composition %s update failed after %d attempts: %w", compositionName, compositionRetryAttempts, err)
	}

	return nil
}

func compositionKey(namespace, name string) string {
	if namespace == "" {
		return name
	}

	return namespace + "/" + name
}

func isRetryableCompositionError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	if isNonRetryableCompositionError(err) {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}

	return true
}

func isNonRetryableCompositionError(err error) bool {
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsNotFound(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return true
	}
	if statusErr, ok := err.(*apierrors.StatusError); ok {
		switch statusErr.ErrStatus.Code {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusBadRequest:
			return true
		}
	}

	return false
}

func GetCompositionDefinitions(ctx context.Context, cli client.Client, gk schema.GroupKind) ([]compositiondefinitionsv1alpha1.CompositionDefinition, error) {
	var cdList compositiondefinitionsv1alpha1.CompositionDefinitionList
	err := cli.List(ctx, &cdList, &client.ListOptions{Namespace: metav1.NamespaceAll})
	if err != nil {
		return nil, fmt.Errorf("error listing CompositionDefinitions: %s", err)
	}

	lst := []compositiondefinitionsv1alpha1.CompositionDefinition{}
	for i := range cdList.Items {
		cd := &cdList.Items[i]

		cdgvk := cd.Status.CurrentGVK()
		if cdgvk.Group == gk.Group &&
			cdgvk.Kind == gk.Kind {
			lst = append(lst, *cd)
		}

		// if cd.Status.Managed.Group == gk.Group &&
		// 	cd.Status.Managed.Kind == gk.Kind {
		// 	lst = append(lst, *cd)
		// }
	}

	return lst, nil
}

// GetCompositionDefinitionsWithVersion retrieves CompositionDefinitions that match the specified Composition GVK
func GetCompositionDefinitionsWithVersion(ctx context.Context, cli client.Client, gvk schema.GroupVersionKind) ([]compositiondefinitionsv1alpha1.CompositionDefinition, error) {
	var cdList compositiondefinitionsv1alpha1.CompositionDefinitionList
	err := cli.List(ctx, &cdList, &client.ListOptions{Namespace: metav1.NamespaceAll})
	if err != nil {
		return nil, fmt.Errorf("error listing CompositionDefinitions: %s", err)
	}

	lst := []compositiondefinitionsv1alpha1.CompositionDefinition{}
	for i := range cdList.Items {
		cd := &cdList.Items[i]
		cdgvk := cd.Status.CurrentGVK()
		if cdgvk.Group == gvk.Group && cdgvk.Kind == gvk.Kind && cdgvk.Version == gvk.Version {
			lst = append(lst, *cd)
		}
	}

	return lst, nil
}
