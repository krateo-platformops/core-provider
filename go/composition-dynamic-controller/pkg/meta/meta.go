package meta

import (
	"fmt"
	"time"

	"github.com/krateo-platformops/plumbing/labels"
	"github.com/krateo-platformops/unstructured-runtime/pkg/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/rand"
)

const (
	// Composition label keys re-exported from plumbing/labels — the single shared source of truth with
	// core-provider (compile-time identical, no cross-repo drift). If they disagreed, owner-scoped
	// version migration would silently select nothing and orphan composition instances.
	CompositionDefinitionNameLabel      = labels.CompositionDefinitionNameLabel
	CompositionDefinitionNamespaceLabel = labels.CompositionDefinitionNamespaceLabel
	CompositionDefinitionGroupLabel     = labels.CompositionDefinitionGroupLabel
	CompositionDefinitionVersionLabel   = labels.CompositionDefinitionVersionLabel
	CompositionDefinitionResourceLabel  = labels.CompositionDefinitionResourceLabel
	CompositionVersionLabel             = labels.CompositionVersionLabel

	// ReleaseNameLabel is the label used to identify the release name of a Helm chart that can be different from the name of the resource.
	ReleaseNameLabel = "krateo.io/release-name"

	// AnnotationKeyReconciliationGracefullyPaused is the key in the annotations map
	// that indicates whether the reconciliation of the resource is gracefully paused.
	AnnotationKeyReconciliationGracefullyPaused = "krateo.io/gracefully-paused"

	// AnnotationKeyReconciliationGracefullyPausedTime is the key in the annotations map
	// that indicates the time when the reconciliation was gracefully paused.
	// This is used to track how long the resource has been paused.
	AnnotationKeyReconciliationGracefullyPausedTime = "krateo.io/gracefully-paused-time"

	// AnnotationKeyOriginVersion records the served version this composition was FIRST
	// reconciled at, in the CR's own version form ("v1-8-70").
	//
	// It exists because nothing else on the object survives a chart-version bump to say where
	// the object came from (#168). The apiVersion is whatever the CRD serves NOW — core-provider
	// re-points the composition CRD to a single served version, so a CR written at v1-8-70 reads
	// back as v1-8-71 after the bump — and krateo.io/composition-version is deliberately
	// re-stamped on every write by the version MutatingAdmissionPolicy, which is what makes it
	// useless as an origin.
	//
	// Written ONCE and never updated (see SetOriginVersion): its whole value is that it does not
	// move when everything else does. An annotation rather than a label because nothing selects
	// on it and it must not widen any informer's selector.
	AnnotationKeyOriginVersion = "krateo.io/composition-origin-version"

	// AnnotationKeyHelmReleaseName is helm's own ownership annotation, present on any object helm
	// created or adopted. On a composition it means the composition was rendered as a child of
	// ANOTHER release — the umbrella or a parent composition — rather than created directly.
	AnnotationKeyHelmReleaseName = "meta.helm.sh/release-name"

	// ParentCompositionNameLabel names the composition that RENDERED this one, stamped by the helm
	// post-renderer on every child manifest (plumbing/helm/utils, LabelsPostRender.Run). Its
	// presence means this object has a parent composition; its absence means nothing renders it.
	//
	// Distinct from CompositionDefinitionNameLabel, which names the owning CompositionDefinition
	// and is present on EVERY composition. This one is about having a parent, not a definition.
	ParentCompositionNameLabel = "krateo.io/composition-name"
)

// IsParentRendered reports whether this composition is rendered by something else — a parent helm
// release (the umbrella) or a parent composition — as opposed to standing alone.
//
// It matters at teardown (#199). A parent-rendered composition is DELETED AND RECREATED when its
// chart version is bumped: the parent re-renders its children at the new GVK, so helm removes the
// old-GVK object and creates a new one. That delete is a handover, not a teardown. A standalone
// composition has nothing to re-render it, so a chart bump re-points its CRD underneath the object
// it already has (verified on krateo-057: the Installer composition created 2026-08-24, generation
// 374, same uid across many versions). It is therefore never deleted by a bump, and every delete it
// does receive — a user removing it, or core-provider tearing down its CompositionDefinition —
// genuinely wants the release uninstalled.
//
// Two independent signals, and EITHER one being present means parent-rendered. Measured on all 83
// compositions of krateo-057 they agree exactly (59 parent-rendered, 24 standalone, empty symmetric
// difference, confirmed in both directions by a second session).
//
// The disjunction is LOAD-BEARING, not belt-and-braces, and each half covers a real shape:
//
//   - helm annotation present, label absent: a plain Helm chart that itself declares a Composition
//     CR, installed as a component rather than rendered by a parent composition. Helm stamps
//     release-name on everything it renders; nothing stamps the parent-composition label, because
//     there is no parent composition. True is CORRECT here — that chart does re-render it on a
//     bump. An AND would call this standalone and uninstall it mid-bump, which is #168's
//     destruction recreated somewhere new.
//
//   - label present, helm annotation absent: a composition child that was hand-applied, re-applied
//     from a dumped manifest, or restored from a backup that dropped Helm's ownership metadata.
//     The label lives in the object's own metadata and survives; the annotation does not. True is
//     WRONG here — nothing will recreate it, so its delete really is a teardown — but it is wrong
//     in the chosen direction: the release is ORPHANED rather than DESTROYED. An orphan can be
//     adopted or cleaned up by hand; a destroyed release and its children cannot. Recorded as a
//     deliberate trade rather than left to be discovered as a gap, and note that hand-applying a
//     chart's CRs is a known way to strip exactly that annotation.
//
// One precision note. This reads ParentCompositionNameLabel as a LABEL only, which is where the
// post-renderer writes it. A census that accepts it as a label OR an annotation selects the same
// objects today (nothing carries it as an annotation), so the two agree — but they would diverge if
// anything ever wrote it as an annotation, and the symptom would be a surprise uninstall. The
// measured 24 should therefore not be treated as re-derivable from a looser check.
func IsParentRendered(o metav1.Object) bool {
	if o.GetAnnotations()[AnnotationKeyHelmReleaseName] != "" {
		return true
	}
	return o.GetLabels()[ParentCompositionNameLabel] != ""
}

