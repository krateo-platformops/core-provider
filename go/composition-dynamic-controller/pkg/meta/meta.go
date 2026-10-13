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

	// AnnotationKeyOriginVersion records the apiVersion version the composition was CREATED at
	// (e.g. "v1-8-70"), stamped once and never rewritten.
	//
	// Nothing else on the object survives a CRD re-point. The apiVersion a read returns is whatever
	// the CRD serves NOW, not what the object was written as, and CompositionVersionLabel is
	// re-stamped on a bump. A composition created at v1-8-70 therefore reads back as v1-8-71 the
	// moment core-provider re-points the CRD — which is exactly when something needs to tell the two
	// apart (core-provider#168).
	AnnotationKeyOriginVersion = "krateo.io/origin-version"
)

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

// SetOriginVersion stamps the version the object was created at, ONCE. A second call is a no-op:
// the value's only use is to survive the CRD re-point that rewrites every other version on the
// object, so a function that could overwrite it would have no more value than the apiVersion it
// exists to replace. Callers may invoke it on every reconcile; only the first one writes.
func SetOriginVersion(o metav1.Object, version string) {
	if version == "" {
		return
	}
	if _, ok := o.GetAnnotations()[AnnotationKeyOriginVersion]; ok {
		return
	}
	meta.AddAnnotations(o, map[string]string{AnnotationKeyOriginVersion: version})
}

// GetOriginVersion returns the stamped origin version and whether it was present. Absent means the
// object predates the stamp, not that it has no origin — callers fall back to the delivered
// apiVersion, which is what they used before it existed.
func GetOriginVersion(o metav1.Object) (string, bool) {
	v, ok := o.GetAnnotations()[AnnotationKeyOriginVersion]
	if !ok || v == "" {
		return "", false
	}
	return v, true
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