// GetOriginVersion returns the served version this composition was first reconciled at, or ""
// when the object predates the stamp.
func GetOriginVersion(o metav1.Object) string {
	return o.GetAnnotations()[AnnotationKeyOriginVersion]
}

// SetOriginVersion stamps version as this composition's origin, and is a NO-OP when the stamp is
// already present. Write-once is the entire point: a value that can be rewritten tells you only
// what the CRD serves today, which is the bug it exists to fix. Mirrors SetReleaseName.
//
// It reports whether it changed the object, so the caller can skip a needless write.
func SetOriginVersion(o metav1.Object, version string) bool {
	if version == "" {
		return false
	}
	ann := o.GetAnnotations()
	if _, ok := ann[AnnotationKeyOriginVersion]; ok {
		return false
	}
	if ann == nil {
		ann = make(map[string]string)
	}
	ann[AnnotationKeyOriginVersion] = version
	o.SetAnnotations(ann)
	return true
}

// CalculateReleaseName generates a release name for the Helm chart based on the name and UID of the resource.
// If safeReleaseName is false, it will not append a random suffix to the release name, which can lead to collisions but is necessary for certain complex charts that have issues with long release names.
func CalculateReleaseName(o runtime.Object, safeReleaseName bool) string {
	obj := o.(metav1.Object)
	if safeReleaseName {
		uid := obj.GetUID()
		if uid == "" {
			// Generate random string if UID is not set
			return fmt.Sprintf("%s-%s", obj.GetName(), rand.SafeEncodeString(rand.String(8)))
		}
		hashstr := rand.SafeEncodeString(string(obj.GetUID())[:8])
		return fmt.Sprintf("%s-%s", obj.GetName(), hashstr)
	}
	return obj.GetName()
}

func GetReleaseName(o metav1.Object) string {
	return o.GetLabels()[ReleaseNameLabel]
}

// Set the release name as a label on the Composition resource.
// Release name will be "name" if the annotation has not been already populated.
func SetReleaseName(o metav1.Object, name string) {
	mglabels := o.GetLabels()
	if mglabels == nil {
		mglabels = make(map[string]string)
	}

	if _, ok := mglabels[ReleaseNameLabel]; !ok {
		mglabels[ReleaseNameLabel] = name
	}
	o.SetLabels(mglabels)
}

type CompositionDefinitionInfo struct {
	Namespace string
	Name      string
	GVR       schema.GroupVersionResource
}

// SetCompositionDefinitionLabels sets the labels for the Composition Definition on the given object.
// It sets the labels for the composition definition name, namespace, group, version, and resource.
// If the labels already exist, they will be updated.
// If the labels do not exist, they will be created.
func SetCompositionDefinitionLabels(o metav1.Object, cdInfo CompositionDefinitionInfo) {
	labels := o.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}

	labels[CompositionDefinitionNameLabel] = cdInfo.Name
	labels[CompositionDefinitionNamespaceLabel] = cdInfo.Namespace
	labels[CompositionDefinitionGroupLabel] = cdInfo.GVR.Group
	labels[CompositionDefinitionVersionLabel] = cdInfo.GVR.Version
	labels[CompositionDefinitionResourceLabel] = cdInfo.GVR.Resource

	o.SetLabels(labels)
}

// IsGracefullyPaused returns true if the object has the AnnotationKeyGracefullyPaused
// annotation set to `true`.
func IsGracefullyPaused(o metav1.Object) bool {
	return o.GetAnnotations()[AnnotationKeyReconciliationGracefullyPaused] == "true"
}

func SetGracefullyPausedTime(o metav1.Object, t time.Time) {
	meta.AddAnnotations(o, map[string]string{AnnotationKeyReconciliationGracefullyPausedTime: t.Format(time.RFC3339)})
}

func GetGracefullyPausedTime(o metav1.Object) (time.Time, bool) {
	t, ok := o.GetAnnotations()[AnnotationKeyReconciliationGracefullyPausedTime]
	if !ok {
		return time.Time{}, false
	}
	pausedTime, err := time.Parse(time.RFC3339, t)
	if err != nil {
		return time.Time{}, false
	}
	return pausedTime, true
}
